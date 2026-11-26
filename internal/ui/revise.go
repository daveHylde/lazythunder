package ui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// revisedMsg is the agent's rewrite of the draft that was open at gen.
type revisedMsg struct {
	gen           int
	subject, body string
	err           error
}

// startRevise asks how the draft should change (draft screen, ":").
func (m Model) startRevise() (tea.Model, tea.Cmd) {
	switch {
	case !m.ag.Enabled():
		m.status = "AI is off (LAZYTHUNDER_LLM_URL=off)"
		return m, nil
	case m.revising:
		m.status = "still revising…"
		return m, nil
	case m.draftFile != "":
		m.err = "fix the draft first (e to edit)"
		return m, nil
	}
	return m.startInput("revise", `how should the draft change? (e.g. "shorter", "more formal", "in English", "ask for a refund instead")`, "")
}

// revise rewrites the open draft with the agent; the previous version can be restored with u.
func (m Model) revise(instruction string) (tea.Model, tea.Cmd) {
	d := *m.draft
	m.revising = true
	m.err, m.status = "", "revising the draft with "+m.model()+"…"
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
			if src != nil { // shortcut: without the original the draft is revised on its own
				original = fmt.Sprintf("From: %s\nSubject: %s\n\n%s", src.Author, src.Subject,
					render.Body(src.Parts.Text, src.Parts.HTML, render.Opts{Width: 100, StripQuotes: true}))
			}
		}
		r, err := ag.ReviseDraft(ctx, original, d.Subject, d.Body, instruction)
		return revisedMsg{gen, r.Subject, r.Body, err}
	}
}

// applyRevision replaces the draft's text, keeping the old one for u.
func (m Model) applyRevision(msg revisedMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.draftGen || m.draft == nil {
		return m, nil // that draft is gone
	}
	m.revising = false
	if msg.err != nil {
		m.err = "revising: " + msg.err.Error()
		return m, nil
	}
	m.draftUndo = append(m.draftUndo, *m.draft)
	m.draft.Body = msg.body
	if m.draft.Kind == tb.NewMail {
		m.draft.Subject = msg.subject // replies and forwards keep Thunderbird's subject
	}
	m.status = fmt.Sprintf("revised (version %d) · : again · u undo", len(m.draftUndo)+1)
	return m, nil
}

// undoRevision restores the draft as it was before the last revision.
func (m Model) undoRevision() (tea.Model, tea.Cmd) {
	n := len(m.draftUndo)
	if n == 0 {
		m.status = "nothing to undo"
		return m, nil
	}
	prev := m.draftUndo[n-1]
	m.draft, m.draftUndo = &prev, m.draftUndo[:n-1]
	m.status = fmt.Sprintf("back to version %d", n)
	return m, nil
}
