package ui

// The Assistant screen (: or alt+0): a full-screen conversation with the
// agent for search, general questions and actions, its input always focused,
// and on the right the messages its tools found (enter opens one in reading
// mode; esc there comes back). It keeps its conversation across openings;
// ctrl+n starts a new one.

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/bg"
	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/tb"
)

type assistant struct {
	open      bool // the screen is showing (or waits under compose / a y/n)
	onFound   bool // tab moved the focus to the Found list
	lines     []chatLine
	found     []tb.Message // what its tools returned, deduped, newest first
	fcur      int
	busy      bool
	state     string // "thinking…" while busy
	stop      context.CancelFunc
	scroll    int // conversation rows scrolled up from the bottom (pgup/pgdown)
	input     textinput.Model
	inputInit bool
}

// chatLine is one entry of the conversation as shown: the user's own words,
// never the prompt sent for them.
type chatLine struct {
	kind byte // 'u' you, 'a' answer, 's' tool step, 'e' error
	text string
}

type (
	asstStateMsg string
	asstStepMsg  string
	asstFoundMsg []tb.Message
	asstReplyMsg struct {
		text string
		err  error
	}
	// confirmMsg is the agent asking y/n before it changes mail.
	confirmMsg struct {
		text  string
		reply chan bool
	}
)

// asstUpdate handles the Assistant's background messages.
func (m Model) asstUpdate(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case asstStateMsg:
		m.asst.state = string(msg)
	case asstStepMsg:
		m.asst.say('s', string(msg))
	case asstFoundMsg:
		m.asst.found = tb.MergeNewest(m.asst.found, msg)
		m.asst.fcur = min(m.asst.fcur, len(m.asst.found)-1)
	case asstReplyMsg:
		m.asst.busy, m.asst.state, m.asst.stop = false, "", nil
		m.setPause(false)
		if m.confirm != nil && m.confirm.reply != nil { // it ended (e.g. cancelled) while asking
			m.confirm, m.mode = nil, m.modeAfterConfirm()
		}
		if msg.err != nil {
			m.asst.say('e', msg.err.Error())
		} else {
			m.asst.say('a', msg.text)
		}
	case confirmMsg:
		m.confirm = &confirmation{text: msg.text, reply: msg.reply}
		if m.mode == mNormal { // reading a found message: the question brings the Assistant back
			m.asst.open, m.backToAsst = true, false
		}
		m.mode = mConfirm
	default:
		return m, nil, false
	}
	return m, nil, true
}

func (a *assistant) say(kind byte, text string) {
	a.lines = append(a.lines, chatLine{kind, text})
	a.scroll = 0
}

// openAssistant shows the Assistant screen with its input focused.
func (m Model) openAssistant() (Model, tea.Cmd) {
	if !m.ag.Enabled() {
		m.status = "AI is off (LAZYTHUNDER_LLM_URL=off)"
		return m, nil
	}
	if !m.asst.inputInit {
		m.asst.input = textinput.New()
		m.asst.input.Prompt = "> "
		m.asst.input.Placeholder = `ask, e.g. "find the train tickets from October"`
		m.asst.input.Cursor.SetMode(cursor.CursorStatic) // no blink ticks
		m.asst.inputInit = true
	}
	m.asst.input.Focus()
	m.asst.open, m.asst.onFound, m.backToAsst, m.mode = true, false, false, mAssistant
	return m, nil
}

// backToAssistant leaves a message opened from Found for the Assistant,
// restoring the list it replaced.
func (m Model) backToAssistant() (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	if m.view.found != nil && m.view.back != nil {
		var mm tea.Model
		mm, cmd = m.show(*m.view.back)
		m = mm.(Model)
	}
	m, open := m.openAssistant()
	return m, tea.Batch(cmd, open)
}

func (m Model) keyAssistant(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	a := &m.asst
	switch s := k.String(); s {
	case "esc":
		if a.busy && a.stop != nil {
			a.stop()
			a.say('s', "cancelling…")
			return m, nil
		}
		a.open, m.mode = false, mNormal
		a.input.Blur()
		return m, nil
	case "tab", "shift+tab":
		a.onFound = !a.onFound && len(a.found) > 0
		return m, nil
	case "ctrl+n":
		if a.busy {
			m.status = m.model() + " is busy (esc to cancel)"
			return m, nil
		}
		m.ag.Reset()
		a.lines, a.found, a.fcur, a.onFound, a.scroll = nil, nil, 0, false, 0
		m.status = "new conversation"
		return m, nil
	case "pgup":
		a.scroll += 10 // clamped when drawn
		return m, nil
	case "pgdown":
		a.scroll = max(0, a.scroll-10)
		return m, nil
	}
	if a.onFound {
		s := k.String()
		if s == "enter" {
			return m.openFound()
		}
		a.fcur = move(a.fcur, len(a.found), s, 5)
		return m, nil
	}
	if k.String() == "enter" {
		v := strings.TrimSpace(a.input.Value())
		if v == "" {
			return m, nil
		}
		if a.busy {
			m.status = m.model() + " is busy (esc to cancel)"
			return m, nil
		}
		a.input.SetValue("")
		return m.ask(v, v)
	}
	var cmd tea.Cmd
	a.input, cmd = a.input.Update(k)
	return m, cmd
}

