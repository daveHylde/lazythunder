package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/bg"
	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/tb"
	"github.com/daveHylde/lazythunder/internal/termimg"
)

const (
	sidebarMin    = 30 // free columns beside the text needed to show the sidebar
	sidebarMax    = 70
	sidebarGap    = 3 // " │ "
	autoPreviewMB = 15
)

var (
	sideTitle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	riskySt   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
)

// attPreview is the sidebar's automatic preview of a message's first safe attachment.
type attPreview struct {
	msgID   int
	name    string
	kind    attKind
	text    string // first lines of a PDF or text file
	key     string // imageStore key of an image attachment
	err     string
	loading bool
}

type sidebarAttMsg struct{ p attPreview }

func (m Model) sidebarOn() bool { return m.sidebar && m.w-2-maxTextWidth-sidebarGap >= sidebarMin }

func (m Model) sidebarWidth() int { return min(sidebarMax, m.w-2-maxTextWidth-sidebarGap) }

// startAttPreview picks the message's first previewable, non-risky attachment
// and prepares it in the background: images are decoded in Go, PDFs go
// through sandboxed pdftotext, text is read as text. Nothing is opened or run.
func (m *Model) startAttPreview(x tb.FullMessage) tea.Cmd {
	m.attPrev = nil
	for _, a := range x.Parts.Attachments {
		kind, _ := classify(a.Name, a.ContentType)
		if kind != attImage && kind != attPDF && kind != attText || a.Size > autoPreviewMB<<20 {
			continue
		}
		p := attPreview{msgID: x.ID, name: bg.OneLine(a.Name), kind: kind, loading: true}
		m.attPrev = &p // the UI's copy; the goroutine below works on its own
		if m.attCancel != nil {
			m.attCancel() // stop the previous message's preview download
		}
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		m.attCancel = cancel
		c, store := m.c, m.images
		return func() tea.Msg {
			defer cancel()
			p := p // never write the struct the UI is rendering
			p.loading = false
			path, err := fetchAttachment(ctx, c, store, x.ID, a)
			if err != nil {
				p.err = err.Error()
				return sidebarAttMsg{p}
			}
			if why := sniffRisky(path); why != "" {
				p.kind, p.err = attRisky, "⚠ really "+why+": not previewed"
				return sidebarAttMsg{p}
			}
			switch kind {
			case attImage:
				p.key = "att\x00" + path
				if err := store.register(p.key, path); err != nil {
					p.err = err.Error()
				}
			case attPDF:
				p.text, err = pdfText(ctx, path, 2)
				if err != nil {
					p.err = err.Error()
				}
			case attText:
				f, err := os.Open(path)
				if err != nil {
					p.err = err.Error()
					break
				}
				b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
				f.Close()
				p.text = string(b)
			}
			return sidebarAttMsg{p}
		}
	}
	return nil
}

// sidebarLines renders the sidebar for the open message, h rows of width w,
// plus the attachment thumbnail's sixel placement (if any) in sidebar cells.
func (m Model) sidebarLines(w, h int) ([]string, *sixelReq) {
	x := m.full
	section := func(title string) string {
		t := sideTitle.Render(title) + " "
		return t + dim.Render(strings.Repeat("─", max(0, w-ansi.StringWidth(t))))
	}
	wrapDim := func(s, prefix string) []string {
		var out []string
		for i, l := range strings.Split(render.Text(s, w-ansi.StringWidth(prefix)), "\n") {
			p := strings.Repeat(" ", ansi.StringWidth(prefix))
			if i == 0 {
				p = prefix
			}
			out = append(out, p+l)
		}
		return out
	}

	// the conversation, then action items (the summary itself sits at the top of the message)
	top := m.threadLines(w, min(threadBoxRows, max(3, h/3)), section) // leave room for the action items
	sum := m.noteFor(x.HeaderMessageID).sum

	// Action items (with the link that does them), then the other safe links
	info := linkInfos(sum)
	urlLine := func(n int) string {
		return dim.Render(ansi.Truncate("  "+shortURL(m.links[n-1].url), w, "…"))
	}
	usable := func(n int) bool { return n >= 1 && n <= len(m.links) && !info[n].Suspicious }
	used := map[int]bool{}
	if len(sum.Actions) > 0 || len(m.links) > 0 {
		top = append(top, section("Action items · o+n opens"))
	}
	for _, a := range sum.Actions {
		text := a.Text
		if usable(a.Link) {
			text += fmt.Sprintf(" [%d]", a.Link)
		}
		top = append(top, wrapDim(text, "☐ ")...)
		if usable(a.Link) {
			used[a.Link] = true
			top = append(top, urlLine(a.Link))
		}
	}

	// attachments (bottom), with an automatic safe preview
	var bottom []string
	var thumb *sixelReq
	if atts := x.Parts.Attachments; len(atts) > 0 {
		bottom = append(bottom, section(fmt.Sprintf("Attachments (%d) · A", len(atts))))
		for _, a := range atts {
			name := bg.OneLine(a.Name)
			line := fmt.Sprintf("📎 %s %s", name, dim.Render(humanSize(a.Size)))
			if kind, why := classify(a.Name, a.ContentType); kind == attRisky {
				line = riskySt.Render("⚠ "+name) + " " + dim.Render(why)
			}
			bottom = append(bottom, ansi.Truncate(line, w, "…"))
		}
		room := max(3, h*45/100-len(bottom))
		if p := m.attPrev; p != nil && p.msgID == x.ID {
			switch {
			case p.loading:
				bottom = append(bottom, dim.Render("previewing "+p.name+"…"))
			case p.err != "":
				bottom = append(bottom, errSt.Render(ansi.Truncate(bg.OneLine(p.err), w, "…")))
			case p.kind == attImage:
				lines, req := m.thumbnail(p.key, w, room)
				if req != nil {
					req.p.Line, req.row = len(bottom), len(bottom)+req.n-1
					thumb = req
				}
				bottom = append(bottom, lines...)
			default:
				for _, l := range strings.Split(render.Text(p.text, w), "\n") {
					if strings.TrimSpace(l) == "" {
						continue
					}
					if len(bottom) >= h*45/100+3 {
						break
					}
					bottom = append(bottom, dim.Render(l))
				}
			}
		}
	}

	// the remaining links fill the middle
	mid := []string{}
	room := h - len(top) - len(bottom) - 1
	for n := 1; n <= len(m.links) && len(mid)+2 <= room; n++ {
		if usable(n) && !used[n] {
			mid = append(mid, ansi.Truncate(fmt.Sprintf("[%d] %s", n, m.linkTitle(n, info)), w, "…"), urlLine(n))
		}
	}
	if len(mid) > 0 {
		mid = append(mid, "")
	}

	// too tall: give up links first, then the end of the action items; keep attachments
	if over := len(top) + len(mid) + len(bottom) - h; over > 0 {
		mid = mid[:max(0, len(mid)-over)]
		if over = len(top) + len(mid) + len(bottom) - h; over > 0 {
			top = top[:max(0, len(top)-over)]
		}
	}
	lines := append(append(top, mid...), make([]string, max(0, h-len(top)-len(mid)-len(bottom)))...)
	if thumb != nil {
		off := len(lines)
		thumb.p.Line += off
		thumb.row += off
	}
	lines = append(lines, bottom...)
	if len(lines) > h { // still too tall (tiny pane): cut the end, without the thumbnail
		thumb = nil
		lines = lines[:h]
	}
	return lines, thumb
}

