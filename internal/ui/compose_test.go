package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// composer is an agent whose model returns revs in order; prompts records
// the user prompt of each request.
func composer(t *testing.T, revs ...agent.Revision) (*agent.Agent, func() []string) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // the default reply guide
	var mu sync.Mutex
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []agent.Msg }
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		prompts = append(prompts, req.Messages[1].Content)
		rev := revs[min(len(prompts), len(revs))-1]
		mu.Unlock()
		rev.Lines = strings.Split(rev.Body, "\n") // the model answers in lines
		content, _ := json.Marshal(rev)
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}}}})
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return &agent.Agent{URL: srv.URL, HTTP: srv.Client()}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
}

// typeText types s into the focused input in one go, like a paste.
func typeText(m Model, s string) Model {
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return m
}

// instruct types an instruction, presses enter and delivers the agent's answer.
func instruct(t *testing.T, m Model, s string) Model {
	t.Helper()
	m, cmd := pressCmd(t, typeText(m, s), "enter")
	if !m.revising || m.compose.Value() != "" || m.draftChat[len(m.draftChat)-1] != "you: "+s {
		t.Fatalf("after enter: revising %v input %q chat %q", m.revising, m.compose.Value(), m.draftChat)
	}
	m, _ = update(m, run(t, cmd))
	return m
}

func TestComposeReplyWritesAndRevises(t *testing.T) {
	m := loaded(t, nil)
	var prompts func() []string
	m.ag, prompts = composer(t,
		agent.Revision{Body: "Hi Alice,\n\nFriday works.", Note: "draft written"},
		agent.Revision{Body: "Hei Alice,\n\nFredag passer.", Note: "translated"})
	full := tb.FullMessage{Message: testInbox()[0]}
	full.Parts.Text = "Does Friday work for you?"
	m.full = &full

	m, cmd := pressCmd(t, m, "r")
	if cmd != nil || m.mode != mDraft || !m.compose.Focused() || m.draft.Kind != tb.Reply || m.version() != 0 {
		t.Fatalf("r: cmd %v mode %d focused %v draft %+v", cmd != nil, m.mode, m.compose.Focused(), m.draft)
	}
	if v := screen(m); !strings.Contains(v, "nothing written yet") || !strings.Contains(v, "esc close") {
		t.Fatalf("empty compose screen:\n%s", v)
	}

	m = instruct(t, m, "yes friday works")
	if m.draft.Body != "Hi Alice,\n\nFriday works." || m.version() != 1 || m.draftChat[1] != "✦ draft written (v1)" {
		t.Fatalf("v1: draft %q chat %q", m.draft.Body, m.draftChat)
	}
	if p := prompts()[0]; !strings.Contains(p, "Does Friday work for you?") || !strings.Contains(p, "nothing written yet") {
		t.Fatalf("v1 prompt lacks the original or the empty draft:\n%s", p)
	}

	m = instruct(t, m, "in Norwegian")
	if m.draft.Body != "Hei Alice,\n\nFredag passer." || m.version() != 2 || m.draftChat[3] != "✦ translated (v2)" {
		t.Fatalf("v2: draft %q chat %q", m.draft.Body, m.draftChat)
	}
	p := prompts()[1]
	for _, want := range []string{"- yes friday works\n", "Friday works.", "Instruction: in Norwegian"} {
		if !strings.Contains(p, want) {
			t.Errorf("v2 prompt lacks %q:\n%s", want, p)
		}
	}
	v := screen(m)
	if !strings.Contains(v, "Fredag passer.") || !strings.Contains(v, "translated (v2)") || !strings.Contains(v, "[2] Reply · v2") {
		t.Fatalf("screen:\n%s", v)
	}
	if strings.Count(v, "Fredag passer.") != 1 {
		t.Fatal("the draft text must not be repeated in the chat")
	}

	m = press(t, m, "u") // typing, not undo
	if m.compose.Value() != "u" || m.version() != 2 {
		t.Fatalf("u while typing: input %q version %d", m.compose.Value(), m.version())
	}
	m = press(t, m, "ctrl+z")
	if m.draft.Body != "Hi Alice,\n\nFriday works." || m.version() != 1 || len(m.draftAsks) != 1 || m.draftChat[4] != "✦ back to v1" {
		t.Fatalf("undo: draft %q asks %q chat %q", m.draft.Body, m.draftAsks, m.draftChat)
	}
}

func TestComposeNewMailTakesRecipients(t *testing.T) {
	m := loaded(t, nil)
	m.ag, _ = composer(t, agent.Revision{To: "bob@x.no", Subject: "Friday", Body: "Hi Bob", Note: "draft written"})
	m = press(t, m, "c")
	m = instruct(t, m, "tell bob@x.no friday works")
	if m.draft.To != "bob@x.no" || m.draft.Subject != "Friday" || !strings.Contains(screen(m), "To: bob@x.no") {
		t.Fatalf("draft %+v", m.draft)
	}
}

