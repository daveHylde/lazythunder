package ui

import (
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/daveHylde/lazythunder/internal/bg"
	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// maxTextWidth caps email text (and the reply/draft text) for readability.
const maxTextWidth = 75

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
	replySt   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)

// Layout: tab bar (1 row), message list (top ~28%), message pane (rest), status (1 row).
// Folders live in a picker (l), so the message pane gets most of the screen.
func (m Model) topHeight() int    { return max(7, (m.h-2)*7/25) }
func (m Model) bottomHeight() int { return max(5, m.h-2-m.topHeight()) }

// listHeight is the number of rows inside a top panel (minus border and title).
func (m Model) listHeight() int { return max(1, m.topHeight()-3) }

func (m *Model) layout() {
	m.preview.Width, m.preview.Height = m.w-2, m.bottomHeight()-3
	if m.sidebarOn() {
		m.preview.Width -= sidebarGap + m.sidebarWidth() // sidebar pinned right; text still wraps at maxTextWidth
	}
	m.attVP.Width, m.attVP.Height = m.w-2, m.bottomHeight()-3
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
	if m.draft != nil && (m.mode == mDraft || m.mode == mConfirm) {
		return strings.Join([]string{autowrapOff + m.viewTabs(), m.viewCompose(), m.viewBottom()}, "\n")
	}
	if m.asstShown() {
		return strings.Join([]string{autowrapOff + m.viewTabs(), m.viewAssistant(), m.viewBottom()}, "\n")
	}
	top := m.viewMessages(m.w, m.topHeight())
	// Autowrap off (DECAWM): a glyph the terminal draws wider than we measured
	// (emoji, newer Unicode) clips at the edge instead of wrapping and
	// scrolling the tab bar off screen. Re-sent whenever the tab row redraws.
	return strings.Join([]string{autowrapOff + m.viewTabs(), top, m.viewDetail(m.w, m.bottomHeight()), m.viewBottom()}, "\n")
}

