package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/davidhyldgaard/lazythunder/internal/tb"
)

var (
	accent    = lipgloss.Color("4")
	dim       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bold      = lipgloss.NewStyle().Bold(true)
	titleSt   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	selFocus  = lipgloss.NewStyle().Reverse(true)
	selBlur   = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	errSt     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	unreadSt  = lipgloss.NewStyle().Bold(true)
	flagSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	confirmSt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
)

func (m Model) widths() (left, mid, right int) {
	left = max(20, min(34, m.w/5))
	mid = (m.w - left) * 45 / 100
	return left, mid, m.w - left - mid
}

// listHeight is the number of rows available inside a list panel.
func (m Model) listHeight() int { return max(1, m.h-2-3) }

func (m *Model) layout() {
	_, _, right := m.widths()
	m.preview.Width, m.preview.Height = right-2, m.listHeight()
	m.agentVP.Width, m.agentVP.Height = right-2, m.listHeight()
	if m.full != nil {
		m.preview.SetContent(m.renderMessage(*m.full))
	}
	m.input.Width = m.w - 4
}

func panel(title string, lines []string, w, h int, focused bool) string {
	border := lipgloss.Color("8")
	if focused {
		border = accent
	}
	inner := w - 2
	out := []string{titleSt.Render(ansi.Truncate(title, inner, "…"))}
	for _, l := range lines {
		if len(out) >= h-2 {
			break
		}
		out = append(out, ansi.Truncate(l, inner, "…"))
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Width(inner).Height(h - 2).Render(strings.Join(out, "\n"))
}

// window returns the visible slice of rows keeping cur on screen, and cur's index within it.
func window(n, cur, height int) (from, to int) {
	from = max(0, min(cur-height/2, n-height))
	return from, min(n, from+height)
}

func highlight(s string, w int, focused bool) string {
	s = ansi.Truncate(s, w, "…")
	pad := strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
	if focused {
		return selFocus.Render(ansi.Strip(s) + pad)
	}
	return selBlur.Render(ansi.Strip(s) + pad)
}

func (m Model) View() string {
	if m.w == 0 {
		return ""
	}
	if m.mode == mHelp {
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(1, 2).Render(helpText)
		return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
	}
	left, mid, right := m.widths()
	h := m.h - 2
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		m.viewFolders(left, h), m.viewMessages(mid, h), m.viewRight(right, h))
	return body + "\n" + m.viewBottom()
}

func (m Model) viewFolders(w, h int) string {
	type row struct {
		text string
		idx  int
	}
	var rows []row
	last := ""
	for i, f := range m.folders {
		if f.AccountID != last {
			last = f.AccountID
			rows = append(rows, row{dim.Render(m.accounts[f.AccountID].Name), -1})
		}
		t := strings.Repeat(" ", f.Depth) + f.Name
		if f.Unread > 0 {
			t = unreadSt.Render(fmt.Sprintf("%s (%d)", t, f.Unread))
		}
		rows = append(rows, row{t, i})
	}
	cur := 0
	for i, r := range rows {
		if r.idx == m.fcur {
			cur = i
		}
	}
	from, to := window(len(rows), cur, m.listHeight())
	var lines []string
	for i := from; i < to; i++ {
		if i == cur {
			lines = append(lines, highlight(rows[i].text, w-2, m.focus == pFolders))
		} else {
			lines = append(lines, rows[i].text)
		}
	}
	title := "[1] Folders"
	if m.mode == mMovePick {
		title = "[1] Move to…"
	}
	return panel(title, lines, w, h, m.focus == pFolders)
}

func shortDate(t time.Time) string {
	t, now := t.Local(), time.Now()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 02")
	}
	return t.Format("2006-01")
}

func name(addr string) string {
	if i := strings.Index(addr, "<"); i > 0 {
		return strings.Trim(strings.TrimSpace(addr[:i]), `"`)
	}
	return addr
}

func (m Model) viewMessages(w, h int) string {
	title := "[2] Messages"
	if m.open != nil {
		title = "[2] " + m.accounts[m.open.AccountID].Name + " › " + m.open.Name
	} else if m.query != "" {
		title = fmt.Sprintf("[2] Search: %s (esc to go back)", m.query)
	}
	if m.unread {
		title += " · unread"
	}
	from, to := window(len(m.msgs), m.mcur, m.listHeight())
	var lines []string
	for i := from; i < to; i++ {
		x := m.msgs[i]
		mark := " "
		if !x.Read {
			mark = "●"
		}
		if x.Flagged {
			mark = flagSt.Render("★")
		}
		line := fmt.Sprintf("%s %-6s %-16s %s", mark, shortDate(x.Date), ansi.Truncate(name(x.Author), 16, "…"), x.Subject)
		if !x.Read {
			line = unreadSt.Render(line)
		}
		if i == m.mcur {
			line = highlight(line, w-2, m.focus == pMessages)
		}
		lines = append(lines, line)
	}
	if len(m.msgs) == 0 {
		lines = append(lines, dim.Render("(empty)"))
	}
	return panel(title, lines, w, h, m.focus == pMessages)
}

