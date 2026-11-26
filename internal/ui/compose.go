package ui

// Compose mode: the draft on the left (always the current version), and an
// agent column for this draft on the right: what you asked, short ✦ notes on
// what changed, and an input line. Every message writes or revises the draft.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/tb"
)

type (
	// revisedMsg is the agent's new version of the draft that was open at gen.
	revisedMsg struct {
		gen int
		ask string
		r   agent.Revision
		err error
	}
	saveDraftMsg struct{ gen int } // "Save to Drafts?" answered y
	discardMsg   struct{ gen int } // … answered n
)

// editedByHand stands in the draft's instruction history for an $EDITOR edit.
const editedByHand = "(I edited the draft by hand)"

// openDraft opens compose mode for d. A draft that already has a body (the
// assistant's) is v1; an empty one waits for the gist in the input.
func (m Model) openDraft(d tb.Draft) Model {
	m.draft, m.draftCtx, m.draftFile, m.mode = &d, m.contextFor(d), "", mDraft
	m.draftGen++
	m.draftUndo, m.draftAsks, m.draftChat, m.revising, m.draftTop, m.draftBase = nil, nil, nil, false, 0, 0
	if strings.TrimSpace(d.Body) != "" {
		m.draftBase = 1
		m.draftChat = []string{"✦ drafted by the assistant (v1)"}
	}
	m.compose = textinput.New()
	m.compose.Prompt = "> "
	m.compose.Cursor.SetMode(cursor.CursorStatic) // no blink ticks to start
	if m.ag.Enabled() {
		m.compose.Placeholder = map[bool]string{true: `what should it say? e.g. "yes, friday works"`,
			false: `how should it change? e.g. "shorter", "in English"`}[m.draftBase == 0]
		m.compose.Focus()
	}
	return m
}

// startCompose opens compose mode; without AI it goes straight to $EDITOR.
func (m Model) startCompose(d tb.Draft) (tea.Model, tea.Cmd) {
	m = m.openDraft(d)
	if !m.ag.Enabled() {
		return m, m.edit()
	}
	return m, nil
}

// version is the draft's version number; 0 while nothing is written.
func (m Model) version() int { return m.draftBase + len(m.draftUndo) }

func (m Model) keyDraft(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.draft == nil {
		m.mode = mNormal
		return m, nil
	}
	if m.submitting {
		m.status = "still saving…"
		return m, nil
	}
	s, ai := k.String(), m.ag.Enabled()
	if !ai { // no input to type into: the plain letters work too
		switch s {
		case "e":
			s = "ctrl+e"
		case "u":
			s = "ctrl+z"
		}
	}
	switch s {
	case "ctrl+s":
		return m.submitDraft(true)
	case "ctrl+d":
		return m.submitDraft(false)
	case "ctrl+e":
		if m.revising {
			m.status = "still writing… (wait for it)"
			return m, nil
		}
		return m, m.edit()
	case "ctrl+z":
		return m.undoRevision()
	case "esc":
		return m.closePrompt()
	case "pgup":
		m.draftTop = max(0, m.draftTop-10)
		return m, nil
	case "pgdown":
		m.draftTop = min(m.draftTop+10, m.draftMaxTop())
		return m, nil
	case "enter":
		if v := strings.TrimSpace(m.compose.Value()); ai && v != "" {
			return m.revise(v)
		}
		return m, nil
	}
	if !ai {
		return m, nil
	}
	var cmd tea.Cmd
	m.compose, cmd = m.compose.Update(k)
	return m, cmd
}

// submitDraft saves (ctrl+d) or, after a y/n, sends (ctrl+s) the draft.
func (m Model) submitDraft(send bool) (tea.Model, tea.Cmd) {
	switch {
	case m.revising:
		m.status = "still writing… (wait, or esc to close)"
		return m, nil
	case m.draftFile != "":
		m.err = "fix the draft first (ctrl+e to edit)"
		return m, nil
	}
	d := *m.draft
	if err := validDraft(d); err != nil {
		m.err = "draft: " + err.Error() + " (ctrl+e to edit)"
		return m, nil
	}
	c, ctx := m.c, m.ctx
	submit := func() tea.Msg { return submittedMsg{send, c.Submit(ctx, d, send)} }
	if !send {
		m.submitting, m.status = true, "saving draft…"
		return m, submit
	}
	m.confirm = &confirmation{text: "Send this message now?", onYes: tea.Sequence(
		func() tea.Msg { return sendingMsg{} }, submit)}
	m.mode = mConfirm
	return m, nil
}

// closePrompt asks whether to keep the draft; an empty one just closes.
func (m Model) closePrompt() (tea.Model, tea.Cmd) {
	if strings.TrimSpace(m.draft.Body) == "" && m.draftFile == "" {
		m = m.closeDraft()
		if m.draft == nil {
			m.status = "draft discarded"
		}
		return m, nil
	}
	gen := m.draftGen
	m.confirm = &confirmation{text: "Save to Drafts? y save · n discard · esc keep writing",
		onYes: func() tea.Msg { return saveDraftMsg{gen} }, onNo: func() tea.Msg { return discardMsg{gen} }}
	m.mode = mConfirm
	return m, nil
}

