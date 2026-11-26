package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/daveHylde/lazythunder/internal/bg"
	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// threadMsg carries the conversation of the open message (Message-ID hdr).
type threadMsg struct {
	hdr   string
	items []tb.ThreadItem
}

// threadBoxRows caps the thread box; longer threads show a window around the open message.
const threadBoxRows = 8

// loadThread looks up the open message's conversation in gloda (milliseconds).
func (m Model) loadThread() tea.Cmd {
	if m.full == nil || m.full.HeaderMessageID == "" {
		return nil
	}
	c, ctx, hdr := m.c, m.ctx, m.full.HeaderMessageID
	return func() tea.Msg {
		items, _ := c.Conversation(ctx, hdr) // shortcut: no thread box on error or before gloda indexes it
		return threadMsg{hdr, items}
	}
}

// threadItems is the open message's conversation, when it has more than one message.
func (m Model) threadItems() []tb.ThreadItem {
	if m.full == nil || m.threadHdr != m.full.HeaderMessageID || len(m.thread) < 2 {
		return nil
	}
	return m.thread
}

// threadIndex is the position of the open message in its conversation (-1: not found).
func (m Model) threadIndex(items []tb.ThreadItem) int {
	for i, it := range items {
		if m.full != nil && sameMsgID(it.HeaderMessageID, m.full.HeaderMessageID) {
			return i
		}
	}
	return -1
}

func sameMsgID(a, b string) bool { return strings.Trim(a, "<>") == strings.Trim(b, "<>") }

func threadLine(n int, it tb.ThreadItem) string {
	return fmt.Sprintf("%2d %s %s", n, shortDate(it.Date), render.Sanitize(bg.OneLine(senderName(it.Author))))
}

// senderName is the display name of an author like `"Kari" <k@x.no>`, else the address.
func senderName(author string) string {
	if i := strings.Index(author, "<"); i > 0 {
		if name := strings.Trim(strings.TrimSpace(author[:i]), `"`); name != "" {
			return name
		}
	}
	return strings.Trim(author, "<> ")
}

// threadLines renders the sidebar's thread box, the open message highlighted.
func (m Model) threadLines(w, rows int, section func(string) string) []string {
	items := m.threadItems()
	if items == nil {
		return nil
	}
	cur := m.threadIndex(items)
	from, to := window(len(items), max(cur, 0), rows)
	lines := []string{section(fmt.Sprintf("Thread (%d) · t+n opens", len(items)))}
	if from > 0 {
		lines = append(lines, dim.Render(fmt.Sprintf("   … %d older", from)))
	}
	for i := from; i < to; i++ {
		line := ansi.Truncate(threadLine(i+1, items[i]), w, "…")
		if i == cur {
			line = highlight(line, w, true)
		} else {
			line = dim.Render(line)
		}
		lines = append(lines, line)
	}
	if to < len(items) {
		lines = append(lines, dim.Render(fmt.Sprintf("   … %d newer", len(items)-to)))
	}
	return lines
}

// openThreadPicker lists the conversation; a number or enter opens a message.
func (m Model) openThreadPicker() (tea.Model, tea.Cmd) {
	items := m.threadItems()
	if items == nil {
		m.status = "not part of a thread"
		return m, nil
	}
	p := &picker{kind: "thread", msgID: m.full.ID, conv: items}
	for i, it := range items {
		p.items = append(p.items, pickItem{n: i + 1, title: senderName(it.Author),
			detail: shortDate(it.Date) + " · " + bg.OneLine(it.Subject), thread: it})
	}
	p.cur = max(0, m.threadIndex(items))
	m.picker, m.mode = p, mPicker
	return m, nil
}

// openThreadItem shows a message of the conversation: the cursor moves to it
// when the list has it, else the whole conversation opens as a list (esc back).
func (m Model) openThreadItem(it tb.ThreadItem, items []tb.ThreadItem) (tea.Model, tea.Cmd) {
	m.picker, m.mode = nil, mNormal
	for i, x := range m.msgs {
		if sameMsgID(x.HeaderMessageID, it.HeaderMessageID) {
			m.mcur = i
			return m, m.schedulePreview()
		}
	}
	back := m.view
	if back.back != nil { // already in a thread view: esc still returns to where the first one came from
		back = *back.back
	}
	m.focus = pMessages
	subject := bg.OneLine(items[0].Subject)
	return m.show(listView{title: fmt.Sprintf("Thread: %s (esc back)", subject), thread: items, focus: it.HeaderMessageID, back: &back})
}

// loadThreadList resolves a thread view's conversation to messages, streamed like search results.
func (m Model) loadThreadList(seq int) tea.Cmd {
	c, ctx, items := m.c, m.ctx, m.view.thread
	if m.viewCtx != nil {
		ctx = m.viewCtx
	}
	return func() tea.Msg {
		err := c.ThreadMessages(ctx, items, func(ms []tb.Message) bool {
			prog.Send(listMsg{seq: seq, msgs: ms, partial: true})
			return latestList.Load() == int64(seq)
		})
		return listMsg{seq: seq, err: err}
	}
}