// thumbnail draws a registered image within w×rows: blank cells plus a sixel
// request in sixel terminals, half-blocks elsewhere.
func (m Model) thumbnail(key string, w, rows int) ([]string, *sixelReq) {
	img, token := m.images.registered(key)
	if img == nil {
		return nil, nil
	}
	b := img.Bounds()
	cw, ch := m.cellW, m.cellH
	if !m.inlineSixel() {
		return termimg.Blocks(img, w, rows), nil
	}
	lines := termimg.Blank(b.Dx(), b.Dy(), w, rows, cw, ch)
	cols := ansi.StringWidth(lines[0])
	return lines, &sixelReq{p: render.Placement{Token: token, Cols: cols, Rows: len(lines)}, top: 0, n: len(lines)}
}

// composeDetail joins the message column and the sidebar, and adds the
// sidebar thumbnail's sixel (tagged rows, like inline images).
func (m Model) composeDetail(left []string, leftW int) []string {
	sw := m.sidebarWidth()
	side, thumb := m.sidebarLines(sw, len(left))
	sep := dim.Render("│") + " "
	out := make([]string, len(left))
	for i := range left {
		l := left[i]
		pad := max(0, leftW-ansi.StringWidth(l))
		s := ""
		if i < len(side) {
			s = side[i]
		}
		out[i] = l + strings.Repeat(" ", pad) + " " + sep + s
	}
	if thumb != nil && thumb.row < len(out) {
		enc := m.images.sixelFor(*thumb, m.cellW, m.cellH)
		tag := fmt.Sprintf("\x1b_lts%d.%d\x1b\\", m.full.ID, len(enc))
		for i := max(0, thumb.row-thumb.n+1); i <= thumb.row; i++ {
			out[i] += tag
		}
		if enc != "" {
			col := 2 + leftW + sidebarGap + thumb.p.Col // 1-based, after the left border, text column and gap
			up := ""
			if thumb.n > 1 {
				up = fmt.Sprintf("\x1b[%dA", thumb.n-1)
			}
			out[thumb.row] += "\x1b7" + up + fmt.Sprintf("\x1b[%dG", col) + enc + "\x1b8"
		}
	}
	return out
}

func linkInfos(sum agent.Summary) map[int]agent.LinkInfo {
	info := map[int]agent.LinkInfo{}
	for _, l := range sum.Links {
		info[l.N] = l
	}
	return info
}

// linkTitle is the model's title for link n, else its text, else its host.
func (m Model) linkTitle(n int, info map[int]agent.LinkInfo) string {
	if t := bg.OneLine(info[n].Title); t != "" {
		return t
	}
	if t := bg.OneLine(m.links[n-1].label); t != "" && !strings.Contains(t, "://") {
		return t
	}
	u := shortURL(m.links[n-1].url)
	host, _, _ := strings.Cut(u, "/")
	return host
}
