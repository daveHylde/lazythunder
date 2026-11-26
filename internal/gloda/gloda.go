// Package gloda searches Thunderbird's global message index
// (<profile>/global-messages-db.sqlite) directly and read-only.
package gloda

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"
)

// Hit is one gloda message matching a search.
type Hit struct {
	HeaderMessageID string // without angle brackets, as gloda stores it
	FolderPath      string // URL-decoded path of the folder URI, e.g. "/INBOX"
	FolderURI       string // e.g. "imap://me%40x.no@imap.x.no/INBOX": names the account too
	Date            time.Time
}

// Opts bounds a search. Zero Since/Until are unbounded; Limit 0 means 500.
type Opts struct {
	Since, Until time.Time
	Limit        int
}

const foldFn = "gloda_fold_contains"

func init() {
	// gloda_fold_contains(text, needle): Unicode case-insensitive substring test;
	// needle must already be lower-cased. SQLite's LIKE only folds ASCII.
	sqlite.MustRegisterDeterministicScalarFunction(foldFn, 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		var s string
		switch v := args[0].(type) {
		case string:
			s = v
		case []byte:
			s = string(v)
		default:
			return false, nil
		}
		n, _ := args[1].(string)
		return strings.Contains(strings.ToLower(s), n), nil
	})
}

var columns = []string{"t.c0body", "t.c1subject", "t.c2attachmentNames", "t.c3author", "t.c4recipients"}

// Search returns messages, newest first, where every whitespace-separated word
// of query occurs (case-insensitively) in the body, subject, attachment names,
// author or recipients. Thunderbird may hold its lock: the file is opened
// immutable, so a snapshot caught mid-write is retried once.
func Search(ctx context.Context, dbPath, query string, o Opts) ([]Hit, error) {
	hits, err := search(ctx, dbPath, query, o)
	if err != nil && retryable(err) && ctx.Err() == nil {
		hits, err = search(ctx, dbPath, query, o)
	}
	return hits, err
}

// ConvMsg is one message of a gloda conversation.
type ConvMsg struct {
	Hit
	Author, Subject string
}

// Conversation returns the messages of the conversation (Thunderbird's own
// threading) that headerMessageID belongs to, oldest first, one per
// Message-ID (its inbox copy when it has one). Nil when gloda has not indexed
// that message (yet).
func Conversation(ctx context.Context, dbPath, headerMessageID string) ([]ConvMsg, error) {
	ms, err := conversation(ctx, dbPath, headerMessageID)
	if err != nil && retryable(err) && ctx.Err() == nil {
		ms, err = conversation(ctx, dbPath, headerMessageID)
	}
	return ms, err
}

func conversation(ctx context.Context, dbPath, hdr string) ([]ConvMsg, error) {
	db, err := open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// shortcut: 200 messages per conversation, oldest dropped first; raise if huge threads matter
	rows, err := db.QueryContext(ctx, `SELECT * FROM (
		SELECT m.headerMessageID, f.folderURI, m.date, c.c3author, c.c1subject
		FROM messages m
		JOIN folderLocations f ON f.id = m.folderID
		LEFT JOIN messagesText_content c ON c.docid = m.id
		WHERE m.deleted = 0 AND m.conversationID IN (SELECT conversationID FROM messages WHERE headerMessageID = ? AND deleted = 0)
		ORDER BY m.date DESC LIMIT 400) ORDER BY date`, strings.Trim(hdr, "<>"))
	if err != nil {
		return nil, fmt.Errorf("gloda conversation: %w", err)
	}
	defer rows.Close()
	var out []ConvMsg
	seen := map[string]int{} // index in out: Gmail keeps copies in several folders
	for rows.Next() {
		var id, uri, author, subject sql.NullString
		var date sql.NullInt64
		if err := rows.Scan(&id, &uri, &date, &author, &subject); err != nil {
			return nil, fmt.Errorf("gloda conversation: %w", err)
		}
		m := ConvMsg{Hit{id.String, folderPath(uri.String), uri.String, time.UnixMicro(date.Int64)}, author.String, subject.String}
		if i, dup := seen[id.String]; dup {
			if !out[i].InInbox() && m.InInbox() {
				out[i] = m
			}
			continue
		}
		seen[id.String] = len(out)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gloda conversation: %w", err)
	}
	return out[max(0, len(out)-200):], nil
}

