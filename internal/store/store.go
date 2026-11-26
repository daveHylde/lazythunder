// Package store persists lazythunder state (notes, job queue, reply status,
// unsubscribe methods, new-mail baseline, key-values) in one SQLite file
// shared by the TUI and the background daemon.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// now is the clock; tests replace it.
var now = time.Now

// staleAfter is how long a running job may go without finishing before
// another worker may reclaim it (its worker probably died).
const staleAfter = 10 * time.Minute

// A failed job is retried after retryAfter, until it has failed maxAttempts
// times; most failures (model or Thunderbird briefly away) are transient.
const (
	retryAfter  = 15 * time.Minute
	maxAttempts = 3
)

// Store is a handle on the database. Safe for concurrent use.
type Store struct {
	db *sql.DB
	// dv is pinned because PRAGMA data_version is per connection.
	dv *sql.Conn
}

// Meta is the short note metadata shown in message lists.
type Meta struct {
	Category, Priority string
}

// Reply records that a message was answered ('r') or forwarded ('f').
type Reply struct {
	Kind   byte
	SentID int
	Date   time.Time
	To     string
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS notes (
		hdr TEXT PRIMARY KEY,
		data BLOB NOT NULL,
		category TEXT NOT NULL DEFAULT '',
		priority TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS jobs (
		hdr TEXT PRIMARY KEY,
		msg_id INTEGER NOT NULL,
		prio INTEGER NOT NULL,
		state TEXT NOT NULL,
		worker TEXT NOT NULL DEFAULT '',
		err TEXT NOT NULL DEFAULT '',
		updated INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS jobs_pick ON jobs (state, prio DESC, updated);
	CREATE TABLE IF NOT EXISTS replies (
		hdr TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		sent_id INTEGER NOT NULL,
		date TEXT NOT NULL,
		to_addr TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS unsubs (hdr TEXT PRIMARY KEY, data BLOB NOT NULL);
	CREATE TABLE IF NOT EXISTS polled (folder TEXT PRIMARY KEY);
	CREATE TABLE IF NOT EXISTS seen (hdr TEXT PRIMARY KEY);
	CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL);`,
	`ALTER TABLE jobs ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;`,
}

// DefaultPath is $XDG_DATA_HOME/lazythunder/lazythunder.db, falling back to
// ~/.local/share.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "lazythunder", "lazythunder.db"), nil
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	if err := createFile(path); err != nil {
		return nil, err
	}
	q := url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)"},
		// Immediate write transactions take the lock up front, so busy_timeout
		// applies instead of failing on a lock upgrade.
		"_txlock": {"immediate"},
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// createFile creates the db file and its dir with private permissions;
// SQLite gives the -wal/-shm files the same mode.
func createFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func (s *Store) init() error {
	if err := s.migrate(); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	s.dv = conn
	return nil
}

// migrate applies pending migrations inside one write transaction, so
// concurrent openers serialize and each step runs once.
func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for ; v < len(migrations); v++ {
		if _, err := tx.Exec(migrations[v]); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close releases the database.
func (s *Store) Close() error {
	if s.dv != nil {
		s.dv.Close()
	}
	return s.db.Close()
}

// DataVersion changes whenever another connection (or process) commits.
func (s *Store) DataVersion() (int64, error) {
	var v int64
	err := s.dv.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&v)
	return v, err
}

// hdrList encodes hdrs for `IN (SELECT value FROM json_each(?))`, which
// avoids SQLite's bound-variable limit on large lists.
func hdrList(hdrs []string) string {
	b, _ := json.Marshal(hdrs)
	return string(b)
}

// PutNote stores or replaces the note for hdr.
func (s *Store) PutNote(hdr string, data []byte, category, priority string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO notes (hdr, data, category, priority)
		VALUES (?, ?, ?, ?)`, hdr, data, category, priority)
	return err
}

// Note returns the note for hdr; ok is false if there is none.
func (s *Store) Note(hdr string) (data []byte, category, priority string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT data, category, priority FROM notes WHERE hdr = ?`, hdr).
		Scan(&data, &category, &priority)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", "", false, nil
	}
	return data, category, priority, err == nil, err
}

// NoteMeta returns category and priority for those hdrs that have a note.
func (s *Store) NoteMeta(hdrs []string) (map[string]Meta, error) {
	rows, err := s.db.Query(`SELECT hdr, category, priority FROM notes
		WHERE hdr IN (SELECT value FROM json_each(?))`, hdrList(hdrs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Meta{}
	for rows.Next() {
		var h string
		var m Meta
		if err := rows.Scan(&h, &m.Category, &m.Priority); err != nil {
			return nil, err
		}
		out[h] = m
	}
	return out, rows.Err()
}

// Enqueue queues hdr for processing. It does nothing if hdr already has a
// note or its job is running; a failed job is requeued, and a queued job
// keeps the higher of the two priorities. Either way its attempts start over.
func (s *Store) Enqueue(hdr string, msgID, prio int) error {
	return s.enqueue(hdr, msgID, prio, false)
}

// Requeue is Enqueue even when a note exists: the stored note is outdated.
func (s *Store) Requeue(hdr string, msgID, prio int) error {
	return s.enqueue(hdr, msgID, prio, true)
}

func (s *Store) enqueue(hdr string, msgID, prio int, redo bool) error {
	_, err := s.db.Exec(`INSERT INTO jobs (hdr, msg_id, prio, state, updated)
		SELECT ?1, ?2, ?3, 'queued', ?4
		WHERE ?5 OR NOT EXISTS (SELECT 1 FROM notes WHERE hdr = ?1)
		ON CONFLICT (hdr) DO UPDATE SET
			msg_id = excluded.msg_id,
			prio = max(prio, excluded.prio),
			updated = CASE WHEN state = 'failed' THEN excluded.updated ELSE updated END,
			err = '',
			attempts = 0,
			state = 'queued'
		WHERE state != 'running'`, hdr, msgID, prio, now().UnixNano(), redo)
	return err
}

// Claim atomically takes the best queued job, one whose worker went stale,
// or a failed one due for a retry. The single UPDATE…RETURNING holds the
// write lock for the whole pick, so two processes never get the same job.
func (s *Store) Claim(worker string) (hdr string, msgID int, ok bool, err error) {
	t := now()
	err = s.db.QueryRow(`UPDATE jobs SET state = 'running', worker = ?1, updated = ?2
		WHERE hdr = (SELECT hdr FROM jobs
			WHERE state = 'queued' OR (state = 'running' AND updated < ?3)
				OR (state = 'failed' AND updated < ?4 AND attempts < ?5)
			ORDER BY prio DESC, updated ASC LIMIT 1)
		RETURNING hdr, msg_id`, worker, t.UnixNano(), t.Add(-staleAfter).UnixNano(),
		t.Add(-retryAfter).UnixNano(), maxAttempts).
		Scan(&hdr, &msgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return hdr, msgID, err == nil, err
}

// Finish deletes hdr's job on success or marks it failed with jobErr.
func (s *Store) Finish(hdr string, jobErr error) error {
	if jobErr == nil {
		_, err := s.db.Exec(`DELETE FROM jobs WHERE hdr = ?`, hdr)
		return err
	}
	_, err := s.db.Exec(`UPDATE jobs SET state = 'failed', err = ?, updated = ?, attempts = attempts + 1
		WHERE hdr = ?`,
		jobErr.Error(), now().UnixNano(), hdr)
	return err
}

// JobState returns "", "queued", "running" or "failed", plus the error text
// of a failed job.
func (s *Store) JobState(hdr string) (state, errText string, err error) {
	err = s.db.QueryRow(`SELECT state, err FROM jobs WHERE hdr = ?`, hdr).Scan(&state, &errText)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return state, errText, err
}

// PutReply records that hdr was replied to or forwarded.
func (s *Store) PutReply(hdr string, r Reply) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO replies (hdr, kind, sent_id, date, to_addr)
		VALUES (?, ?, ?, ?, ?)`, hdr, string(r.Kind), r.SentID, r.Date.Format(time.RFC3339Nano), r.To)
	return err
}

