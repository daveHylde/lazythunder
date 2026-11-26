package bg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
	"github.com/daveHylde/lazythunder/internal/unsub"
)

// fakeMail serves folder contents by folder id; Subject filters by substring.
type fakeMail struct {
	folders map[string][]tb.Message
	full    map[int]tb.FullMessage
	headers mail.Header
	subjErr error // returned by subject (reply lookup) searches
	readErr error
}

func (f *fakeMail) Search(_ context.Context, _ string, o tb.SearchOpts) (tb.Page, error) {
	if o.Subject != "" && f.subjErr != nil {
		return tb.Page{}, f.subjErr
	}
	var out []tb.Message
	for _, m := range f.folders[o.Folder] {
		if strings.Contains(m.Subject, o.Subject) {
			out = append(out, m)
		}
	}
	return tb.Page{Messages: out}, nil
}

func (f *fakeMail) Read(_ context.Context, id int) (tb.FullMessage, error) {
	return f.full[id], f.readErr
}

func (f *fakeMail) Headers(context.Context, int) (mail.Header, error) { return f.headers, nil }

func (f *fakeMail) ByMessageID(_ context.Context, hdr string) ([]tb.Message, error) {
	var out []tb.Message
	for _, m := range f.full {
		if m.HeaderMessageID == hdr {
			out = append(out, m.Message)
		}
	}
	return out, nil
}

type fakeAI struct {
	calls int
	sum   *agent.Summary // nil: a low-priority newsletter
}

func (a *fakeAI) Enabled() bool { return true }

func (a *fakeAI) Summarize(context.Context, string, string, string, []string) (agent.Summary, error) {
	a.calls++
	if a.sum != nil {
		return *a.sum, nil
	}
	return agent.Summary{Summary: "gist", Category: "newsletter", Priority: "low"}, nil
}

