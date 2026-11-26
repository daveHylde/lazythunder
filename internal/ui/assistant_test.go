package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// chatLLM is a fake chat model: reply(n) is the assistant message JSON for
// the n-th request (0-based). It records each request's messages.
func chatLLM(t *testing.T, reply func(n int, r *http.Request) string) (*httptest.Server, func() [][]agent.Msg) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // the default reply guide
	var mu sync.Mutex
	var seen [][]agent.Msg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []agent.Msg }
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		n := len(seen)
		seen = append(seen, req.Messages)
		mu.Unlock()
		w.Write([]byte(`{"choices":[{"message":` + reply(n, r) + `}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]agent.Msg {
		mu.Lock()
		defer mu.Unlock()
		return append([][]agent.Msg(nil), seen...)
	}
}

// script answers requests in order with the given messages.
func script(replies ...string) func(int, *http.Request) string {
	return func(n int, _ *http.Request) string { return replies[min(n, len(replies)-1)] }
}

func toolCall(name, args string) string {
	b, _ := json.Marshal(args)
	return `{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"` + name + `","arguments":` + string(b) + `}}]}`
}

func answer(text string) string {
	b, _ := json.Marshal(text)
	return `{"content":` + string(b) + `}`
}

// asstModel is a loaded model with an assistant on a fake hub and a fake LLM;
// background messages land in the returned channel.
func asstModel(t *testing.T, reply func(int, *http.Request) string) (Model, *testHub, chan tea.Msg, func() [][]agent.Msg) {
	t.Helper()
	client, h := newTestHub(t)
	h.set("accounts", testAccounts())
	m := loaded(t, client)
	srv, seen := chatLLM(t, reply)
	m.ag = &agent.Agent{URL: srv.URL, TB: client, HTTP: srv.Client()}
	ch := make(chan tea.Msg, 100)
	old := sendMsg
	sendMsg = func(msg tea.Msg) { ch <- msg }
	t.Cleanup(func() { sendMsg = old })
	return m, h, ch, seen
}

// drive runs an agent command to its end, applying the messages it sends;
// onConfirm answers y/n questions.
func drive(t *testing.T, m Model, cmd tea.Cmd, ch chan tea.Msg, onConfirm func(Model) Model) Model {
	t.Helper()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	for {
		select {
		case msg := <-ch:
			m, _ = update(m, msg)
			if _, ok := msg.(confirmMsg); ok {
				m = onConfirm(m)
			}
		case msg := <-done:
			for len(ch) > 0 {
				m, _ = update(m, <-ch)
			}
			m, _ = update(m, msg)
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("agent did not finish")
		}
	}
}

// askAsst opens the Assistant, types q and runs the request.
func askAsst(t *testing.T, m Model, q string, ch chan tea.Msg, onConfirm func(Model) Model) Model {
	t.Helper()
	m = press(t, m, ":")
	m, cmd := pressCmd(t, typeText(m, q), "enter")
	if !m.asst.busy || m.asst.input.Value() != "" {
		t.Fatalf("enter: busy %v input %q", m.asst.busy, m.asst.input.Value())
	}
	return drive(t, m, cmd, ch, onConfirm)
}

func TestAssistantOpensFocused(t *testing.T) {
	m, _, _, _ := asstModel(t, script(answer("hi")))
	for _, k := range []string{":", "alt+0"} {
		m = press(t, m, k)
		if m.mode != mAssistant || !m.asst.input.Focused() {
			t.Fatalf("%s: mode %d focused %v", k, m.mode, m.asst.input.Focused())
		}
		v := screen(m)
		if !strings.Contains(v, "Assistant · assistant") || !strings.Contains(v, "Found (0)") || strings.Contains(v, "[1] All inboxes") {
			t.Fatalf("%s: not the full-screen Assistant:\n%s", k, v)
		}
		m = press(t, m, "q", "esc") // q types; esc closes
		if m.mode != mNormal || m.asst.open || m.asst.input.Value() != "q" {
			t.Fatalf("esc: mode %d open %v input %q", m.mode, m.asst.open, m.asst.input.Value())
		}
		m.asst.input.SetValue("")
	}
}

func TestAssistantAnswersAndFinds(t *testing.T) {
	m, h, ch, seen := asstModel(t, script(
		toolCall("search", `{"query":"","from":"alice"}`),
		answer("<|channel>thought\nthe user wants alice<channel|>Alice wrote twice.")))
	hits := []tb.Message{testMsg(3, "a1", "Meeting", false, 3), testMsg(99, "a1", "Old one", true, 900), testMsg(1, "a1", "Hello", false, 1)}
	for i := range hits {
		hits[i].HeaderMessageID = fmt.Sprintf("<%d@x>", hits[i].ID)
	}
	h.set("search", tb.Page{Messages: hits})
	m = askAsst(t, m, "what did alice send?", ch, nil)

	if m.asst.busy || len(m.asst.found) != 3 || m.asst.found[0].ID != 1 || m.asst.found[2].ID != 99 {
		t.Fatalf("busy %v found %v", m.asst.busy, ids(m.asst.found))
	}
	v := screen(m)
	for _, want := range []string{"you: what did alice send?", "· searching…", "✦ Alice wrote twice.", "Found (3)", "Old one"} {
		if !strings.Contains(v, want) {
			t.Errorf("screen lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "thought") || strings.Contains(v, "<|") {
		t.Errorf("leaked reasoning shown:\n%s", v)
	}
	if last := m.ag.History[len(m.ag.History)-1]; last.Content != "Alice wrote twice." {
		t.Errorf("history keeps the reasoning: %q", last.Content)
	}
	if r := seen(); r[0][1].Content != "what did alice send?" || !strings.Contains(r[0][0].Content, "Current view: All inboxes") {
		t.Errorf("sent %q / system %q", r[0][1].Content, r[0][0].Content)
	}

	// Found: tab, j k, enter on a listed message moves the list cursor there
	m = press(t, m, "tab", "j", "j", "k")
	if !m.asst.onFound || m.asst.fcur != 1 || !strings.Contains(statusLine(m), "enter open") {
		t.Fatalf("Found focus %v cur %d", m.asst.onFound, m.asst.fcur)
	}
	m = press(t, m, "enter")
	if m.mode != mNormal || m.asst.open || m.msgs[m.mcur].ID != 3 || !strings.Contains(statusLine(m), "esc back to the assistant") {
		t.Fatalf("enter: mode %d cur %d", m.mode, m.mcur)
	}
	m = press(t, m, "esc")
	if m.mode != mAssistant || m.view.title != "All inboxes" {
		t.Fatalf("esc: mode %d view %q", m.mode, m.view.title)
	}

	// a message not in the list opens the Found messages as a list
	m = press(t, m, "tab", "G", "enter")
	if m.view.found == nil || !strings.HasPrefix(m.view.title, "Assistant results (3)") || m.mode != mNormal {
		t.Fatalf("view %q mode %d", m.view.title, m.mode)
	}
	m, _ = update(m, run(t, m.loadList(false)))
	if len(m.msgs) != 3 || m.msgs[m.mcur].ID != 99 {
		t.Fatalf("results %v cur %d", ids(m.msgs), m.mcur)
	}
	m = press(t, m, "esc")
	if m.mode != mAssistant || m.view.title != "All inboxes" || m.view.found != nil {
		t.Fatalf("esc: mode %d view %q", m.mode, m.view.title)
	}

	// ctrl+n starts over
	m = press(t, m, "tab", "ctrl+n")
	if m.asst.lines != nil || m.asst.found != nil || m.ag.History != nil || m.asst.onFound {
		t.Fatalf("ctrl+n kept: lines %d found %d history %d", len(m.asst.lines), len(m.asst.found), len(m.ag.History))
	}
}

func TestZSummarizesInAssistant(t *testing.T) {
	m, _, ch, seen := asstModel(t, script(answer("Alice says hello.")))
	m, cmd := pressCmd(t, m, "z")
	if m.mode != mAssistant || !m.asst.busy {
		t.Fatalf("z: mode %d busy %v", m.mode, m.asst.busy)
	}
	m = drive(t, m, cmd, ch, nil)
	v := screen(m)
	if !strings.Contains(v, `you: summarize "Hello"`) || strings.Contains(v, "Summarize message id") || !strings.Contains(v, "✦ Alice says hello.") {
		t.Fatalf("screen:\n%s", v)
	}
	if p := seen()[0][1].Content; !strings.Contains(p, "Summarize message id 1.") {
		t.Fatalf("prompt %q", p)
	}
}

func TestAssistantConfirmsInline(t *testing.T) {
	for _, yes := range []bool{true, false} {
		m, h, ch, seen := asstModel(t, script(toolCall("archive", `{"ids":[1]}`), answer("ok")))
		var asked string
		m = askAsst(t, m, "archive the hello mail", ch, func(m Model) Model {
			if m.mode != mConfirm || !m.asstShown() {
				t.Fatalf("question: mode %d shown %v", m.mode, m.asstShown())
			}
			asked = screen(m)
			if m = press(t, m, map[bool]string{true: "y", false: "n"}[yes]); m.mode != mAssistant {
				t.Fatalf("after answer: mode %d", m.mode)
			}
			return m
		})
		if !strings.Contains(asked, "archive [1]? [y/n]") {
			t.Fatalf("question not inline:\n%s", asked)
		}
		archived := false
		for _, c := range h.Calls() {
			archived = archived || strings.HasPrefix(c, "archive ")
		}
		tool := seen()[1]
		if archived != yes || strings.Contains(tool[len(tool)-1].Content, "NOT DONE") == yes {
			t.Fatalf("yes=%v: archived %v, tool result %q", yes, archived, tool[len(tool)-1].Content)
		}
	}
}

func TestAssistantEscCancelsThenCloses(t *testing.T) {
	m, _, ch, _ := asstModel(t, func(_ int, r *http.Request) string {
		<-r.Context().Done() // never answers
		return answer("late")
	})
	m = press(t, m, ":")
	m, cmd := pressCmd(t, typeText(m, "slow question"), "enter")
	m = press(t, m, "esc")
	if m.mode != mAssistant || !m.asst.busy {
		t.Fatalf("first esc should cancel, not close: mode %d busy %v", m.mode, m.asst.busy)
	}
	m = drive(t, m, cmd, ch, nil)
	if m.asst.busy || m.asst.lines[len(m.asst.lines)-1].kind != 'e' {
		t.Fatalf("after cancel: busy %v lines %+v", m.asst.busy, m.asst.lines)
	}
	if m = press(t, m, "esc"); m.mode != mNormal {
		t.Fatalf("second esc: mode %d", m.mode)
	}
}

func TestAssistantDraftOpensComposeAndReturns(t *testing.T) {
	m, _, ch, _ := asstModel(t, script(
		toolCall("propose_draft", `{"kind":"reply","message_id":1,"body":"Takk!"}`), answer("Drafted.")))
	m = askAsst(t, m, "reply thanks to alice", ch, nil)
	if m.mode != mDraft || m.draft == nil || m.draft.Body != "Takk!" || !strings.Contains(screen(m), "drafted by the assistant (v1)") {
		t.Fatalf("propose_draft: mode %d draft %+v", m.mode, m.draft)
	}
	m, cmd := pressCmd(t, press(t, m, "esc"), "n") // close, discard
	m, _ = update(m, run(t, cmd))
	if m.draft != nil || m.mode != mAssistant || !strings.Contains(screen(m), "✦ Drafted.") {
		t.Fatalf("after compose: mode %d\n%s", m.mode, screen(m))
	}
}

func TestAssistantNarrowStacks(t *testing.T) {
	m, _, _, _ := asstModel(t, script(answer("hi")))
	m = press(t, m, ":")
	m, _ = update(m, tea.WindowSizeMsg{Width: 60, Height: 30})
	lines := strings.Split(screen(m), "\n")
	conv, found := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "Assistant · ") {
			conv = i
		}
		if strings.Contains(l, "Found (0)") {
			found = i
		}
		if len([]rune(l)) > 60 {
			t.Fatalf("line wider than the terminal: %q", l)
		}
	}
	if len(lines) != 30 || conv < 0 || found <= conv {
		t.Fatalf("not stacked (%d rows, conv %d found %d):\n%s", len(lines), conv, found, screen(m))
	}
}
