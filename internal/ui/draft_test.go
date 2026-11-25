package ui

import (
	"testing"

	"github.com/daveHylde/lazythunder/internal/tb"
)

func TestDraftRoundTrip(t *testing.T) {
	in := tb.Draft{Kind: tb.NewMail, From: "a@x.no", To: "b@y.no", Subject: "Hei: igjen", Body: "Linje 1\n# not a comment in body\n"}
	out, err := parseDraft(tb.Draft{Kind: tb.NewMail}, renderDraft(in, "context line"))
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v want %+v", out, in)
	}
}

func TestDraftReplyIgnoresHeadersAndNeedsBody(t *testing.T) {
	d := tb.Draft{Kind: tb.Reply, ReplyTo: 7}
	if _, err := parseDraft(d, renderDraft(d, "")); err == nil {
		t.Fatal("empty body accepted")
	}
	out, err := parseDraft(d, renderDraft(d, "")+"Takk!\n")
	if err != nil || out.Body != "Takk!\n" || out.ReplyTo != 7 {
		t.Fatalf("got %+v, %v", out, err)
	}
}

func TestDraftMissingTo(t *testing.T) {
	d := tb.Draft{Kind: tb.Forward, ReplyTo: 1, Body: "fyi"}
	if _, err := parseDraft(d, renderDraft(d, "")); err == nil {
		t.Fatal("forward without To accepted")
	}
}
