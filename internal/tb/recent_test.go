package tb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// The test binary doubles as a fake `tb` when LT_FAKE_TB is set: a folder with
// a sparse recent tail and a 2000-message burst ~40 days ago.
func TestMain(m *testing.M) {
	if os.Getenv("LT_FAKE_TB") == "" {
		os.Exit(m.Run())
	}
	var since, until time.Time
	folder := fakeFolder
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--since":
			since, _ = time.Parse(time.RFC3339, args[i+1])
		case "--until":
			until, _ = time.Parse(time.RFC3339, args[i+1])
		case "--folder":
			if args[i+1] == "g" {
				folder = sparseFolder
			}
		}
	}
	var hits []Message
	for _, msg := range folder() {
		if !msg.Date.Before(since) && !msg.Date.After(until) {
			hits = append(hits, msg) // oldest first, like tb search
		}
	}
	more := len(hits) > 1000
	if more {
		hits = hits[:1000]
	}
	data, _ := json.Marshal(map[string]any{"messages": hits, "hasMore": more})
	fmt.Printf(`{"ok":true,"data":%s}`, data)
	os.Exit(0)
}

var fakeNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func fakeFolder() (ms []Message) {
	id := 0
	add := func(t time.Time) { id++; ms = append(ms, Message{ID: id, Date: t}) }
	burst := fakeNow.Add(-40 * 24 * time.Hour)
	for i := 0; i < 2000; i++ {
		add(burst.Add(time.Duration(i) * time.Minute))
	}
	for i := 10; i > 0; i-- {
		add(fakeNow.Add(-time.Duration(i) * 12 * time.Hour))
	}
	return
}

// sparseFolder has one message every 3 days for two years, ids from 100001.
func sparseFolder() (ms []Message) {
	for i := 0; i < 240; i++ {
		ms = append(ms, Message{ID: 100001 + i, Date: fakeNow.Add(-time.Duration(240-i) * 72 * time.Hour)})
	}
	return
}

func TestRecentMergedIsOrderedAndComplete(t *testing.T) {
	t.Setenv("LT_FAKE_TB", "1")
	c := &Client{Bin: os.Args[0], Port: "0"}
	seen := map[int]bool{}
	var order []Message
	before := fakeNow.Add(time.Hour)
	for page := 0; ; page++ {
		if page > 300 {
			t.Fatal("paging did not terminate")
		}
		ms, next, more, err := c.RecentMerged(context.Background(), []string{"f", "g"}, before, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if !seen[m.ID] {
				seen[m.ID] = true
				order = append(order, m)
			}
		}
		if !more {
			break
		}
		before = next
	}
	if want := len(fakeFolder()) + len(sparseFolder()); len(seen) != want {
		t.Fatalf("saw %d of %d messages", len(seen), want)
	}
	for i := 1; i < len(order); i++ {
		if order[i].Date.After(order[i-1].Date) {
			t.Fatalf("out of order at %d: %v after %v", i, order[i].Date, order[i-1].Date)
		}
	}
}

func TestRecentPagesThroughBurstWithoutLoss(t *testing.T) {
	t.Setenv("LT_FAKE_TB", "1")
	c := &Client{Bin: os.Args[0], Port: "0"}
	seen := map[int]bool{}
	before := fakeNow.Add(time.Hour)
	for page := 0; ; page++ {
		if page > 200 {
			t.Fatal("paging did not terminate")
		}
		ms, next, more, err := c.Recent(context.Background(), "f", before, false)
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range ms {
			if i > 0 && m.Date.After(ms[i-1].Date) {
				t.Fatal("page not newest-first")
			}
			seen[m.ID] = true
		}
		if !more {
			break
		}
		before = next
	}
	if len(seen) != len(fakeFolder()) {
		t.Fatalf("saw %d of %d messages", len(seen), len(fakeFolder()))
	}
}
