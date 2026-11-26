package ui

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// testStore is a fresh database holding notes (hdr → summary).
func testStore(t *testing.T, notes map[string]agent.Summary) *store.Store {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for h, sum := range notes {
		b, _ := json.Marshal(sum)
		if err := st.PutNote(h, b, sum.Category, sum.Priority); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestNoteTag(t *testing.T) {
	for in, want := range map[store.Meta]string{
		{Category: "reply-needed", Priority: "high"}: "!reply ",
		{Category: "fyi", Priority: "high"}:          "! ",
		{Category: "fyi", Priority: "normal"}:        "",
		{Category: "newsletter", Priority: "low"}:    "news ",
		{Category: "receipt"}:                        "rcpt ",
		{}:                                           "",
	} {
		if got := ansi.Strip(noteTag(in)); got != want {
			t.Errorf("%+v: %q, want %q", in, got, want)
		}
	}
}

func TestLookupStates(t *testing.T) {
	st := testStore(t, map[string]agent.Summary{"done": {Summary: "x"}})
	m := Model{st: st}
	st.Enqueue("q", 1, 0)
	st.Enqueue("f", 2, 0)
	st.Claim("w") // both running
	st.Claim("w")
	st.Finish("f", errors.New("boom"))
	st.Enqueue("q2", 3, 0)
	for hdr, want := range map[string]sumState{"done": sumDone, "f": sumFailed, "q2": sumQueued, "q": sumRunning, "none": sumNone, "": sumNone} {
		if got := m.lookup(hdr).state; got != want {
			t.Errorf("%q: state %v, want %v", hdr, got, want)
		}
	}
	if e := m.lookup("f").errText; e != "boom" {
		t.Errorf("failed job error %q", e)
	}
	if got := (Model{}).lookup("done").state; got != sumNone {
		t.Error("no store must mean no notes")
	}
}

func TestSidebarFitsHeightAndWidth(t *testing.T) {
	m := Model{ag: &agent.Agent{URL: "http://x"}, st: testStore(t, map[string]agent.Summary{"h": {Summary: strings.Repeat("ord ", 40), Actions: []agent.Action{{Text: "Gjør noe viktig i dag", Link: 2}, {Text: "Svar Ola"}},
		Links: []agent.LinkInfo{{N: 1, Title: "Ordrestatus"}, {N: 3, Title: "Svindel", Suspicious: true}}}})}
	full := tb.FullMessage{}
	full.HeaderMessageID = "h"
	full.Parts.Attachments = []tb.Attachment{{Name: "faktura.pdf.exe", ContentType: "application/pdf", Size: 1000}, {Name: "bilde.png", ContentType: "image/png", Size: 5000}}
	m.full = &full
	m.loadNote()
	m.st = nil // rendering must use the cached note, not the store
	for i := 0; i < 40; i++ {
		m.links = append(m.links, link{url: "https://x.no/" + strings.Repeat("a", 80), label: ""})
	}
	for _, h := range []int{8, 20, 40} {
		lines, _ := m.sidebarLines(32, h)
		if len(lines) > h {
			t.Fatalf("h=%d: %d lines", h, len(lines))
		}
		joined := strings.Join(lines, "\n")
		for _, l := range lines {
			if ansi.StringWidth(l) > 32 {
				t.Fatalf("h=%d: line too wide (%d): %q", h, ansi.StringWidth(l), l)
			}
		}
		plain := ansi.Strip(joined)
		if h >= 20 && !strings.Contains(plain, "[2]") || strings.Contains(plain, "Svindel") || strings.Contains(plain, "[3]") {
			t.Fatalf("h=%d: action link or suspicious link wrong:\n%s", h, plain)
		}
		if h >= 40 && !strings.Contains(plain, "[1] Ordrestatus") {
			t.Fatalf("titled link missing:\n%s", plain)
		}
		if !strings.Contains(plain, "⚠ faktura.pdf.exe") { // attachments survive trimming
			t.Fatalf("risky attachment not flagged:\n%s", ansi.Strip(joined))
		}
	}
}

func TestNoteCache(t *testing.T) {
	st := testStore(t, nil)
	m := Model{st: st}
	full := tb.FullMessage{}
	full.HeaderMessageID = "h"
	m.full = &full
	m.loadNote()
	if m.loadNote() || m.noteFor("h").state != sumNone {
		t.Fatal("no note: a reload should change nothing")
	}
	st.Enqueue("h", 1, 10)
	if m.noteFor("h").state != sumNone {
		t.Fatal("cache read the store")
	}
	if !m.loadNote() || m.noteFor("h").state != sumQueued {
		t.Fatalf("reload missed the queued job: %+v", m.note)
	}
	if m.noteFor("other").state != sumNone {
		t.Fatal("another message got the open message's note")
	}
}

func TestSidebarWithAIOff(t *testing.T) {
	m := Model{ag: &agent.Agent{URL: "off"}}
	full := tb.FullMessage{}
	full.HeaderMessageID = "h"
	m.full = &full
	m.links = []link{{url: "https://shop.no/o/1", label: "Se ordren"}}
	lines, _ := m.sidebarLines(32, 20)
	plain := ansi.Strip(strings.Join(lines, "\n"))
	if strings.Contains(plain, "Summary") || !strings.Contains(plain, "[1] Se ordren") {
		t.Fatalf("AI off sidebar:\n%s", plain)
	}
}
