package bg

import (
	"testing"
	"time"

	"github.com/daveHylde/lazythunder/internal/tb"
)

func TestMatchReplies(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	msgs := []tb.Message{
		{ID: 1, Subject: "Tilbud på kaffe", Author: "Ola <ola@x.no>", Date: t0},
		{ID: 2, Subject: "Møte", Author: "Kari <kari@y.no>", Date: t0},
		{ID: 3, Subject: "Faktura", Author: "a@z.no", Date: t0},
		{ID: 4, Subject: "Hei", Author: "b@z.no", Date: t0},
		{ID: 5, Subject: "API integrasjon?", Author: "K <k@g.com>", Date: t0},
	}
	sent := []tb.Message{
		{ID: 10, Subject: "SV: Tilbud på kaffe", Recipients: []string{"ola@x.no"}, Date: t0.Add(time.Hour)},
		{ID: 11, Subject: "Fwd: Tilbud på kaffe", Recipients: []string{"c@d.no"}, Date: t0.Add(2 * time.Hour)},
		{ID: 12, Subject: "Re: Møte", Recipients: []string{"someone@else.no"}, Date: t0.Add(time.Hour)}, // not to the author
		{ID: 13, Subject: "VS: Faktura", Recipients: []string{"regnskap@z.no"}, Date: t0.Add(time.Hour)},
		{ID: 14, Subject: "Re: Hei", Recipients: []string{"b@z.no"}, Date: t0.Add(-time.Hour)},              // before the message
		{ID: 15, Subject: "API integrasjon?", Recipients: []string{"K <k@g.com>"}, Date: t0.Add(time.Hour)}, // reply without Re:
	}
	got := MatchReplies(msgs, sent)
	if r := got[1]; r.Kind != 'r' || r.SentID != 10 {
		t.Fatalf("1: %+v (reply should win over later forward)", r)
	}
	if _, ok := got[2]; ok {
		t.Fatal("2: reply to someone else counted")
	}
	if r := got[3]; r.Kind != 'f' || r.SentID != 13 {
		t.Fatalf("3: %+v", r)
	}
	if _, ok := got[4]; ok {
		t.Fatal("4: earlier sent message counted")
	}
	if r := got[5]; r.Kind != 'r' || r.SentID != 15 {
		t.Fatalf("5: prefix-less reply missed: %+v", r)
	}
}

func TestSplitSubject(t *testing.T) {
	for in, want := range map[string]string{"Re: SV: Fwd: Hei": "hei", "AW[2]: Test": "test", "Retur booket": "retur booket"} {
		if _, n := SplitSubject(in); n != want {
			t.Errorf("%q -> %q, want %q", in, n, want)
		}
	}
	if k, _ := SplitSubject("Retur booket"); k != 0 {
		t.Error("'Retur' must not count as a Re: prefix")
	}
}
