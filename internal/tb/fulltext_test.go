package tb

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func glodaFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gloda.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMicro()
	for _, q := range []string{
		`CREATE TABLE folderLocations (id INTEGER PRIMARY KEY, folderURI TEXT NOT NULL, dirtyStatus INTEGER NOT NULL, name TEXT NOT NULL, indexingPriority INTEGER NOT NULL)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, folderID INTEGER, messageKey INTEGER, conversationID INTEGER NOT NULL, date INTEGER, headerMessageID TEXT, deleted INTEGER NOT NULL default 0, jsonAttributes TEXT, notability INTEGER NOT NULL default 0)`,
		`CREATE TABLE messagesText_content(docid INTEGER PRIMARY KEY, c0body, c1subject, c2attachmentNames, c3author, c4recipients)`,
		`INSERT INTO folderLocations VALUES (1, 'imap://me%40x.no@imap.x.no/INBOX', 0, 'INBOX', 0)`,
		`INSERT INTO messages VALUES (1, 1, 1, 1, ` + itoa(day) + `, 'a@x', 0, NULL, 0)`,
		`INSERT INTO messages VALUES (2, 1, 2, 2, ` + itoa(day+1e6) + `, 'b@x', 0, NULL, 0)`,
		`INSERT INTO messagesText_content VALUES (1, 'Fakturaen forfaller', 'Faktura', '', 'Fiken', 'me')`,
		`INSERT INTO messagesText_content VALUES (2, 'ny faktura', 'Hei', '', 'Kari', 'me')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestFullTextResolvesAndFilters(t *testing.T) {
	inbox := FolderRef{AccountID: "account1", Path: "/INBOX"}
	allMail := FolderRef{AccountID: "account1", Path: "/[Gmail]/All Mail"}
	c := fakeHub(t, func(op string, params json.RawMessage) (any, *Error) {
		if op == "folders" {
			return []Folder{{ID: "account1://INBOX", AccountID: "account1", Path: "/INBOX"}, {ID: "account1://[Gmail]/All Mail", AccountID: "account1", Path: "/[Gmail]/All Mail"}}, nil
		}
		var p struct {
			Lookups []struct{ Folders []string }
		}
		json.Unmarshal(params, &p)
		if op != "byMessageId" || len(p.Lookups) == 0 || len(p.Lookups[0].Folders) != 1 || p.Lookups[0].Folders[0] != "account1://INBOX" {
			t.Errorf("want byMessageId scoped to the INBOX, got %s %s", op, params)
		}
		return []Message{
			{ID: 10, HeaderMessageID: "a@x", Author: "Fiken", Folder: allMail, Date: time.Unix(1, 0)},
			{ID: 11, HeaderMessageID: "a@x", Author: "Fiken", Folder: inbox, Date: time.Unix(1, 0), Read: true},
			{ID: 20, HeaderMessageID: "b@x", Author: "Kari", Folder: inbox, Date: time.Unix(2, 0)},
		}, nil
	})
	c.Gloda = glodaFixture(t)
	ctx := context.Background()
	p, err := c.Search(ctx, "FAKTURA", SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 2 || p.Messages[0].ID != 20 || p.Messages[1].ID != 11 {
		t.Fatalf("want [20 11] (gloda's folder copy, newest first), got %+v", p.Messages)
	}
	if p, _ = c.Search(ctx, "faktura", SearchOpts{Unread: true}); len(p.Messages) != 1 || p.Messages[0].ID != 20 {
		t.Fatalf("unread filter: %+v", p.Messages)
	}
	if p, _ = c.Search(ctx, "faktura", SearchOpts{From: "fiken"}); len(p.Messages) != 1 || p.Messages[0].ID != 11 {
		t.Fatalf("from filter: %+v", p.Messages)
	}
	if p, _ = c.Search(ctx, "faktura", SearchOpts{Limit: 1}); len(p.Messages) != 1 || !p.HasMore {
		t.Fatalf("limit: %+v", p)
	}
}

func TestParseDate(t *testing.T) {
	now := time.Date(2026, 3, 31, 15, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":                     {},
		"today":                time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
		"yesterday":            time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC),
		"7d":                   now.AddDate(0, 0, -7),
		"2w":                   now.AddDate(0, 0, -14),
		"1m":                   now.AddDate(0, -1, 0),
		"1y":                   now.AddDate(-1, 0, 0),
		"2026-01-02":           time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		"2026-01-02T03:04:05Z": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	} {
		got, err := ParseDate(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseDate(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseDate("soon", now); err == nil {
		t.Error("want error for garbage")
	}
}