// InInbox reports whether the message is in an inbox (IMAP INBOX or a local Inbox).
func (h Hit) InInbox() bool { return strings.EqualFold(h.FolderPath, "/INBOX") }

// open opens the database read-only. Per call: an immutable connection never
// revalidates its page cache, so a pooled one would mix stale pages with
// Thunderbird's later writes.
func open(dbPath string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func retryable(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case 5, 6, 11, 26: // BUSY, LOCKED, CORRUPT, NOTADB
			return true
		}
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "malformed") || strings.Contains(m, "corrupt") || strings.Contains(m, "locked") || strings.Contains(m, "busy")
}

func search(ctx context.Context, dbPath, query string, o Opts) ([]Hit, error) {
	words := strings.Fields(query)
	if len(words) == 0 {
		return nil, nil
	}
	db, err := open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	q, args := buildQuery(words, o)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gloda search: %w", err)
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var id, uri sql.NullString
		var date sql.NullInt64
		if err := rows.Scan(&id, &uri, &date); err != nil {
			return nil, fmt.Errorf("gloda search: %w", err)
		}
		hits = append(hits, Hit{
			HeaderMessageID: id.String,
			FolderPath:      folderPath(uri.String),
			FolderURI:       uri.String,
			Date:            time.UnixMicro(date.Int64),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gloda search: %w", err)
	}
	return hits, nil
}

func buildQuery(words []string, o Opts) (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString(`SELECT m.headerMessageID, f.folderURI, m.date
FROM messages m
JOIN messagesText_content t ON t.docid = m.id
LEFT JOIN folderLocations f ON f.id = m.folderID
WHERE m.deleted = 0`)
	if !o.Since.IsZero() {
		b.WriteString(" AND m.date >= ?")
		args = append(args, o.Since.UnixMicro())
	}
	if !o.Until.IsZero() {
		b.WriteString(" AND m.date < ?")
		args = append(args, o.Until.UnixMicro())
	}
	for _, w := range words {
		pat, ascii := likePattern(w)
		b.WriteString(" AND (")
		for i, c := range columns {
			if i > 0 {
				b.WriteString(" OR ")
			}
			if ascii {
				fmt.Fprintf(&b, `%s LIKE ? ESCAPE '\'`, c)
				args = append(args, pat)
			} else {
				// LIKE with '_' per non-ASCII rune is a cheap prefilter; the Go
				// function does the exact Unicode fold on the few candidates.
				fmt.Fprintf(&b, `(%s LIKE ? ESCAPE '\' AND %s(%s, ?))`, c, foldFn, c)
				args = append(args, pat, strings.ToLower(w))
			}
		}
		b.WriteString(")")
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 500
	}
	b.WriteString(" ORDER BY m.date DESC LIMIT ?")
	args = append(args, limit)
	return b.String(), args
}

// likePattern turns a word into a %...% LIKE pattern with wildcards escaped
// and each non-ASCII rune replaced by '_'. ascii reports whether none was.
func likePattern(w string) (pat string, ascii bool) {
	var b strings.Builder
	b.WriteByte('%')
	ascii = true
	for _, r := range w {
		switch {
		case r >= utf8.RuneSelf:
			b.WriteByte('_')
			ascii = false
		case r == '%' || r == '_' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('%')
	return b.String(), ascii
}

// folderPath returns the decoded path of a folder URI like
// "imap://user%40host@server/[Gmail]/All%20Mail" -> "/[Gmail]/All Mail".
func folderPath(uri string) string {
	if i := strings.Index(uri, "://"); i >= 0 {
		uri = uri[i+3:]
	}
	i := strings.IndexByte(uri, '/')
	if i < 0 {
		return ""
	}
	p := uri[i:]
	if d, err := url.PathUnescape(p); err == nil {
		return d
	}
	return p
}