// revise writes (v1) or rewrites the open draft with the agent. The call is
// stateless: it carries the original message, this draft's earlier
// instructions and the current draft.
func (m Model) revise(instruction string) (tea.Model, tea.Cmd) {
	switch {
	case m.revising:
		m.status = "still writing… (send it when this one is done)"
		return m, nil
	case m.draftFile != "":
		m.err = "fix the draft first (ctrl+e to edit)"
		return m, nil
	}
	d := *m.draft
	history := append([]string(nil), m.draftAsks...)
	m.compose.SetValue("")
	m.revising, m.err, m.status = true, "", ""
	m.draftChat = append(m.draftChat, "you: "+instruction)
	ag, c, ctx, gen := m.ag, m.c, m.ctx, m.draftGen
	var open *tb.FullMessage
	if m.full != nil && m.full.ID == d.ReplyTo {
		open = m.full
	}
	return m, func() tea.Msg {
		original := ""
		if d.Kind != tb.NewMail {
			src := open
			if src == nil {
				if x, err := c.Read(ctx, d.ReplyTo); err == nil {
					src = &x
				}
			}
			if src != nil { // shortcut: without the original the draft is written on its own
				original = fmt.Sprintf("From: %s\nSubject: %s\n\n%s", src.Author, src.Subject,
					render.Body(src.Parts.Text, src.Parts.HTML, render.Opts{Width: 100, StripQuotes: true}))
			}
		}
		r, err := ag.ReviseDraft(ctx, d, original, history, instruction)
		return revisedMsg{gen, instruction, r, err}
	}
}

// applyRevision makes the agent's version current, keeping the old one for undo.
func (m Model) applyRevision(msg revisedMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.draftGen || m.draft == nil {
		return m, nil // that draft is gone
	}
	m.revising = false
	if msg.err != nil {
		m.err = "writing: " + msg.err.Error()
		m.draftChat = append(m.draftChat, "✦ failed: "+msg.err.Error())
		return m, nil
	}
	d := *m.draft
	m.draftUndo, m.draftAsks = append(m.draftUndo, d), append(m.draftAsks, msg.ask)
	d.Subject, d.To, d.Cc, d.Body = msg.r.Subject, msg.r.To, msg.r.Cc, msg.r.Body
	m.draft = &d
	m.draftChat = append(m.draftChat, fmt.Sprintf("✦ %s (v%d)", msg.r.Note, m.version()))
	if m.compose.Placeholder != "" {
		m.compose.Placeholder = `how should it change? e.g. "shorter", "in English"`
	}
	return m, nil
}

// undoRevision restores the version before the last revision or edit.
func (m Model) undoRevision() (tea.Model, tea.Cmd) {
	n := len(m.draftUndo)
	switch {
	case m.revising:
		m.status = "still writing… (wait for it)"
		return m, nil
	case m.draftFile != "":
		m.err = "fix the draft first (ctrl+e to edit)"
		return m, nil
	case n == 0:
		m.status = "nothing to undo"
		return m, nil
	}
	prev := m.draftUndo[n-1]
	m.draft, m.draftUndo, m.draftAsks = &prev, m.draftUndo[:n-1], m.draftAsks[:n-1]
	note := fmt.Sprintf("✦ back to v%d", m.version())
	if m.version() == 0 {
		note = "✦ back to an empty draft"
	}
	m.draftChat = append(m.draftChat, note)
	return m, nil
}

// composeLayout puts the draft and its agent side by side when the terminal
// is wide enough, else the draft on top and the agent below.
func (m Model) composeLayout() (side bool, dw, dh, aw, ah int) {
	h := m.h - 2 // tab bar and status line
	if m.w >= 90 {
		aw = min(60, m.w*2/5)
		return true, m.w - aw, h, aw, h
	}
	ah = max(6, h/3)
	return false, m.w, h - ah, m.w, ah
}

func (m Model) draftMaxTop() int {
	_, dw, dh, _, _ := m.composeLayout()
	return max(0, len(m.draftLines(dw-2))-(dh-3))
}

func (m Model) viewCompose() string {
	side, dw, dh, aw, ah := m.composeLayout()
	title := "[2] " + kindName(m.draft.Kind)
	if v := m.version(); v > 0 {
		title += fmt.Sprintf(" · v%d", v)
	}
	lines := m.draftLines(dw - 2)
	draft := panel(title, lines[min(m.draftTop, max(0, len(lines)-(dh-3))):], dw, dh, true)
	atitle := "✦ " + m.model()
	if m.revising {
		atitle += " · writing…"
	}
	chat := panel(atitle, m.composeChatLines(aw-2, ah-3), aw, ah, m.ag.Enabled())
	if side {
		return lipgloss.JoinHorizontal(lipgloss.Top, draft, chat)
	}
	return draft + "\n" + chat
}

// composeChatLines is the end of this draft's conversation with the input
// line on the bottom row.
func (m Model) composeChatLines(w, rows int) []string {
	var chat []string
	for _, c := range m.draftChat {
		l := render.Text(c, w)
		if strings.HasPrefix(c, "✦") {
			l = dim.Render(l)
		}
		chat = append(chat, strings.Split(l, "\n")...)
	}
	if m.revising {
		chat = append(chat, dim.Render("✦ writing…"))
	}
	chat = chat[max(0, len(chat)-(rows-1)):]
	for len(chat) < rows-1 {
		chat = append(chat, "")
	}
	if !m.ag.Enabled() {
		return append(chat, dim.Render("AI is off · e opens your editor"))
	}
	in := m.compose
	in.Width = max(1, w-3)
	return append(chat, in.View())
}

// composeHint is the status line's key hint in compose mode.
func (m Model) composeHint() string {
	keys := "ctrl+z undo · ctrl+e editor"
	if !m.ag.Enabled() {
		keys = "u undo · e editor"
	}
	hint := keys + " · ctrl+s send · ctrl+d save draft · esc close"
	if v := m.version(); v > 0 {
		hint = fmt.Sprintf("v%d · %s", v, hint)
	}
	return hint
}