// ask sends prompt to the agent; the conversation shows only shown.
func (m Model) ask(shown, prompt string) (Model, tea.Cmd) {
	if m.asst.busy {
		m.status = m.model() + " is busy (esc to cancel)"
		return m, nil
	}
	m.asst.busy, m.asst.state = true, "thinking…"
	m.asst.say('u', shown)
	m.setPause(true)
	ctx, cancel := context.WithCancel(m.ctx)
	m.asst.stop = cancel
	view, ag := m.viewContext(), m.ag
	hooks := agent.Hooks{
		Status:  func(s string) { sendMsg(asstStateMsg(s)) },
		Step:    func(s string) { sendMsg(asstStepMsg(s)) },
		Found:   func(ms []tb.Message) { sendMsg(asstFoundMsg(ms)) },
		Propose: func(d tb.Draft) { sendMsg(draftMsg{d}) },
		Confirm: func(text string) bool {
			reply := make(chan bool, 1)
			sendMsg(confirmMsg{text, reply})
			select {
			case ok := <-reply:
				return ok
			case <-ctx.Done():
				return false
			}
		},
	}
	return m, func() tea.Msg {
		defer cancel()
		text, err := ag.Ask(ctx, prompt, view, hooks)
		return asstReplyMsg{text, err}
	}
}

// openFound shows the Found message under the cursor in reading mode: the
// cursor moves to it when the list has it, else the Found messages open as
// a list. Either way esc comes back to the Assistant.
func (m Model) openFound() (tea.Model, tea.Cmd) {
	if len(m.asst.found) == 0 {
		return m, nil
	}
	x := m.asst.found[m.asst.fcur]
	m.asst.open, m.mode, m.focus = false, mNormal, pMessages
	m.asst.input.Blur()
	for i, y := range m.msgs {
		if y.ID == x.ID {
			m.mcur, m.backToAsst = i, true
			return m, m.schedulePreview()
		}
	}
	back := m.view
	if back.back != nil { // from a thread or an earlier results list: esc goes back to the mail
		back = *back.back
	}
	found := append([]tb.Message(nil), m.asst.found...)
	mm, cmd := m.show(listView{title: fmt.Sprintf("Assistant results (%d) · esc back", len(found)), found: found, focus: x.HeaderMessageID, back: &back})
	m = mm.(Model)
	m.backToAsst = true
	return m, cmd
}

// ---- view ----

func (m Model) viewAssistant() string {
	side, cw, ch, fw, fh := m.splitLayout()
	title := "Assistant · " + m.model()
	if m.asst.busy {
		title += " · " + m.asst.state + " (esc cancels)"
	}
	conv := panel(title, m.asstLines(cw-2, ch-3), cw, ch, !m.asst.onFound)
	found := panel(fmt.Sprintf("Found (%d)", len(m.asst.found)), m.foundLines(fw-2, fh-3), fw, fh, m.asst.onFound)
	if side {
		return lipgloss.JoinHorizontal(lipgloss.Top, conv, found)
	}
	return conv + "\n" + found
}

// asstLines is the visible part of the conversation with the input (or the
// agent's y/n question) on the bottom row.
func (m Model) asstLines(w, rows int) []string {
	var all []string
	for _, l := range m.asst.lines {
		var s string
		switch l.kind {
		case 'u':
			s = bold.Render("you: ") + render.Text(l.text, w-5)
		case 'a':
			s = titleSt.Render("✦ ") + render.Text(l.text, w-2)
		case 's':
			s = dim.Render("  · " + ansi.Truncate(render.Sanitize(bg.OneLine(l.text)), w-4, "…"))
		case 'e':
			s = errSt.Render(render.Text("error: "+l.text, w))
		}
		if l.kind == 'u' && len(all) > 0 {
			all = append(all, "") // a blank line before each question
		}
		all = append(all, strings.Split(s, "\n")...)
	}
	if len(all) == 0 {
		all = []string{dim.Render("Search, ask, or have it act on your mail (it asks y/n before changing anything)."),
			dim.Render("z on a message asks for its summary here. ctrl+n starts a new conversation.")}
	}
	h := rows - 1
	scroll := min(m.asst.scroll, max(0, len(all)-h))
	end := len(all) - scroll
	view := all[max(0, end-h):end]
	for len(view) < h {
		view = append(view, "")
	}
	if c := m.confirm; c != nil && c.reply != nil {
		return append(view, confirmSt.Render(ansi.Truncate(render.Sanitize(c.text)+"? [y/n]", w, "…")))
	}
	in := m.asst.input
	in.Width = max(1, w-3)
	return append(view, in.View())
}

func (m Model) foundLines(w, rows int) []string {
	if len(m.asst.found) == 0 {
		return []string{dim.Render("messages the assistant finds show here")}
	}
	from, to := window(len(m.asst.found), m.asst.fcur, rows)
	var out []string
	for i := from; i < to; i++ {
		x := m.asst.found[i]
		line := fmt.Sprintf("%-6s %s · %s", shortDate(x.Date), render.Sanitize(senderName(x.Author)), render.Sanitize(bg.OneLine(x.Subject)))
		if i == m.asst.fcur {
			line = highlight(line, w, m.asst.onFound)
		} else {
			line = ansi.Truncate(line, w, "…")
		}
		out = append(out, line)
	}
	return out
}

func (m Model) asstHint() string {
	switch {
	case m.asst.onFound:
		return "j/k move · enter open · tab input · esc back"
	case m.asst.busy:
		return "esc cancel · pgup/pgdown scroll"
	}
	return "enter ask · tab Found · ctrl+n new conversation · pgup/pgdown scroll · esc back"
}