func (m Model) viewRight(w, h int) string {
	focused := m.focus == pRight
	if m.draft != nil && (m.mode == mDraft || m.mode == mConfirm) {
		return panel("[3] "+kindName(m.draft.Kind), m.draftLines(w-2), w, h, true)
	}
	if m.showAgent {
		title := "[3] borealis2"
		if m.agentBusy {
			title += " · " + m.agentState
		}
		return panel(title, strings.Split(m.agentVP.View(), "\n"), w, h, focused)
	}
	return panel("[3] Preview", strings.Split(m.preview.View(), "\n"), w, h, focused)
}

func (m Model) draftLines(w int) []string {
	d := m.draft
	var head []string
	if d.Kind == tb.NewMail {
		head = append(head, "From: "+d.From, "To: "+d.To)
		if d.Cc != "" {
			head = append(head, "Cc: "+d.Cc)
		}
		head = append(head, "Subject: "+d.Subject)
	} else {
		head = append(head, strings.Split(m.draftCtx, "\n")...)
		if d.Kind == tb.Forward {
			head = append(head, "Forward to: "+d.To)
		}
	}
	for i := range head {
		head[i] = dim.Render(head[i])
	}
	body := lipgloss.NewStyle().Width(w).Render(d.Body)
	help := bold.Render("e edit · enter save draft · S send · x discard")
	return append(append(append(head, ""), strings.Split(body, "\n")...), "", help)
}

func (m Model) renderMessage(x tb.FullMessage) string {
	w := m.preview.Width
	var b strings.Builder
	hdr := func(k, v string) {
		if v != "" {
			b.WriteString(dim.Render(k+": ") + v + "\n")
		}
	}
	hdr("From", x.Author)
	hdr("To", strings.Join(x.Recipients, ", "))
	hdr("Cc", strings.Join(x.CcList, ", "))
	hdr("Date", x.Date.Local().Format("Mon 2006-01-02 15:04"))
	b.WriteString(bold.Render(x.Subject) + "\n")
	for _, a := range x.Parts.Attachments {
		b.WriteString(dim.Render(fmt.Sprintf("📎 %s (%d KB)", a.Name, (a.Size+1023)/1024)) + "\n")
	}
	b.WriteString("\n" + x.PlainText())
	return lipgloss.NewStyle().Width(w).Render(b.String())
}

func (m Model) viewBottom() string {
	switch {
	case m.mode == mInput:
		return m.input.View()
	case m.mode == mConfirm && m.confirm != nil:
		return confirmSt.Render(m.confirm.text + " [y/n]")
	}
	left := m.status
	if m.err != "" {
		left = errSt.Render(m.err)
	}
	var hint string
	switch {
	case !m.ready:
		hint = "ctrl+r retry · q quit"
	case m.mode == mMovePick:
		hint = "enter move here · esc cancel"
	case m.focus == pFolders:
		hint = "enter open · / search · a agent · c compose · ? help"
	default:
		hint = "r reply · e archive · d delete · s summarize · w AI reply · a agent · ? help"
	}
	gap := max(1, m.w-ansi.StringWidth(left)-ansi.StringWidth(hint))
	return ansi.Truncate(left, m.w, "…") + strings.Repeat(" ", gap) + dim.Render(hint)
}

const helpText = `lazythunder — keys

 navigation      tab / shift+tab, 1 2 3, h l    switch panel
                 j k, g G, ctrl+d ctrl+u        move / scroll
                 enter                           open folder / read message (marks read)
                 u                               toggle unread-only
                 /                               search all accounts (esc returns)
                 ctrl+r                          refresh

 message         m  toggle read      F  toggle flag
                 e  archive          d  delete (to trash)
                 M  move to folder   c  compose new
                 r  reply   R  reply all   f  forward

 borealis2       a  ask the agent (it can search, read, and act with your y/n)
                 s  summarize selected message/thread
                 w  draft a reply with AI      p  back to preview
                 ctrl+l  new conversation      esc  cancel a running request

 drafts          e edit in $EDITOR · enter save to Drafts · S send · x discard

 q quit · any key closes this help`