const autowrapOff, autowrapOn = "\x1b[?7l", "\x1b[?7h"

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
	title := "[1] " + m.view.title + " · l folders"
	if m.unread {
		title += " · unread"
	}
	if len(m.marked) > 0 {
		title += fmt.Sprintf(" · %d selected (esc clears)", len(m.marked))
	}
	merged := m.view.accountID == "" // show which account each message is from
	from, to := window(len(m.msgs), m.mcur, m.listHeight())
	var lines []string
	for i := from; i < to; i++ {
		x := m.msgs[i]
		sel := " "
		if m.marked[x.ID] {
			sel = flagSt.Render("▌")
		}
		mark := " "
		if !x.Read {
			mark = "●"
		}
		if x.Flagged {
			mark = flagSt.Render("★")
		}
		switch m.replied[x.ID].Kind {
		case 'r':
			mark += replySt.Render("↩")
		case 'f':
			mark += replySt.Render("→")
		default:
			mark += " "
		}
		who := ansi.Truncate(render.Sanitize(name(x.Author)), 20, "…")
		subj := noteTag(m.meta[x.HeaderMessageID]) + render.Sanitize(x.Subject)
		if merged {
			acct := m.accounts[x.Folder.AccountID].Name
			subj = dim.Render(ansi.Truncate(acct, 14, "…")+" ") + subj
		}
		line := fmt.Sprintf("%s%s %-6s %-20s %s", sel, mark, shortDate(x.Date), who, subj)
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

// categoryTags are the list's short names for note categories; fyi has none.
var categoryTags = map[string]string{"reply-needed": "reply", "action": "act", "newsletter": "news",
	"receipt": "rcpt", "notification": "note", "spam": "spam"}

// noteTag is the dim marker before a subject: "!" for high priority, then the category.
func noteTag(n store.Meta) string {
	var out string
	if n.Priority == "high" {
		out = errSt.Render("!")
	}
	if t := categoryTags[n.Category]; t != "" {
		out += dim.Render(t)
	}
	if out != "" {
		out += " "
	}
	return out
}

func (m Model) viewDetail(w, h int) string {
	focused := m.focus == pDetail
	if m.accts != nil && m.draft == nil && (m.mode == mAccounts || m.mode == mConfirm) {
		title := "[2] Accounts"
		if m.accts.form != nil {
			title = "[2] " + m.accts.form.title
		}
		return panel(title, m.acctLines(w-2, h-3), w, h, true)
	}
	if m.mode == mPicker && m.picker != nil {
		title := fmt.Sprintf("[2] %s (%d) · %s", pickerTitle(m.picker.kind), len(m.picker.items), m.pickerHelp())
		return panel(title, m.pickerLines(w-2), w, h, true)
	}
	if m.mode == mAttView {
		title := "[2] " + m.attTitle + " · esc back"
		if m.attVP.TotalLineCount() > m.attVP.Height {
			title += fmt.Sprintf(" · %d%%", int(m.attVP.ScrollPercent()*100))
		}
		return panel(title, strings.Split(m.attVP.View(), "\n"), w, h, true)
	}
	title := "[2] Preview"
	if m.full != nil {
		title = "[2] " + m.full.Subject
	}
	if x, ok := m.selected(); ok && (m.full == nil || m.full.ID != x.ID) && m.previewFailed != x.ID {
		title = "[2] loading " + x.Subject + "…"
	}
	left := m.withSixel(strings.Split(m.preview.View(), "\n"))
	if m.sidebarOn() && m.full != nil {
		left = m.composeDetail(left, m.preview.Width)
	}
	return panel(title, left, w, h, focused)
}

// visibleSixels lists the on-screen part of every inline image in the preview.
func (m Model) visibleSixels() []sixelReq {
	if !m.inlineSixel() || m.full == nil {
		return nil
	}
	top, h := m.preview.YOffset, m.preview.Height
	var out []sixelReq
	for _, p := range m.placements {
		a, b := max(top, p.Line), min(top+h, p.Line+p.Rows)
		if b > a {
			out = append(out, sixelReq{p: p, top: a - p.Line, n: b - a, row: b - 1 - top})
		}
	}
	return out
}

// withSixel paints sharp sixel images into the blank areas the renderer left
// for them. bubbletea rewrites only rows whose text changed, and rewriting a
// row erases the pixels under it, so every row of a visible image carries the
// same invisible state marker (offset, crop, encoded or not): the rows change
// together, and the sequence on the last row runs after all of them have been
// written, redrawing the image. Unchanged frames rewrite nothing.
func (m Model) withSixel(lines []string) []string {
	for _, r := range m.visibleSixels() {
		if r.row >= len(lines) {
			continue
		}
		enc := m.images.sixelFor(r, m.cellW, m.cellH)
		tag := fmt.Sprintf("\x1b_lt%d.%d.%d.%d\x1b\\", m.preview.YOffset, r.top, r.n, len(enc)) // APC: ignored by the terminal
		for i := max(0, r.row-r.n+1); i <= r.row; i++ {
			lines[i] += tag
		}
		if enc == "" {
			continue // not encoded yet (e.g. just scrolled): blank briefly
		}
		up := ""
		if r.n > 1 {
			up = fmt.Sprintf("\x1b[%dA", r.n-1)
		}
		// column: 1-based, +1 for the panel's left border
		lines[r.row] += "\x1b7" + up + fmt.Sprintf("\x1b[%dG", r.p.Col+2) + enc + "\x1b8"
	}
	return lines
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
	if strings.TrimSpace(d.Body) == "" {
		return append(head, "", dim.Render("(nothing written yet: tell the assistant what to say, or ctrl+e to write it yourself)"))
	}
	return append(append(head, ""), strings.Split(render.Text(d.Body, min(w, maxTextWidth)), "\n")...)
}

// asstShown: the Assistant screen is on (its y/n questions show inline).
func (m Model) asstShown() bool {
	return m.asst.open && m.draft == nil && (m.mode == mAssistant || m.mode == mConfirm)
}

// summaryBlock is the AI summary shown under the message headers.
func (m Model) summaryBlock(hdr string, w int) string {
	if !m.ag.Enabled() || m.st == nil { // no store: no notes
		return ""
	}
	n := m.noteFor(hdr)
	var text string
	switch n.state {
	case sumDone:
		text = render.Text(n.sum.Summary, w-2)
	case sumRunning:
		text = dim.Render("summarizing…")
	case sumQueued:
		text = dim.Render("queued for a summary")
	case sumFailed:
		text = errSt.Render(ansi.Truncate("AI unavailable: "+bg.OneLine(render.Sanitize(n.errText)), w-2, "…"))
	default:
		text = dim.Render("rest on the message for a summary")
	}
	rule := dim.Render(strings.Repeat("─", w)) + "\n" // its own section: ruled off from the headers and the body
	return rule + sideTitle.Render("✦ ") + strings.ReplaceAll(text, "\n", "\n  ") + "\n"
}

// renderMessage returns the preview text, the image srcs still to fetch and
// the message's links.
func (m Model) renderMessage(x tb.FullMessage) (string, []string, []link) {
	paneW := m.preview.Width
	w := min(paneW, maxTextWidth) // text wraps at 75 columns; images may use the pane
	var b strings.Builder
	if r, ok := m.replied[x.ID]; ok {
		b.WriteString(m.renderOwnReply(x.ID, r, w))
	}
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
	b.WriteString(m.summaryBlock(x.HeaderMessageID, w))
	b.WriteString(dim.Render(strings.Repeat("─", w)) + "\n")
	var misses []string
	image := func(src string) (image.Image, string) {
		img, fetch, token := m.images.lookup(x.ID, src)
		if fetch {
			misses = append(misses, src)
		}
		return img, token
	}
	var links []link // links[n-1] is the text's [n]
	onLink := func(u, label string) { links = append(links, link{url: u, label: label}) }
	cw, ch := m.cellW, m.cellH
	if cw <= 0 || ch <= 0 {
		cw, ch = 8, 16
	}
	b.WriteString(render.Body(x.Parts.Text, x.Parts.HTML, render.Opts{Width: w, Styled: true, Links: true, PreferHTML: true,
		Image: image, CellW: cw, CellH: ch, ImageRows: max(3, m.preview.Height-1), ImageWidth: paneW,
		BlankImages: m.inlineSixel(), SafeLink: bg.SafeLink, OnLink: onLink}))
	return b.String(), misses, links
}

// renderOwnReply is the user's earlier reply (or a forward note), dimmed
// behind a ┃ gutter, separated from the message it answers.
func (m Model) renderOwnReply(id int, r store.Reply, w int) string {
	var b strings.Builder
	gutter := replySt.Render("┃ ")
	line := func(s string) { b.WriteString(gutter + dim.Render(s) + "\n") }
	verb := "↩ You replied"
	if r.Kind == 'f' {
		verb = "→ You forwarded this"
	}
	line(fmt.Sprintf("%s · %s · to %s", verb, r.Date.Local().Format("Mon 02 Jan 15:04"), render.Sanitize(name(r.To))))
	if r.Kind == 'r' {
		if m.reply != nil && m.reply.forID == id {
			body := render.Body(m.reply.msg.Parts.Text, m.reply.msg.Parts.HTML, render.Opts{Width: w - 2, StripQuotes: true})
			for _, l := range strings.Split(body, "\n") {
				line(l)
			}
		} else {
			line("loading…")
		}
	}
	label := " original message "
	side := max(2, (w-ansi.StringWidth(label))/2)
	b.WriteString(dim.Render(strings.Repeat("─", side)+label+strings.Repeat("─", max(0, w-side-ansi.StringWidth(label)))) + "\n")
	return b.String()
}

func (m Model) viewBottom() string {
	switch {
	case m.mode == mInput:
		return ansi.Truncate(m.input.View(), m.w, "")
	case m.mode == mConfirm && m.confirm != nil && !m.asstShown():
		text := render.Sanitize(m.confirm.text)
		if m.confirm.onNo == nil { // with onNo the text spells out its keys
			text += " [y/n]"
		}
		return ansi.Truncate(confirmSt.Render(text), m.w, "…")
	}
	left := render.Sanitize(m.status)
	if m.err != "" {
		left = errSt.Render(render.Sanitize(m.err))
	}
	var hint string
	switch {
	case !m.ready:
		hint = "F5 retry · q quit"
	case m.mode == mPicker && m.picker != nil:
		hint = m.pickerHelp()
	case m.mode == mAttView:
		hint = "j/k scroll · esc back"
	case m.mode == mAccounts && m.accts != nil:
		hint = m.acctHelp()
	case m.mode == mDraft && m.draft != nil:
		hint = m.composeHint()
	case m.asstShown() && m.confirm != nil:
		hint = "y yes · n no"
	case m.asstShown():
		hint = m.asstHint()
	case m.backToAsst:
		hint = "esc back to the assistant · r reply · a archive · d delete · ? help"
	default:
		hint = "l folders · r reply · space select · a archive · d delete · C all read · F5 refresh · : assistant · ? help"
	}
	gap := m.w - ansi.StringWidth(left) - ansi.StringWidth(hint)
	if gap < 1 { // never let the status line wrap: that scrolls the whole screen
		return ansi.Truncate(left, m.w, "…")
	}
	return left + strings.Repeat(" ", gap) + dim.Render(hint)
}

const helpText = `lazythunder — keys

 tabs          alt+1 all inboxes · alt+2..9 accounts · [ ] previous/next · alt+0 or : the assistant

 navigation    tab, 1 2, enter / h             switch between list and message
               l                               folder picker
               v                               toggle the sidebar (to do + links, attachments)
               j k, g G, ctrl+d ctrl+u         move / scroll (space pages the email)
               n p                             next / previous unread
               pgup pgdown                     always scroll the email
               enter                           read message (resting 1s on it marks it read)
               u                               toggle unread-only
               /                               search sender + subject (all time) and text (90 days), newest first
               F5                              refresh
               T                               open the Thunderbird window (replaces the headless one)

 message       m  toggle read      S  toggle star (flag)
               a  archive          d, Del  delete (to trash)
               M  move to folder   c  compose new
               r  reply   R  reply all   f  forward   (open compose mode, below)
               C  mark everything in the folder(s) read
               space  select in the list; m S a d M then act on all selected (esc clears)
               i  view the message's images full screen (sixel/kitty)
               o  safe links (type n to open [n], enter open, y copy)
               t  the message's thread (type n to open message n, enter open)
               y  copy the message's code (login code, booking ref, voucher…; y+n when several)
               U  unsubscribe without a browser (one-click, mail, or the agent on the page), then trash
               A  attachments (enter preview, d download, o open)
               list: ● unread  ★ starred  ↩ you replied  → you forwarded  ▌ selected
               tags: ! high priority  reply  act  news  rcpt (receipt)  note (notification)  spam

 assistant     : or alt+0  the Assistant screen: ask, search, act (y/n first); found messages on the right
               z  summarize the selected message/thread in the Assistant
               enter ask · tab Found list (j k move, enter opens the message; esc there comes back)
               ctrl+n new conversation · pgup pgdown scroll · esc cancel a running request, else back to mail

 compose       r R f c: type the gist + enter writes the draft (reply-guide.md), each message revises it
               ctrl+z undo · ctrl+e $EDITOR · pgup pgdown scroll · ctrl+s send · ctrl+d save to Drafts · esc close
               (AI off: e editor · u undo)

 accounts      ctrl+a  accounts screen: a add · e edit servers/identity · D remove (local copy only)
               i add identity · x delete identity · * make identity default · esc close

 q quit · any key closes this help`
