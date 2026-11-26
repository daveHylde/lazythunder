package tb

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEventsRedialAfterHubRestart: the subscription survives Thunderbird/hub
// restarts. Takes ~1s: Events waits a fixed second before redialing.
func TestEventsRedialAfterHubRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sock")
	serveOnce := func(event string) <-chan struct{} {
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer os.Remove(path)
			defer l.Close()
			nc, err := l.Accept()
			if err != nil {
				return
			}
			nc.Write([]byte("junk\n{\"event\":\"" + event + "\",\"data\":{\"folder\":\"\"}}\n"))
			nc.Close() // hub goes away
		}()
		return done
	}
	firstGone := serveOnce("first")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{Socket: path}
	ch, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recv := func(want string) {
		t.Helper()
		select {
		case ev, ok := <-ch:
			if !ok || ev.Name != want || string(ev.Data) != `{"folder":""}` {
				t.Fatalf("got %+v (open %v), want %s", ev, ok, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s event", want)
		}
	}
	recv("first")
	// Restart only after the first listener is gone, so the client must redial.
	<-firstGone
	serveOnce("second")
	recv("second")
	cancel()
	for range ch { // closes once ctx ends
	}
}

// folderHub serves the search op over folders by name, honouring since/until,
// and fails with NOT_FOUND for folders it doesn't have.
func folderHub(t *testing.T, folders map[string][]Message) *Client {
	return fakeHub(t, func(op string, params json.RawMessage) (any, *Error) {
		var p struct{ Folder, Since, Until string }
		json.Unmarshal(params, &p)
		ms, ok := folders[p.Folder]
		if !ok {
			return nil, &Error{Code: "NOT_FOUND", Msg: "no folder " + p.Folder}
		}
		since, _ := time.Parse(time.RFC3339, p.Since)
		until, _ := time.Parse(time.RFC3339, p.Until)
		var hits []Message
		for _, m := range ms {
			if !m.Date.Before(since) && !m.Date.After(until) {
				hits = append(hits, m)
			}
		}
		return Page{Messages: hits}, nil
	})
}

// hourly returns n messages one hour apart ending an hour before fakeNow, ids from id0.
func hourly(id0, n int) (ms []Message) {
	for i := range n {
		ms = append(ms, Message{ID: id0 + i, Date: fakeNow.Add(-time.Duration(i+1) * time.Hour)})
	}
	return
}

func TestRecentMergedHorizon(t *testing.T) {
	old := Message{ID: 999, Date: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := folderHub(t, map[string][]Message{"busy": hourly(1, 60), "quiet": {old}})
	before := fakeNow.Add(time.Hour)
	ms, next, more, err := c.RecentMerged(context.Background(), []string{"busy", "quiet"}, before, false)
	if err != nil {
		t.Fatal(err)
	}
	if !more || next.IsZero() || !next.Before(before) {
		t.Fatalf("more %v next %v", more, next)
	}
	for i, m := range ms {
		if m.ID == old.ID {
			t.Fatal("message older than the busy folder's cursor shown before its turn")
		}
		if m.Date.Before(next) {
			t.Fatalf("message %d older than horizon %v", m.ID, next)
		}
		if i > 0 && m.Date.After(ms[i-1].Date) {
			t.Fatal("not newest first")
		}
	}
	if len(ms) != 60 {
		t.Fatalf("got %d messages, want the busy folder's 60", len(ms))
	}
}

func TestRecentMergedPartialFailure(t *testing.T) {
	c := folderHub(t, map[string][]Message{"a": hourly(1, 3), "b": hourly(10, 2)})
	ms, _, more, err := c.RecentMerged(context.Background(), []string{"a", "gone", "b"}, fakeNow.Add(time.Hour), false)
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("want error naming the failing folder, got %v", err)
	}
	if len(ms) != 5 || more {
		t.Fatalf("want the 5 messages of the healthy folders, got %d (more %v)", len(ms), more)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].Date.After(ms[i-1].Date) {
			t.Fatal("not newest first")
		}
	}

	ms, next, more, err := c.RecentMerged(context.Background(), []string{"x", "y"}, time.Time{}, false)
	if err == nil || ms != nil || more || !next.IsZero() {
		t.Fatalf("all failing: %v %v %v %v", ms, next, more, err)
	}
	if !strings.Contains(err.Error(), "x:") || !strings.Contains(err.Error(), "y:") {
		t.Fatalf("error should name every folder: %v", err)
	}
}

