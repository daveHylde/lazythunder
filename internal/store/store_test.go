package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func tempDB(t *testing.T) string {
	return filepath.Join(t.TempDir(), "sub", "test.db")
}

func setClock(t *testing.T, at *time.Time) {
	t.Cleanup(func() { now = time.Now })
	now = func() time.Time { return *at }
}

func state(t *testing.T, s *Store, hdr string) string {
	t.Helper()
	st, _, err := s.JobState(hdr)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestFileModes(t *testing.T) {
	path := tempDB(t)
	open(t, path)
	for p, want := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
}

func TestClaimAcrossHandles(t *testing.T) {
	path := tempDB(t)
	a, b := open(t, path), open(t, path)
	const n = 50
	for i := range n {
		if err := a.Enqueue(fmt.Sprint("h", i), i, i%3); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var got []string
	var wg sync.WaitGroup
	for w, s := range []*Store{a, b, a, b} {
		wg.Go(func() {
			for {
				h, _, ok, err := s.Claim(fmt.Sprint("w", w))
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				got = append(got, h)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	slices.Sort(got)
	if len(got) != n || len(slices.Compact(got)) != n {
		t.Fatalf("claimed %d jobs (%d distinct), want %d distinct", len(got), len(slices.Compact(got)), n)
	}
}

func TestClaimOrderAndStale(t *testing.T) {
	s := open(t, tempDB(t))
	clock := time.Unix(1000, 0)
	setClock(t, &clock)
	s.Enqueue("low", 1, 0)
	clock = clock.Add(time.Second)
	s.Enqueue("high", 2, 5)
	h, id, ok, err := s.Claim("w1")
	if err != nil || !ok || h != "high" || id != 2 {
		t.Fatalf("Claim = %q %d %v %v, want high 2", h, id, ok, err)
	}
	if h, _, _, _ := s.Claim("w1"); h != "low" {
		t.Fatalf("second claim %q, want low", h)
	}
	if _, _, ok, _ := s.Claim("w2"); ok {
		t.Fatal("claimed a fresh running job")
	}
	clock = clock.Add(staleAfter + time.Second)
	h, _, ok, _ = s.Claim("w2")
	if !ok || h != "high" {
		t.Fatalf("stale reclaim = %q %v, want high", h, ok)
	}
}

func TestEnqueueRules(t *testing.T) {
	s := open(t, tempDB(t))
	s.PutNote("noted", []byte(`{}`), "", "")
	s.Enqueue("noted", 1, 0)
	if st := state(t, s, "noted"); st != "" {
		t.Fatalf("job for noted msg: %q", st)
	}

	s.Enqueue("a", 1, 1)
	s.Enqueue("b", 2, 2)
	s.Enqueue("a", 1, 9) // raises a above b
	s.Enqueue("b", 2, 0) // never lowers
	if h, _, _, _ := s.Claim("w"); h != "a" {
		t.Fatalf("claimed %q, want raised a", h)
	}
	s.Enqueue("a", 1, 0)
	if st := state(t, s, "a"); st != "running" {
		t.Fatalf("enqueue touched running job: %q", st)
	}

	s.Finish("a", errors.New("boom"))
	if st, msg, _ := s.JobState("a"); st != "failed" || msg != "boom" {
		t.Fatalf("after failure: %q %q", st, msg)
	}
	s.Enqueue("a", 1, 0)
	if st, msg, _ := s.JobState("a"); st != "queued" || msg != "" {
		t.Fatalf("failed job not requeued: %q %q", st, msg)
	}
	s.Claim("w")
	s.Finish("a", nil)
	if st := state(t, s, "a"); st != "" {
		t.Fatalf("finished job remains: %q", st)
	}
}

func TestMarkSeen(t *testing.T) {
	s := open(t, tempDB(t))
	fresh, err := s.MarkSeen([]string{"a", "b"})
	if err != nil || !slices.Equal(fresh, []string{"a", "b"}) {
		t.Fatalf("first MarkSeen = %v %v", fresh, err)
	}
	fresh, _ = s.MarkSeen([]string{"b", "c", "c"})
	if !slices.Equal(fresh, []string{"c"}) {
		t.Fatalf("second MarkSeen = %v, want [c]", fresh)
	}
}

func TestImportJSON(t *testing.T) {
	s := open(t, tempDB(t))
	s.PutNote("keep", []byte(`"mine"`), "cat", "hi")
	file := filepath.Join(t.TempDir(), "notes.json")
	os.WriteFile(file, []byte(`{"keep":"theirs","new":{"x":1}}`), 0o600)
	if err := s.ImportJSON(file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file not deleted: %v", err)
	}
	if d, c, _, _, _ := s.Note("keep"); string(d) != `"mine"` || c != "cat" {
		t.Fatalf("existing note overwritten: %s %s", d, c)
	}
	if d, c, p, ok, _ := s.Note("new"); !ok || string(d) != `{"x":1}` || c != "" || p != "" {
		t.Fatalf("imported note = %s %q %q %v", d, c, p, ok)
	}
	if err := s.ImportJSON(file); err != nil {
		t.Fatalf("missing file: %v", err)
	}
}

func TestFailedJobRetries(t *testing.T) {
	s := open(t, tempDB(t))
	clock := time.Unix(1000, 0)
	setClock(t, &clock)
	s.Enqueue("a", 1, 0)
	for i := 1; i <= maxAttempts; i++ {
		if h, _, ok, _ := s.Claim("w"); !ok || h != "a" {
			t.Fatalf("attempt %d not claimed: %q %v", i, h, ok)
		}
		s.Finish("a", errors.New("down"))
		if _, _, ok, _ := s.Claim("w"); ok {
			t.Fatalf("attempt %d: retried before the backoff", i)
		}
		clock = clock.Add(retryAfter + time.Second)
	}
	if _, _, ok, _ := s.Claim("w"); ok {
		t.Fatal("retried past maxAttempts")
	}
	s.Enqueue("a", 1, 0) // explicit enqueue starts over
	clock = clock.Add(time.Second)
	s.Claim("w")
	s.Finish("a", errors.New("down"))
	clock = clock.Add(retryAfter + time.Second)
	if h, _, ok, _ := s.Claim("w"); !ok || h != "a" {
		t.Fatal("Enqueue did not reset attempts")
	}
}

func TestMigrateFromV1(t *testing.T) {
	path := tempDB(t)
	s := open(t, path)
	// rebuild a version-1 database: jobs without attempts
	for _, q := range []string{`DROP TABLE jobs`, `PRAGMA user_version = 0`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(migrations[0] + `INSERT INTO jobs (hdr, msg_id, prio, state, updated) VALUES ('old', 1, 0, 'queued', 0);
		PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, path)
	if h, _, ok, err := s.Claim("w"); !ok || h != "old" || err != nil {
		t.Fatalf("v1 job after migration: %q %v %v", h, ok, err)
	}
	if err := s.Finish("old", errors.New("x")); err != nil {
		t.Fatalf("attempts column missing: %v", err)
	}
}

func TestUnseen(t *testing.T) {
	s := open(t, tempDB(t))
	s.MarkSeen([]string{"a"})
	got, err := s.Unseen([]string{"a", "b", "c"})
	if err != nil || !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("Unseen = %v %v", got, err)
	}
	if again, _ := s.Unseen([]string{"b"}); !slices.Equal(again, []string{"b"}) {
		t.Fatal("Unseen marked something")
	}
}

func TestImportCorruptJSON(t *testing.T) {
	s := open(t, tempDB(t))
	file := filepath.Join(t.TempDir(), "notes.json")
	os.WriteFile(file, []byte(`{broken`), 0o600)
	if err := s.ImportJSON(file); err == nil {
		t.Fatal("corrupt file not reported")
	}
	if _, err := os.Stat(file + ".bad"); err != nil {
		t.Fatalf("not moved aside: %v", err)
	}
	if err := s.ImportJSON(file); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if err := s.PutNote("x", []byte(`1`), "", ""); err != nil {
		t.Fatalf("store unusable: %v", err)
	}
}

func TestReopenAndRemigrate(t *testing.T) {
	path := tempDB(t)
	s := open(t, path)
	date := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	s.PutNote("n", []byte(`1`), "work", "high")
	s.PutReply("n", Reply{Kind: 'f', SentID: 7, Date: date, To: "x@y"})
	s.PutUnsub("n", []byte(`[]`))
	s.SetPolled("INBOX")
	s.SetKV("pause", "later")
	s.Close()

	s = open(t, path)
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var v int
	s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != len(migrations) {
		t.Fatalf("user_version %d, want %d", v, len(migrations))
	}
	if m, _ := s.NoteMeta([]string{"n", "missing"}); len(m) != 1 || m["n"] != (Meta{"work", "high"}) {
		t.Fatalf("NoteMeta = %v", m)
	}
	r, _ := s.Replies([]string{"n"})
	if got := r["n"]; got.Kind != 'f' || got.SentID != 7 || !got.Date.Equal(date) || got.To != "x@y" {
		t.Fatalf("Replies = %+v", r)
	}
	if d, ok, _ := s.Unsub("n"); !ok || string(d) != `[]` {
		t.Fatalf("Unsub = %s %v", d, ok)
	}
	if ok, _ := s.Polled("INBOX"); !ok {
		t.Fatal("polled lost")
	}
	if ok, _ := s.Polled("Other"); ok {
		t.Fatal("unknown folder polled")
	}
	if v, ok, _ := s.KV("pause"); !ok || v != "later" {
		t.Fatalf("KV = %q %v", v, ok)
	}
}

func TestDataVersion(t *testing.T) {
	path := tempDB(t)
	a, b := open(t, path), open(t, path)
	before, err := a.DataVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := a.DataVersion(); v != before {
		t.Fatalf("data_version moved without a write: %d -> %d", before, v)
	}
	b.SetKV("heartbeat", "now")
	if after, _ := a.DataVersion(); after == before {
		t.Fatal("data_version unchanged after another handle's write")
	}
}

func TestRequeueRedoesExistingNote(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	if err := s.PutNote("h", []byte(`{}`), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("h", 1, 0); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := s.JobState("h"); st != "" {
		t.Fatalf("Enqueue with a note queued it: %q", st)
	}
	if err := s.Requeue("h", 1, 0); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := s.JobState("h"); st != "queued" {
		t.Fatalf("Requeue state = %q", st)
	}
}