// Replies returns the recorded replies for those hdrs that have one.
func (s *Store) Replies(hdrs []string) (map[string]Reply, error) {
	rows, err := s.db.Query(`SELECT hdr, kind, sent_id, date, to_addr FROM replies
		WHERE hdr IN (SELECT value FROM json_each(?))`, hdrList(hdrs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Reply{}
	for rows.Next() {
		var h, kind, date string
		var r Reply
		if err := rows.Scan(&h, &kind, &r.SentID, &date, &r.To); err != nil {
			return nil, err
		}
		if kind != "" {
			r.Kind = kind[0]
		}
		if r.Date, err = time.Parse(time.RFC3339Nano, date); err != nil {
			return nil, err
		}
		out[h] = r
	}
	return out, rows.Err()
}

// PutUnsub stores the unsubscribe methods found for hdr.
func (s *Store) PutUnsub(hdr string, data []byte) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO unsubs (hdr, data) VALUES (?, ?)`, hdr, data)
	return err
}

// Unsub returns the unsubscribe methods stored for hdr.
func (s *Store) Unsub(hdr string) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM unsubs WHERE hdr = ?`, hdr).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return data, err == nil, err
}

// Polled reports whether folder has had its new-mail baseline taken.
func (s *Store) Polled(folder string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM polled WHERE folder = ?`, folder).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SetPolled records that folder's baseline has been taken.
func (s *Store) SetPolled(folder string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO polled (folder) VALUES (?)`, folder)
	return err
}

// Unseen returns those hdrs not marked seen, without marking them.
func (s *Store) Unseen(hdrs []string) ([]string, error) {
	rows, err := s.db.Query(`SELECT value FROM json_each(?)
		WHERE value NOT IN (SELECT hdr FROM seen)`, hdrList(hdrs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// MarkSeen records hdrs as seen and returns those not seen before.
func (s *Store) MarkSeen(hdrs []string) (fresh []string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO seen (hdr) VALUES (?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, h := range hdrs {
		res, err := stmt.Exec(h)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			fresh = append(fresh, h)
		}
	}
	return fresh, tx.Commit()
}

// SetKV stores v under k.
func (s *Store) SetKV(k, v string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO kv (k, v) VALUES (?, ?)`, k, v)
	return err
}

// KV returns the value under k.
func (s *Store) KV(k string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// ImportJSON migrates a legacy JSON file of hdr → note into the notes table,
// keeping existing rows, and deletes the file once the import is committed.
// A missing file is not an error; a corrupt one is moved to path+".bad" so
// it is not retried, and reported. The TUI and the daemon may both import
// at once, so the file vanishing midway is fine.
func (s *Store) ImportJSON(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var notes map[string]json.RawMessage
	if err := json.Unmarshal(b, &notes); err != nil {
		err = fmt.Errorf("store: import %s: %w (moved to .bad)", path, err)
		return errors.Join(err, ignoreGone(os.Rename(path, path+".bad")))
	}
	if err := s.insertNotes(notes); err != nil {
		return err
	}
	return ignoreGone(os.Remove(path))
}

// ignoreGone drops the error of a file another process removed first.
func ignoreGone(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) insertNotes(notes map[string]json.RawMessage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO notes (hdr, data) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for h, data := range notes {
		if _, err := stmt.Exec(h, []byte(data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