func TestRecentMergedSingleFolder(t *testing.T) {
	c := folderHub(t, map[string][]Message{"a": hourly(1, 3)})
	ms, _, _, err := c.RecentMerged(context.Background(), []string{"gone"}, time.Time{}, false)
	if Code(err) != "NOT_FOUND" || ms != nil {
		t.Fatalf("single folder error should pass through unwrapped: %v", err)
	}
	ms, _, more, err := c.RecentMerged(context.Background(), []string{"a"}, fakeNow.Add(time.Hour), false)
	if err != nil || len(ms) != 3 || more {
		t.Fatalf("%v %v %v", ms, more, err)
	}
}

func TestRecentSendsUnreadAndJunk(t *testing.T) {
	var got struct {
		Unread, IncludeJunk bool
		Limit               int
	}
	c := fakeHub(t, func(_ string, params json.RawMessage) (any, *Error) {
		json.Unmarshal(params, &got)
		return Page{Messages: hourly(1, 50)}, nil
	})
	if _, _, _, err := c.Recent(context.Background(), "f", time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	if !got.Unread || !got.IncludeJunk || got.Limit != 1000 {
		t.Fatalf("search params %+v", got)
	}
}

func TestMergeNewest(t *testing.T) {
	at := func(h int) time.Time { return fakeNow.Add(time.Duration(h) * time.Hour) }
	have := []Message{{ID: 1, Date: at(1), Subject: "old copy"}, {ID: 2, Date: at(3)}}
	more := []Message{{ID: 1, Date: at(1), Subject: "new copy"}, {ID: 3, Date: at(2)}, {ID: 3, Date: at(2)}}
	got := MergeNewest(have, more)
	var ids []int
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if len(ids) != 3 || ids[0] != 2 || ids[1] != 3 || ids[2] != 1 {
		t.Fatalf("ids %v, want [2 3 1]", ids)
	}
	if got[2].Subject != "old copy" {
		t.Fatalf("kept %q, want the message already shown", got[2].Subject)
	}
	if have[0].ID != 1 || have[1].ID != 2 {
		t.Fatal("input reordered")
	}
	if out := MergeNewest(nil, nil); len(out) != 0 {
		t.Fatalf("empty merge: %v", out)
	}
}

func TestParseDateEdges(t *testing.T) {
	oslo := time.FixedZone("CET", 3600)
	now := time.Date(2026, 3, 31, 0, 30, 0, 0, oslo) // just past local midnight
	for in, want := range map[string]time.Time{
		"  TODAY ":                  time.Date(2026, 3, 31, 0, 0, 0, 0, oslo),
		"Yesterday":                 time.Date(2026, 3, 30, 0, 0, 0, 0, oslo),
		"0d":                        now,
		"3D":                        now.AddDate(0, 0, -3),
		"2026-01-02":                time.Date(2026, 1, 2, 0, 0, 0, 0, oslo),
		"2026-01-02t03:04:05z":      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		"2026-01-02T03:04:05+02:00": time.Date(2026, 1, 2, 1, 4, 5, 0, time.UTC),
	} {
		got, err := ParseDate(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseDate(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-1d", "d", "1x", "2026-13-01", "2026/01/02", "x"} {
		if _, err := ParseDate(in, now); err == nil {
			t.Errorf("ParseDate(%q) accepted", in)
		}
	}
}
