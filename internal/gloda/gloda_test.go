package gloda

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

type fixMsg struct {
	id, folder                    int
	date                          time.Time
	mid                           string
	deleted                       int
	body, subj, att, author, rcpt string
}

var base = time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "global-messages-db.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE folderLocations (id INTEGER PRIMARY KEY, folderURI TEXT NOT NULL, dirtyStatus INTEGER NOT NULL, name TEXT NOT NULL, indexingPriority INTEGER NOT NULL)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, folderID INTEGER, messageKey INTEGER, conversationID INTEGER NOT NULL, date INTEGER, headerMessageID TEXT, deleted INTEGER NOT NULL default 0, jsonAttributes TEXT, notability INTEGER NOT NULL default 0)`,
		`CREATE TABLE messagesText_content(docid INTEGER PRIMARY KEY, c0body, c1subject, c2attachmentNames, c3author, c4recipients)`,
		`INSERT INTO folderLocations VALUES (1, 'imap://david.hyldgaard%40gmail.com@imap.gmail.com/INBOX', 0, 'INBOX', 0)`,
		`INSERT INTO folderLocations VALUES (2, 'imap://david.hyldgaard%40gmail.com@imap.gmail.com/[Gmail]/All%20Mail', 0, 'All Mail', 0)`,
		`INSERT INTO folderLocations VALUES (3, 'mailbox://nobody@Local%20Folders/Trash', 0, 'Trash', 0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	msgs := []fixMsg{
		{1, 1, base, "m1@x", 0, "Here is your faktura for January", "Invoice", "", "Shop <shop@x>", "david@x"},
		{2, 2, base.Add(24 * time.Hour), "m2@x", 0, "hello", "Møte med ØYSTEIN", "", "Øystein <o@x>", "david@x"},
		{3, 3, base.Add(48 * time.Hour), "m3@x", 0, "100% done, see file_v2", "Status", "report_final.pdf", "a@x", "b@x"},
		{4, 1, base.Add(72 * time.Hour), "m4@x", 1, "faktura deleted", "Deleted", "", "a@x", "b@x"},
		{5, 1, base.Add(96 * time.Hour), "m5@x", 0, "100 percent done", "FAKTURA reminder", "", "a@x", "b@x"},
	}
	for _, m := range msgs {
		if _, err := db.Exec(`INSERT INTO messages (id, folderID, messageKey, conversationID, date, headerMessageID, deleted) VALUES (?,?,?,?,?,?,?)`,
			m.id, m.folder, m.id, m.id, m.date.UnixMicro(), m.mid, m.deleted); err != nil {
			t.Fatal(err)
		}
		var att any = m.att
		if m.att == "" {
			att = nil
		}
		if _, err := db.Exec(`INSERT INTO messagesText_content VALUES (?,?,?,?,?,?)`, m.id, m.body, m.subj, att, m.author, m.rcpt); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func ids(hs []Hit) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.HeaderMessageID)
	}
	return out
}

func TestSearch(t *testing.T) {
	path := fixture(t)
	ctx := context.Background()
	cases := []struct {
		name, q string
		o       Opts
		want    []string
	}{
		{"match ascii case-insensitive, newest first, deleted excluded", "Faktura", Opts{}, []string{"m5@x", "m1@x"}},
		{"words ANDed across columns", "faktura invoice", Opts{}, []string{"m1@x"}},
		{"AND with no common message", "faktura status", Opts{}, nil},
		{"unicode fold lower finds upper", "øystein", Opts{}, []string{"m2@x"}},
		{"unicode fold mixed", "MØTE", Opts{}, []string{"m2@x"}},
		{"non-ascii prefilter does not overmatch", "oystein", Opts{}, nil},
		{"percent escaped", "100%", Opts{}, []string{"m3@x"}},
		{"underscore escaped", "file_v2", Opts{}, []string{"m3@x"}},
		{"underscore not wildcard", "report_", Opts{}, []string{"m3@x"}},
		{"underscore literal only", "fileXv2", Opts{}, nil},
		{"since bound", "faktura", Opts{Since: base.Add(time.Hour)}, []string{"m5@x"}},
		{"until bound exclusive", "faktura", Opts{Until: base.Add(96 * time.Hour)}, []string{"m1@x"}},
		{"limit", "x", Opts{Limit: 2}, []string{"m5@x", "m3@x"}},
		{"empty query", "   ", Opts{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hs, err := Search(ctx, path, c.q, c.o)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(hs); !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestHitFields(t *testing.T) {
	hs, err := Search(context.Background(), fixture(t), "hello", Opts{})
	if err != nil || len(hs) != 1 {
		t.Fatalf("hits %v err %v", hs, err)
	}
	if hs[0].FolderPath != "/[Gmail]/All Mail" || !hs[0].Date.Equal(base.Add(24*time.Hour)) {
		t.Fatalf("bad hit %+v", hs[0])
	}
	for uri, want := range map[string]string{
		"imap://david.hyldgaard%40gmail.com@imap.gmail.com/INBOX": "/INBOX",
		"mailbox://nobody@Local%20Folders/Trash":                  "/Trash",
		"imap://u%40x@imap.gmail.com/[Gmail]/Søppelpost":          "/[Gmail]/Søppelpost",
		"imap://u%40x@h/[Gmail]/All e-post":                       "/[Gmail]/All e-post",
		"imap://u%40x@h/100%zz":                                   "/100%zz",
	} {
		if got := folderPath(uri); got != want {
			t.Errorf("folderPath(%q) = %q, want %q", uri, got, want)
		}
	}
}

// GLODA_DB=~/.thunderbird/<profile>/global-messages-db.sqlite go test -run Real -v ./internal/gloda
func TestRealDB(t *testing.T) {
	path := os.Getenv("GLODA_DB")
	if path == "" {
		t.Skip("GLODA_DB not set")
	}
	for _, q := range []string{"faktura", "faktura januar", "øystein", "Øystein møte", "zzzznotfound"} {
		start := time.Now()
		hs, err := Search(context.Background(), path, q, Opts{})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-16q %4d hits in %v", q, len(hs), time.Since(start))
	}
}

func TestConversation(t *testing.T) {
	path := fixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// m1 (conversation 1) gets a reply in two folders (a Gmail copy) and a deleted one.
	for _, q := range []string{
		`INSERT INTO messages (id, folderID, messageKey, conversationID, date, headerMessageID, deleted) VALUES (10, 1, 10, 1, ` + strconv.FormatInt(base.Add(time.Hour).UnixMicro(), 10) + `, 'r1@x', 0)`,
		`INSERT INTO messages (id, folderID, messageKey, conversationID, date, headerMessageID, deleted) VALUES (11, 2, 11, 1, ` + strconv.FormatInt(base.Add(time.Hour).UnixMicro(), 10) + `, 'r1@x', 0)`,
		`INSERT INTO messages (id, folderID, messageKey, conversationID, date, headerMessageID, deleted) VALUES (12, 1, 12, 1, ` + strconv.FormatInt(base.Add(2*time.Hour).UnixMicro(), 10) + `, 'gone@x', 1)`,
		`INSERT INTO messagesText_content VALUES (10, 'ok', 'Re: Invoice', NULL, 'Kari <k@x>', 'shop@x')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	got, err := Conversation(t.Context(), path, "<r1@x>")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].HeaderMessageID != "m1@x" || got[1].HeaderMessageID != "r1@x" {
		t.Fatalf("want [m1 r1] oldest first, one per Message-ID, deleted left out; got %+v", got)
	}
	if got[1].Author != "Kari <k@x>" || got[1].Subject != "Re: Invoice" || got[1].FolderPath != "/INBOX" {
		t.Fatalf("details %+v", got[1])
	}
	if none, err := Conversation(t.Context(), path, "unknown@x"); err != nil || none != nil {
		t.Fatalf("unindexed message: %v %v", none, err)
	}
}