func TestComposeEscAsksSaveOrDiscard(t *testing.T) {
	client, h := newTestHub(t)
	m := loaded(t, client)
	m.ag, _ = composer(t)
	m = press(t, m, "c", "esc") // nothing written: closes without asking
	if m.draft != nil || m.mode != mNormal || m.status != "draft discarded" {
		t.Fatalf("empty draft: %+v mode %d", m.draft, m.mode)
	}
	m = openTestDraft(m, tb.Draft{Kind: tb.Reply, ReplyTo: 1, Body: "hi\n"})
	m = press(t, m, "esc")
	if m.mode != mConfirm || !strings.Contains(statusLine(m), "Save to Drafts? y save · n discard · esc keep writing") {
		t.Fatalf("esc: mode %d line %q", m.mode, statusLine(m))
	}
	if m = press(t, m, "esc"); m.mode != mDraft || m.draft == nil {
		t.Fatal("esc on the question should keep writing")
	}
	m = press(t, m, "esc")
	m, cmd := pressCmd(t, m, "y")
	m, cmd = update(m, run(t, cmd)) // saveDraftMsg → save
	if !m.submitting || m.status != "saving draft…" {
		t.Fatalf("y: submitting %v status %q", m.submitting, m.status)
	}
	m, _ = update(m, run(t, cmd))
	wantCalls(t, h, `submit {"body":"hi\n","kind":"reply","replyTo":1,"send":false}`)
	if m.draft != nil || m.status != "saved to Drafts" {
		t.Fatalf("draft %+v status %q", m.draft, m.status)
	}
}

func TestComposeSendAsksFirst(t *testing.T) {
	client, h := newTestHub(t)
	m := loaded(t, client)
	m.ag, _ = composer(t)
	m = openTestDraft(m, tb.Draft{Kind: tb.NewMail, To: "bob@x.no", Subject: "Hi", Body: "hello\n"})
	m = press(t, m, "ctrl+s")
	if m.mode != mConfirm || m.confirm.text != "Send this message now?" || len(h.Calls()) != 0 {
		t.Fatalf("ctrl+s: mode %d", m.mode)
	}
	if m2 := press(t, m, "n"); m2.mode != mDraft || m2.draft == nil {
		t.Fatal("n should return to compose mode")
	}
	m, cmd := pressCmd(t, m, "y")
	for _, step := range subCmds(t, run(t, cmd)) {
		m, _ = update(m, run(t, step))
	}
	wantCalls(t, h, `submit {"body":"hello\n","kind":"new","send":true,"subject":"Hi","to":"bob@x.no"}`)
	if m.status != "sent" || m.draft != nil {
		t.Fatalf("status %q", m.status)
	}
}

func TestLateRevisionIgnoredForAnotherDraft(t *testing.T) {
	m := loaded(t, nil)
	m = m.openDraft(tb.Draft{Kind: tb.NewMail, Body: "first"})
	gen := m.draftGen
	m = m.closeDraft()
	m = m.openDraft(tb.Draft{Kind: tb.NewMail, Body: "second"})
	m, _ = update(m, revisedMsg{gen: gen, r: agent.Revision{Body: "rewritten first"}})
	if m.draft.Body != "second" || m.version() != 1 {
		t.Fatalf("a revision of the closed draft replaced the open one: %q", m.draft.Body)
	}
	m, _ = update(m, discardMsg{gen}) // nor may a late answer to its save question
	if m.draft == nil {
		t.Fatal("late discard closed the open draft")
	}
}

func TestComposeWithAIOff(t *testing.T) {
	m := loaded(t, nil)
	m.ag = &agent.Agent{URL: "off"}
	m, cmd := pressCmd(t, m, "c")
	if cmd == nil || m.mode != mDraft || m.compose.Focused() {
		t.Fatalf("c without AI should open the editor: cmd %v mode %d", cmd != nil, m.mode)
	}
	m = editDraft(t, m, "To: bob@x.no\nSubject: Hi\n--- write below this line ---\nhello\n")
	m = press(t, m, "x", "enter")
	if m.compose.Value() != "" || m.revising || m.draft.Body != "hello\n" {
		t.Fatalf("input should be off: %q", m.compose.Value())
	}
	if v := screen(m); !strings.Contains(v, "AI is off") || !strings.Contains(v, "u undo · e editor") {
		t.Fatalf("screen:\n%s", v)
	}
	if _, cmd := pressCmd(t, m, "e"); cmd == nil {
		t.Fatal("e should open the editor")
	}
	m = press(t, m, "u")
	if m.draft.Body != "" || m.version() != 0 {
		t.Fatalf("u should undo the edit: %q", m.draft.Body)
	}
}

func TestAssistantDraftOpensAsV1(t *testing.T) {
	m := openTestDraft(loaded(t, nil), tb.Draft{Kind: tb.Reply, ReplyTo: 1, Body: "Takk!\n"})
	if m.mode != mDraft || m.version() != 1 || !strings.Contains(screen(m), "drafted by the assistant (v1)") {
		t.Fatalf("mode %d chat %q", m.mode, m.draftChat)
	}
}

func TestComposeLayouts(t *testing.T) {
	m := openTestDraft(loaded(t, nil), tb.Draft{Kind: tb.Reply, ReplyTo: 1, Body: "Takk!\n"})
	row := func(m Model, s string) int {
		for i, l := range strings.Split(screen(m), "\n") {
			if strings.Contains(l, s) {
				return i
			}
		}
		t.Fatalf("%q not on screen:\n%s", s, screen(m))
		return -1
	}
	if row(m, "[2] Reply · v1") != row(m, "✦ assistant") {
		t.Fatal("wide terminal: draft and agent side by side")
	}
	m, _ = update(m, tea.WindowSizeMsg{Width: 60, Height: 30})
	lines := strings.Split(screen(m), "\n")
	if len(lines) != 30 || row(m, "[2] Reply · v1") >= row(m, "✦ assistant") || !strings.Contains(screen(m), "Takk!") {
		t.Fatalf("narrow terminal: agent below the draft, %d rows:\n%s", len(lines), screen(m))
	}
	for _, l := range lines {
		if w := len([]rune(l)); w > 60 {
			t.Fatalf("line wider than the terminal (%d): %q", w, l)
		}
	}
}
