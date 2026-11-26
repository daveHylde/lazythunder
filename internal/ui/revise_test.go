package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// reviser is an agent whose model always returns body.
func reviser(t *testing.T, body string) *agent.Agent {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"subject\":\"\",\"body\":\"` + body + `\"}"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return &agent.Agent{URL: srv.URL, HTTP: srv.Client()}
}

func TestReviseDraftAndUndo(t *testing.T) {
	m := loaded(t, nil)
	m.ag = reviser(t, "Kort.")
	m = m.openDraft(tb.Draft{Kind: tb.NewMail, To: "a@x.no", Subject: "Hei", Body: "Lang tekst."})
	m = press(t, m, ":")
	if m.mode != mInput || m.inputFor != "revise" || !strings.Contains(screen(m), "Lang tekst.") {
		t.Fatalf(": should ask how to revise with the draft still showing (mode %v)", m.mode)
	}
	m = press(t, m, "k", "o", "r", "t")
	m, cmd := pressCmd(t, m, "enter")
	if m.mode != mDraft || !m.revising {
		t.Fatalf("after enter: mode %v revising %v", m.mode, m.revising)
	}
	m, _ = update(m, run(t, cmd))
	if m.draft.Body != "Kort." || len(m.draftUndo) != 1 || !strings.Contains(screen(m), "u undo") {
		t.Fatalf("draft %q undo %d", m.draft.Body, len(m.draftUndo))
	}
	m = press(t, m, "u")
	if m.draft.Body != "Lang tekst." || len(m.draftUndo) != 0 {
		t.Fatalf("undo gave %q", m.draft.Body)
	}
}

func TestLateRevisionIgnoredForAnotherDraft(t *testing.T) {
	m := loaded(t, nil)
	m = m.openDraft(tb.Draft{Kind: tb.NewMail, Body: "first"})
	gen := m.draftGen
	m = m.closeDraft()
	m = m.openDraft(tb.Draft{Kind: tb.NewMail, Body: "second"})
	m, _ = update(m, revisedMsg{gen: gen, body: "rewritten first"})
	if m.draft.Body != "second" {
		t.Fatalf("a revision of the closed draft replaced the open one: %q", m.draft.Body)
	}
}

func TestReviseNeedsAI(t *testing.T) {
	m := loaded(t, nil)
	m.ag = &agent.Agent{URL: "off"}
	m = m.openDraft(tb.Draft{Kind: tb.NewMail, Body: "x"})
	m = press(t, m, ":")
	if m.mode != mDraft || !strings.Contains(m.status, "AI is off") {
		t.Fatalf("mode %v status %q", m.mode, m.status)
	}
}
