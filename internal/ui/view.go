package ui

import (
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/davidhyldgaard/lazythunder/internal/render"
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
	tabOn     = lipgloss.NewStyle().Bold(true).Reverse(true).Foreground(accent)
)

// Layout: tab bar (1 row), folders + messages (top), detail pane (bottom), status (1 row).
func (m Model) topHeight() int    { return max(8, (m.h-2)*2/5) }
func (m Model) bottomHeight() int { return max(5, m.h-2-m.topHeight()) }
func (m Model) folderWidth() int  { return max(20, min(34, m.w/5)) }

// listHeight is the number of rows inside a top panel (minus border and title).
func (m Model) listHeight() int { return max(1, m.topHeight()-3) }

func (m *Model) layout() {
	m.preview.Width, m.preview.Height = m.w-2, m.bottomHeight()-3
	m.agentVP.Width, m.agentVP.Height = m.w-2, m.bottomHeight()-3
	if len(m.chat) > 0 {
		m.agentVP.SetContent(m.renderChat())
	}
	m.input.Width = m.w - 4
}

func panel(title string, lines []string, w, h int, focused bool) string {
	border := lipgloss.Color("8")
	if focused {
		border = accent
	}
	inner := w - 2
	out := []string{titleSt.Render(ansi.Truncate(render.Sanitize(title), inner, "…"))}
	for _, l := range lines {
		if len(out) >= h-2 {
			break
		}
		out = append(out, ansi.Truncate(l, inner, "…"))
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Width(inner).Height(h - 2).Render(strings.Join(out, "\n"))
}

// window returns the visible row range keeping cur on screen.
func window(n, cur, height int) (from, to int) {
	from = max(0, min(cur-height/2, n-height))
	return from, min(n, from+height)
}

func highlight(s string, w int, focused bool) string {
	s = ansi.Strip(ansi.Truncate(s, w, "…"))
	s += strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
	if focused {
		return selFocus.Render(s)
	}
	return selBlur.Render(s)
}

func (m Model) View() string {
	if m.w == 0 {
		return ""
	}
	if m.h < 16 || m.w < 50 { // the fixed rows + minimum panels would overflow and scroll
		return ansi.Truncate("lazythunder: terminal too small (need 50×16)", m.w, "")
	}
	if m.mode == mHelp {
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(1, 2).Render(helpText)
		return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top,
		m.viewFolders(m.folderWidth(), m.topHeight()),
		m.viewMessages(m.w-m.folderWidth(), m.topHeight()))
	return strings.Join([]string{m.viewTabs(), top, m.viewDetail(m.w, m.bottomHeight()), m.viewBottom()}, "\n")
}

func (m Model) viewTabs() string {
	label := func(i int, name string, unread int) string {
		s := fmt.Sprintf(" %d %s", i+1, name)
		if unread > 0 {
			s += fmt.Sprintf(" (%d)", unread)
		}
		s += " "
		if i == m.tab {
			return tabOn.Render(s)
		}
		if unread > 0 {
			return bold.Render(s)
		}
		return dim.Render(s)
	}
	parts := []string{label(0, "All inboxes", m.inboxUnread(""))}
	for i, id := range m.acctOrder {
		parts = append(parts, label(i+1, render.Sanitize(m.accounts[id].Name), m.inboxUnread(id)))
	}
	return ansi.Truncate(strings.Join(parts, dim.Render("│")), m.w, "…")
}

func (m Model) viewFolders(w, h int) string {
	vis := m.visibleFolders()
	merged := m.tabAccount() == "" && m.mode != mMovePick // All tab: one inbox per account
	from, to := window(len(vis), m.fcur, m.listHeight())
	var lines []string
	for i := from; i < to; i++ {
		f := vis[i]
		t := strings.Repeat(" ", f.Depth-1) + render.Sanitize(f.Name)
		if merged {
			t = render.Sanitize(m.accounts[f.AccountID].Name)
		}
		if f.Unread > 0 {
			t = unreadSt.Render(fmt.Sprintf("%s (%d)", t, f.Unread))
		}
		if i == m.fcur {
			t = highlight(t, w-2, m.focus == pFolders)
		}
		lines = append(lines, t)
	}
	title := "[1] Folders"
	if m.mode == mMovePick {
		title = "[1] Move to…"
	} else if merged {
		title = "[1] Inboxes"
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
	title := "[2] " + m.view.title
	if m.unread {
		title += " · unread"
	}
	merged := m.view.accountID == "" // show which account each message is from
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
		who := ansi.Truncate(render.Sanitize(name(x.Author)), 20, "…")
		subj := render.Sanitize(x.Subject)
		if merged {
			acct := m.accounts[x.Folder.AccountID].Name
			subj = dim.Render(ansi.Truncate(acct, 14, "…")+" ") + subj
		}
		line := fmt.Sprintf("%s %-6s %-20s %s", mark, shortDate(x.Date), who, subj)
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

func (m Model) viewDetail(w, h int) string {
	focused := m.focus == pDetail
	if m.draft != nil && (m.mode == mDraft || m.mode == mConfirm) {
		return panel("[3] "+kindName(m.draft.Kind), m.draftLines(w-2), w, h, true)
	}
	if m.showAgent {
		title := "[3] borealis2 (p: back to email)"
		if m.agentBusy {
			title = "[3] borealis2 · " + m.agentState
		}
		return panel(title, strings.Split(m.agentVP.View(), "\n"), w, h, focused)
	}
	title := "[3] Preview"
	if m.full != nil {
		title = "[3] " + m.full.Subject
		if pct := m.preview.ScrollPercent(); m.preview.TotalLineCount() > m.preview.Height {
			title += fmt.Sprintf(" · %d%%", int(pct*100))
		}
	}
	return panel(title, strings.Split(m.preview.View(), "\n"), w, h, focused)
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
		head[i] = dim.Render(render.Sanitize(head[i]))
	}
	help := bold.Render("e edit · enter save draft · S send · x discard")
	return append(append(append([]string{help, ""}, head...), ""), strings.Split(render.Text(d.Body, w), "\n")...)
}

func (m Model) renderChat() string {
	var parts []string
	for _, c := range m.chat {
		parts = append(parts, render.Text(c, m.agentVP.Width))
	}
	return strings.Join(parts, "\n\n")
}

// renderMessage returns the preview text and the image srcs still to fetch.
func (m Model) renderMessage(x tb.FullMessage) (string, []string) {
	w := m.preview.Width
	var b strings.Builder
	hdr := func(k, v string) {
		if v != "" {
			b.WriteString(ansi.Wrap(dim.Render(k+": ")+render.Sanitize(v), w, "") + "\n")
		}
	}
	hdr("From", x.Author)
	hdr("To", strings.Join(x.Recipients, ", "))
	hdr("Cc", strings.Join(x.CcList, ", "))
	hdr("Date", x.Date.Local().Format("Mon 2006-01-02 15:04"))
	for _, a := range x.Parts.Attachments {
		b.WriteString(dim.Render(fmt.Sprintf("📎 %s (%d KB)", render.Sanitize(a.Name), (a.Size+1023)/1024)) + "\n")
	}
	b.WriteString(dim.Render(strings.Repeat("─", w)) + "\n")
	var misses []string
	image := func(src string) image.Image {
		img, fetch := m.images.lookup(x.ID, src)
		if fetch {
			misses = append(misses, src)
		}
		return img
	}
	b.WriteString(render.Body(x.Parts.Text, x.Parts.HTML, render.Opts{Width: w, Styled: true, Links: true, PreferHTML: true, Image: image}))
	return b.String(), misses
}

func (m Model) viewBottom() string {
	switch {
	case m.mode == mInput:
		return ansi.Truncate(m.input.View(), m.w, "")
	case m.mode == mConfirm && m.confirm != nil:
		return ansi.Truncate(confirmSt.Render(render.Sanitize(m.confirm.text)+" [y/n]"), m.w, "…")
	}
	left := render.Sanitize(m.status)
	if m.err != "" {
		left = errSt.Render(render.Sanitize(m.err))
	}
	var hint string
	switch {
	case !m.ready:
		hint = "ctrl+r retry · q quit"
	case m.mode == mMovePick:
		hint = "enter move here · esc cancel"
	case m.focus == pFolders:
		hint = "enter open · alt+1-9 tabs · / search · a agent · c compose · ? help"
	default:
		hint = "r reply · e archive · d delete · s summarize · w AI reply · a agent · ? help"
	}
	gap := m.w - ansi.StringWidth(left) - ansi.StringWidth(hint)
	if gap < 1 { // never let the status line wrap: that scrolls the whole screen
		return ansi.Truncate(left, m.w, "…")
	}
	return left + strings.Repeat(" ", gap) + dim.Render(hint)
}

const helpText = `lazythunder — keys

 tabs          alt+1 all inboxes · alt+2..9 accounts · [ ] previous/next

 navigation    tab / shift+tab, 1 2 3, h l     switch panel
               j k, g G, ctrl+d ctrl+u         move / scroll (space pages the email)
               enter                           open folder / read message (marks read)
               u                               toggle unread-only
               /                               search all accounts (esc returns)
               ctrl+r                          refresh

 message       m  toggle read      F  toggle flag
               e  archive          d  delete (to trash)
               M  move to folder   c  compose new
               r  reply   R  reply all   f  forward
               i  view the message's images full screen (sixel/kitty)

 borealis2     a  ask the agent (it can search, read, and act with your y/n)
               s  summarize selected message/thread
               w  draft a reply with AI      p  back to the email
               ctrl+l  new conversation      esc  cancel a running request

 drafts        e edit in $EDITOR · enter save to Drafts · S send · x discard

 q quit · any key closes this help`