func openStore(t *testing.T) *store.Store {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func msg(id int, hdr, subj, author string, date time.Time, to ...string) tb.Message {
	return tb.Message{ID: id, HeaderMessageID: hdr, Subject: subj, Author: author, Date: date, Recipients: to}
}

func TestPollerBaselineThenFresh(t *testing.T) {
	s := openStore(t)
	t0 := time.Now().Add(-time.Hour)
	fm := &fakeMail{folders: map[string][]tb.Message{"in": {msg(1, "old", "Old", "a@x.no", t0)}}}
	folders := []tb.Folder{{ID: "in", AccountID: "a1", Type: "inbox"}, {ID: "junk", AccountID: "a1", Type: "junk"}}
	p := Poller{Store: s, Mail: fm, AI: true}
	if n, err := p.Pass(t.Context(), folders); n != 0 || err != nil {
		t.Fatalf("baseline pass: n=%d err=%v", n, err)
	}
	if st, _, _ := s.JobState("old"); st != "" {
		t.Fatalf("baseline queued the backlog: %q", st)
	}
	fm.folders["in"] = append(fm.folders["in"], msg(2, "new", "New", "b@x.no", t0))
	if n, err := p.Pass(t.Context(), folders); n != 1 || err != nil {
		t.Fatalf("second pass: n=%d err=%v", n, err)
	}
	if st, _, _ := s.JobState("new"); st != "queued" {
		t.Fatalf("fresh mail not queued: %q", st)
	}
	if n, _ := p.Pass(t.Context(), folders); n != 0 {
		t.Fatalf("already seen counted again: %d", n)
	}

	// AI off: counted, not queued
	p.AI = false
	fm.folders["in"] = append(fm.folders["in"], msg(3, "noai", "No AI", "c@x.no", t0))
	if n, _ := p.Pass(t.Context(), folders); n != 1 {
		t.Fatalf("AI off: n=%d", n)
	}
	if st, _, _ := s.JobState("noai"); st != "" {
		t.Fatalf("AI off but queued: %q", st)
	}
}

func TestPollerSentRecordsReply(t *testing.T) {
	s := openStore(t)
	t0 := time.Now().Add(-2 * time.Hour)
	orig := msg(1, "orig", "Møte fredag", "Kari <kari@y.no>", t0)
	fm := &fakeMail{folders: map[string][]tb.Message{"in": {orig}, "sent": {}}}
	folders := []tb.Folder{{ID: "in", AccountID: "a1", Type: "inbox"}, {ID: "sent", AccountID: "a1", Type: "sent"},
		{ID: "in2", AccountID: "a2", Type: "inbox"}}
	p := Poller{Store: s, Mail: fm}
	p.Pass(t.Context(), folders) // baselines
	fm.folders["sent"] = []tb.Message{
		msg(10, "r1", "SV: Møte fredag", "me@x.no", t0.Add(time.Hour), "kari@y.no"),
		msg(11, "f1", "Fwd: Møte fredag", "me@x.no", t0.Add(90*time.Minute), "boss@x.no"),
	}
	if _, err := p.Pass(t.Context(), folders); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.Replies([]string{"orig"})
	if r := rs["orig"]; r.Kind != 'r' || r.SentID != 10 {
		t.Fatalf("reply not recorded (or a later forward replaced it): %+v", r)
	}
	if n, _ := p.Pass(t.Context(), folders); n != 0 {
		t.Fatalf("sent mail counted as new: %d", n)
	}
}

func TestPollerRetriesFailedReplyLookup(t *testing.T) {
	s := openStore(t)
	t0 := time.Now().Add(-2 * time.Hour)
	fm := &fakeMail{folders: map[string][]tb.Message{"in": {msg(1, "orig", "Faktura", "a@y.no", t0)}, "sent": {}}}
	folders := []tb.Folder{{ID: "in", AccountID: "a1", Type: "inbox"}, {ID: "sent", AccountID: "a1", Type: "sent"}}
	p := Poller{Store: s, Mail: fm}
	p.Pass(t.Context(), folders) // baselines
	fm.folders["sent"] = []tb.Message{msg(10, "r1", "Re: Faktura", "me@x.no", t0.Add(time.Hour), "a@y.no")}
	fm.subjErr = errors.New("bridge down")
	if _, err := p.Pass(t.Context(), folders); err == nil {
		t.Fatal("failed lookup not reported")
	}
	if u, _ := s.Unseen([]string{sentKey("r1")}); len(u) != 1 {
		t.Fatal("sent mail marked seen though its lookup failed")
	}
	fm.subjErr = nil
	if _, err := p.Pass(t.Context(), folders); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.Replies([]string{"orig"}); rs["orig"].SentID != 10 {
		t.Fatalf("reply not recorded on the next pass: %+v", rs)
	}
}

func TestPollerRetriesFailedEnqueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw, err := sql.Open("sqlite", path) // a second handle to make inserts into jobs fail
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	t0 := time.Now().Add(-time.Hour)
	fm := &fakeMail{folders: map[string][]tb.Message{"in": {}}}
	folders := []tb.Folder{{ID: "in", AccountID: "a1", Type: "inbox"}}
	p := Poller{Store: s, Mail: fm, AI: true}
	p.Pass(t.Context(), folders) // baseline
	fm.folders["in"] = []tb.Message{msg(2, "new", "New", "b@x.no", t0)}
	if _, err := raw.Exec(`CREATE TRIGGER nojobs BEFORE INSERT ON jobs BEGIN SELECT RAISE(FAIL, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Pass(t.Context(), folders); n != 0 || err == nil {
		t.Fatalf("failed enqueue: n=%d err=%v", n, err)
	}
	raw.Exec(`DROP TRIGGER nojobs`)
	if n, err := p.Pass(t.Context(), folders); n != 1 || err != nil {
		t.Fatalf("retry pass: n=%d err=%v", n, err)
	}
	if st, _, _ := s.JobState("new"); st != "queued" {
		t.Fatalf("not queued on retry: %q", st)
	}
}

func TestWorkerFindsMessageAgainAfterIDsChange(t *testing.T) {
	s := openStore(t)
	other, moved := tb.FullMessage{}, tb.FullMessage{}
	other.ID, other.HeaderMessageID = 7, "someone-else"
	moved.ID, moved.HeaderMessageID = 9, "h"
	ai := &fakeAI{sum: &agent.Summary{Summary: "gist"}}
	w := &Worker{Store: s, Mail: &fakeMail{full: map[int]tb.FullMessage{7: other, 9: moved}}, AI: ai, Name: "t"}
	s.Enqueue("h", 7, 0) // id 7 was "h" before Thunderbird restarted
	if _, err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, _ := s.Note("h"); !ok {
		t.Fatal("no note for h")
	}
	if _, _, _, ok, _ := s.Note("someone-else"); ok {
		t.Fatal("note written for the wrong message")
	}
}

func TestWorkerDropsUnknownCategory(t *testing.T) {
	s := openStore(t)
	full := tb.FullMessage{}
	full.ID, full.HeaderMessageID = 7, "h"
	ai := &fakeAI{sum: &agent.Summary{Summary: "gist", Category: "urgent!!", Priority: "HIGH"}}
	w := &Worker{Store: s, Mail: &fakeMail{full: map[int]tb.FullMessage{7: full}}, AI: ai, Name: "t"}
	s.Enqueue("h", 7, 0)
	if _, err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, cat, prio, ok, _ := s.Note("h"); !ok || cat != "" || prio != "" {
		t.Fatalf("note meta = %v %q %q, want empty", ok, cat, prio)
	}
}

func TestWorkerRequeuesWhenOffline(t *testing.T) {
	s := openStore(t)
	down := errors.New("bridge down")
	var asked error
	w := &Worker{Store: s, Mail: &fakeMail{readErr: down}, AI: &fakeAI{}, Name: "t",
		Offline: func(err error) bool { asked = err; return true }}
	s.Enqueue("h", 7, 0)
	w.Step(t.Context())
	if st := jobState(s, "h"); st != "queued" || asked != down {
		t.Fatalf("offline failure: state %q, Offline got %v", st, asked)
	}
	w.Offline = func(error) bool { return false }
	w.Step(t.Context())
	if st := jobState(s, "h"); st != "failed" {
		t.Fatalf("real failure: state %q", st)
	}
}

func jobState(s *store.Store, hdr string) string {
	st, _, _ := s.JobState(hdr)
	return st
}

func TestDaemonAlive(t *testing.T) {
	now := time.Unix(10_000, 0)
	for v, want := range map[string]bool{
		"10000": true, "9900": true, "9870": false, // beats every 30s; 2 min of silence is death
		"0": false, "": false, "junk": false,
	} {
		if got := DaemonAlive(v, now); got != want {
			t.Errorf("DaemonAlive(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestWorkerStoresNoteAndUnsub(t *testing.T) {
	s := openStore(t)
	full := tb.FullMessage{}
	full.ID, full.HeaderMessageID, full.Subject = 7, "h", "Nyhetsbrev"
	full.Parts.Text = "Hei. Meld deg av: https://news.no/unsubscribe?u=1"
	h := mail.Header{"List-Unsubscribe": {"<https://news.no/u/1>"}, "List-Unsubscribe-Post": {"List-Unsubscribe=One-Click"}}
	ai := &fakeAI{}
	w := &Worker{Store: s, Mail: &fakeMail{full: map[int]tb.FullMessage{7: full}, headers: h}, AI: ai, Name: "t"}

	s.SetKV(KeyPauseUntil, strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	s.Enqueue("h", 7, 0)
	if did, err := w.Step(t.Context()); did || err != nil || ai.calls != 0 {
		t.Fatalf("paused worker claimed: did=%v err=%v", did, err)
	}
	s.SetKV(KeyPauseUntil, "0")
	if did, err := w.Step(t.Context()); !did || err != nil {
		t.Fatalf("step: did=%v err=%v", did, err)
	}
	data, cat, prio, ok, _ := s.Note("h")
	var sum agent.Summary
	json.Unmarshal(data, &sum)
	if !ok || sum.Summary != "gist" || cat != "newsletter" || prio != "low" {
		t.Fatalf("note: ok=%v %q %q %+v", ok, cat, prio, sum)
	}
	if st, _, _ := s.JobState("h"); st != "" {
		t.Fatalf("job not finished: %q", st)
	}
	raw, ok, _ := s.Unsub("h")
	var ms []unsub.Method
	json.Unmarshal(raw, &ms)
	if !ok || len(ms) != 2 || ms[0].Kind != unsub.OneClick || ms[1].URL != "https://news.no/unsubscribe?u=1" {
		t.Fatalf("unsub methods: %s", raw)
	}
	if did, _ := w.Step(t.Context()); did {
		t.Fatal("empty queue claimed something")
	}
}
