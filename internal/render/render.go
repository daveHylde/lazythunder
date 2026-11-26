// Package render turns email bodies (HTML or plain text) into terminal text.
package render

import (
	"fmt"
	"image"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/daveHylde/lazythunder/internal/termimg"
)

type Opts struct {
	Width      int  // wrap width; 0 = no wrapping
	Styled     bool // ANSI bold/italic/underline/colour
	Links      bool // mark numbered links as [n] in the text
	PreferHTML bool // render the HTML part even when a text part exists
	// Image returns a loaded image for an <img> src, or nil to fall back to
	// [alt]. A non-empty token marks the image's first row (see Placements).
	Image      func(src string) (image.Image, string)
	ImageRows  int // max rows per inline image (default 12)
	ImageWidth int // max columns per inline image (default Width): images may be wider than text
	// BlankImages leaves image areas empty instead of drawing half-blocks,
	// for terminals where the UI paints sharp graphics over them.
	BlankImages  bool
	CellW, CellH int // terminal cell size in px, for natural image sizing (default 6×12)
	// SafeLink decides which http(s) links get a number (nil: all of them).
	SafeLink func(url, label string) bool
	// OnLink is called once per numbered link, in number order (n = call count).
	OnLink func(url, label string)
	// StripQuotes drops quoted history (for showing just what a reply added).
	StripQuotes bool
}

// Body picks the best part of a message and renders it.
func Body(text, htmlPart string, o Opts) string {
	switch {
	case htmlPart != "" && (o.PreferHTML || strings.TrimSpace(text) == ""):
		return HTML(htmlPart, o)
	case LooksLikeHTML(text): // some senders put HTML in the text/plain part
		return HTML(text, o)
	}
	if o.StripQuotes {
		text = stripTextQuotes(text)
	}
	if o.Links || o.OnLink != nil {
		num := numberer(o)
		text = reURL.ReplaceAllStringFunc(Sanitize(text), func(u string) string {
			trimmed := strings.TrimRight(u, ".,;:!?")
			if n := num(trimmed, ""); n > 0 && o.Links {
				return trimmed + fmt.Sprintf(" [%d]", n) + u[len(trimmed):]
			}
			return u
		})
	}
	return Text(text, o.Width)
}

// numberer returns a func giving a link its number (1-based, stable per URL),
// or 0 when it is not an http(s) link that SafeLink accepts.
func numberer(o Opts) func(url, label string) int {
	nums := map[string]int{}
	return func(u, label string) int {
		if n, ok := nums[u]; ok {
			return n
		}
		l := strings.ToLower(u)
		if !strings.HasPrefix(l, "https://") && !strings.HasPrefix(l, "http://") || o.SafeLink != nil && !o.SafeLink(u, label) {
			return 0
		}
		nums[u] = len(nums) + 1
		if o.OnLink != nil {
			o.OnLink(u, label)
		}
		return nums[u]
	}
}

var (
	reURL      = regexp.MustCompile(`https?://(?:[^\s<>"'()\[\]]|\([^\s<>"'()]*\))+`) // balanced (…) as in Wikipedia URLs
	reQuoteCut = regexp.MustCompile(`(?im)^(-{3,}\s*(original message|opprinnelig melding|forwarded message|videresendt melding)|_{10,}\s*\n\s*(from|fra): .*\n(sent|date|sendt|dato): |(from|fra): .*\n(sent|sendt): )`)
)

// stripTextQuotes keeps what a plain-text reply added: drops "> " lines, the
// "... wrote:" line introducing them, and everything after an Outlook-style
// "Original Message" separator.
func stripTextQuotes(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if loc := reQuoteCut.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	lines := strings.Split(s, "\n")
	var out []string
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			continue
		}
		next := ""
		for _, n := range lines[i+1:] {
			if strings.TrimSpace(n) != "" {
				next = n
				break
			}
		}
		if strings.HasSuffix(strings.TrimSpace(l), ":") && strings.HasPrefix(strings.TrimSpace(next), ">") {
			continue // attribution line
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// Image placement markers: an APC sequence (zero width, never shown) at the
// start of an image's first row tells the UI where to draw sharp graphics.
const markerPrefix, markerEnd = "\x1b_lt;", "\x1b\\"

// Placement is an inline image's position in rendered text, in cells.
type Placement struct {
	Token      string
	Line, Col  int
	Cols, Rows int
}

// Placements strips image markers from rendered text and reports where they were.
func Placements(s string) (string, []Placement) {
	if !strings.Contains(s, markerPrefix) {
		return s, nil
	}
	lines := strings.Split(s, "\n")
	var ps []Placement
	for i, l := range lines {
		for {
			a := strings.Index(l, markerPrefix)
			if a < 0 {
				break
			}
			b := strings.Index(l[a:], markerEnd)
			if b < 0 {
				break
			}
			f := strings.Split(l[a+len(markerPrefix):a+b], ";")
			if len(f) == 3 {
				cols, _ := strconv.Atoi(f[1])
				rows, _ := strconv.Atoi(f[2])
				ps = append(ps, Placement{Token: f[0], Line: i, Col: ansi.StringWidth(l[:a]), Cols: cols, Rows: rows})
			}
			l = l[:a] + l[a+b+len(markerEnd):]
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n"), ps
}

var reTag = regexp.MustCompile(`(?i)<(br|p|div|table|td|span|b|a|font|html|body)\b[^>]*>`)

// LooksLikeHTML reports whether s contains several common HTML tags.
func LooksLikeHTML(s string) bool { return len(reTag.FindAllStringIndex(s, 3)) >= 3 }

// Text cleans and wraps a plain-text body.
func Text(s string, width int) string {
	var out []string
	for _, l := range strings.Split(Sanitize(s), "\n") {
		out = append(out, wrap(strings.TrimRight(l, " "), width))
	}
	return tidy(strings.Join(out, "\n"))
}

// Sanitize expands tabs, drops \r, control and zero-width characters, so
// terminal width maths stays right (a stray tab or \r breaks the layout).
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r == '\u00a0':
			return ' '
		case unicode.IsControl(r), isInvisible(r):
			return -1
		}
		return r
	}, s)
}

func isInvisible(r rune) bool {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u034f', '\u00ad', '\u180e':
		return true
	case '\ufe0f': // emoji presentation: terminals disagree on its width
		return true
	}
	if r >= 0x1f3fb && r <= 0x1f3ff { // skin tones: wcswidth terminals (foot) draw them as an extra 2 cells
		return true
	}
	return r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069' // bidi overrides can spoof URLs
}

func wrap(s string, width int) string {
	if width <= 0 || ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Wrap(s, width, "")
}

var reBlank = regexp.MustCompile(`\n{3,}`)

// tidy trims trailing spaces and collapses runs of blank lines.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(ansi.Strip(l)) == "" {
			lines[i] = ""
		} else {
			lines[i] = strings.TrimRight(l, " ")
		}
	}
	return strings.Trim(reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"), "\n")
}

// ---- HTML ----

const (
	sgrBold, sgrBoldOff     = "\x1b[1m", "\x1b[22m"
	sgrItalic, sgrItalicOff = "\x1b[3m", "\x1b[23m"
	sgrUnder, sgrUnderOff   = "\x1b[4m", "\x1b[24m"
	sgrHead, sgrHeadOff     = "\x1b[1;34m", "\x1b[22;39m"
	sgrDim, sgrDimOff       = "\x1b[90m", "\x1b[39m" // colour, so it doesn't cancel bold
	maxDepth                = 8                      // deeper quotes/lists are flattened
)

type renderer struct {
	o       Opts
	num     func(url, label string) int // numbers safe links (shared by nested renderers)
	out     []string                    // finished, wrapped lines
	line    strings.Builder             // inline content not yet wrapped
	tail    rune                        // last visible rune in line; 0 at line start
	pre     int
	depth   int
	stopped bool // StripQuotes reached the quoted part
	imgAt   int  // index in out of the last block image's first row (-1: none)
}

// isQuote spots quoted reply history from Thunderbird, Gmail, Apple Mail and Outlook.
func isQuote(n *html.Node) bool {
	if n.DataAtom == atom.Blockquote && strings.EqualFold(attr(n, "type"), "cite") {
		return true
	}
	class := " " + attr(n, "class") + " "
	for _, c := range []string{" moz-cite-prefix ", " gmail_quote ", " gmail_attr ", " gmail_quote_container ", " yahoo_quoted "} {
		if strings.Contains(class, c) {
			return true
		}
	}
	id := attr(n, "id")
	return id == "divRplyFwdMsg" || id == "appendonsend"
}

// HTML renders an HTML document or fragment.
func HTML(src string, o Opts) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return Text(src, o.Width)
	}
	r := &renderer{o: o, num: numberer(o)}
	r.walk(doc)
	body := tidy(r.finish())
	if o.StripQuotes { // the separator rule before a stripped quote
		body = strings.TrimRight(strings.TrimRight(body, "─"), "\n")
	}
	return body
}

func (r *renderer) sgr(on string) {
	if r.o.Styled {
		r.line.WriteString(on)
	}
}

// write appends visible text and remembers its last rune.
func (r *renderer) write(s string) {
	if s == "" {
		return
	}
	r.line.WriteString(s)
	r.tail, _ = utf8.DecodeLastRuneInString(s)
}

func (r *renderer) atBreak() bool { return r.tail == 0 || r.tail == ' ' || r.tail == '\n' }

// flush wraps the pending inline text into finished lines.
func (r *renderer) flush() {
	s := r.line.String()
	r.line.Reset()
	r.tail = 0
	if strings.TrimSpace(ansi.Strip(s)) == "" {
		return
	}
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if r.pre > 0 {
			l = strings.TrimRight(l, " ")
		} else {
			l = strings.TrimSpace(l)
		}
		lines = append(lines, strings.Split(wrap(l, r.o.Width), "\n")...)
	}
	r.out = append(r.out, balance(lines)...)
}

// balance makes every line self-contained: styles still open at a line's end
// are reset there and re-opened on the next line, so nothing bleeds into the
// panel border or gets lost after a wrap.
func balance(lines []string) []string {
	var bold, italic, under bool
	color := ""
	reopen := func() string {
		var p []string
		if bold {
			p = append(p, "1")
		}
		if italic {
			p = append(p, "3")
		}
		if under {
			p = append(p, "4")
		}
		if color != "" {
			p = append(p, color)
		}
		if len(p) == 0 {
			return ""
		}
		return "\x1b[" + strings.Join(p, ";") + "m"
	}
	for i, l := range lines {
		prefix := reopen()
		for _, m := range reSGR.FindAllStringSubmatch(l, -1) {
			for _, p := range strings.Split(m[1], ";") {
				switch p {
				case "0", "":
					bold, italic, under, color = false, false, false, ""
				case "1":
					bold = true
				case "22":
					bold = false
				case "3":
					italic = true
				case "23":
					italic = false
				case "4":
					under = true
				case "24":
					under = false
				case "39":
					color = ""
				default:
					color = p
				}
			}
		}
		suffix := ""
		if reopen() != "" {
			suffix = "\x1b[0m"
		}
		lines[i] = prefix + l + suffix
	}
	return lines
}

var reSGR = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

func (r *renderer) blank() {
	r.flush()
	if n := len(r.out); n > 0 && r.out[n-1] != "" {
		r.out = append(r.out, "")
	}
}

func (r *renderer) finish() string {
	r.flush()
	return strings.Join(r.out, "\n")
}

// sub renders n's children at a narrower width, for prefixed blocks.
func (r *renderer) sub(n *html.Node, indent int) []string {
	s := &renderer{o: r.o, num: r.num, pre: r.pre, depth: r.depth + 1}
	if r.o.Width > 0 {
		s.o.Width = max(10, r.o.Width-indent)
	}
	if r.o.ImageWidth > 0 {
		s.o.ImageWidth = max(10, r.o.ImageWidth-indent)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		s.walk(c)
	}
	r.stopped = r.stopped || s.stopped
	return strings.Split(tidy(s.finish()), "\n")
}

// cell renders a table cell as a block of lines at the given width (0: unlimited).
func (r *renderer) cell(n *html.Node, width int) []string {
	s := &renderer{o: r.o, num: r.num, pre: r.pre, depth: r.depth + 1}
	s.o.Width = width
	if width > 0 {
		s.o.ImageWidth = width // images stay inside their column
	}
	if n.DataAtom == atom.Th {
		s.sgr(sgrBold)
	}
	s.children(n)
	r.stopped = r.stopped || s.stopped
	return strings.Split(tidy(s.finish()), "\n")
}

func blockWidth(lines []string) int {
	w := 0
	for _, l := range lines {
		w = max(w, ansi.StringWidth(l))
	}
	return w
}

func align(n *html.Node) string {
	a := strings.ToLower(strings.TrimSpace(attr(n, "align")))
	st := strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", "")
	for _, v := range []string{"right", "center"} {
		if a == v || strings.Contains(st, "text-align:"+v) {
			return v
		}
	}
	return ""
}

func hasImg(n *html.Node) bool {
	if n.DataAtom == atom.Img {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasImg(c) {
			return true
		}
	}
	return false
}

const colGap, minCol = 2, 10

// row lays a table row's cells out side by side, as the sender's layout does.
// It reports false (and renders nothing) when the row should flow inline.
// shortcut: columns are sized per row, so cells of different rows don't line up; colspan/rowspan ignored.
func (r *renderer) row(n *html.Node) bool {
	if r.depth >= maxDepth {
		return false
	}
	var cells []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.DataAtom == atom.Td || c.DataAtom == atom.Th) && !hidden(c) &&
			(textContent(c) != "" || hasImg(c)) {
			cells = append(cells, c)
		}
	}
	avail := r.o.Width - colGap*(len(cells)-1)
	if len(cells) < 2 || r.o.Width > 0 && avail < minCol*len(cells) {
		return false
	}
	// natural widths first; only cells that don't fit are rendered again
	// shortcut: nested overflowing rows can re-render up to 2^maxDepth times; cache by (cell, width) if real mail gets slow.
	blocks := make([][]string, len(cells))
	widths := make([]int, len(cells))
	total := 0
	for i, c := range cells {
		blocks[i] = r.cell(c, r.o.Width)
		widths[i] = blockWidth(blocks[i])
		total += widths[i]
	}
	if r.o.Width > 0 && total > avail {
		alloc := make([]int, len(cells))
		sum := 0
		for i, w := range widths {
			alloc[i] = max(minCol, w*avail/total)
			sum += alloc[i]
		}
		for sum > avail { // minimums overshot: take from the widest
			w := 0
			for i := range alloc {
				if alloc[i] > alloc[w] {
					w = i
				}
			}
			d := min(sum-avail, alloc[w]-minCol)
			alloc[w] -= d
			sum -= d
		}
		total = 0
		for i, c := range cells {
			if widths[i] > alloc[i] {
				blocks[i] = r.cell(c, alloc[i])
				widths[i] = blockWidth(blocks[i])
			}
			total += widths[i]
		}
	}
	aligns := make([]string, len(cells))
	for i, c := range cells {
		aligns[i] = align(c)
	}
	if spare := avail - total; r.o.Width > 0 && spare > 0 {
		for i := range cells { // push the first right-aligned cell to the right edge
			if aligns[i] == "right" {
				widths[i] += spare
				break
			}
		}
	}
	rows := 0
	for _, b := range blocks {
		rows = max(rows, len(b))
	}
	r.flush()
	for j := range rows {
		var sb strings.Builder
		for i, b := range blocks {
			if i > 0 {
				sb.WriteString(strings.Repeat(" ", colGap))
			}
			l := ""
			if j < len(b) {
				l = b[j]
			}
			pad := widths[i] - ansi.StringWidth(l)
			left := 0
			switch aligns[i] {
			case "right":
				left = pad
			case "center":
				left = pad / 2
			}
			sb.WriteString(strings.Repeat(" ", left) + l + strings.Repeat(" ", pad-left))
		}
		r.out = append(r.out, strings.TrimRight(sb.String(), " "))
	}
	return true
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hidden(n *html.Node) bool {
	st := strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", "")
	return strings.Contains(st, "display:none") || strings.Contains(st, "visibility:hidden") ||
		strings.Contains(st, "max-height:0") || attr(n, "hidden") != "" || attr(n, "aria-hidden") == "true"
}

var reSpace = regexp.MustCompile(`\s+`)

func (r *renderer) text(s string) {
	s = Sanitize(s)
	if r.pre > 0 {
		r.write(s)
		return
	}
	s = reSpace.ReplaceAllString(s, " ")
	if strings.HasPrefix(s, " ") && r.atBreak() {
		s = s[1:]
	}
	r.write(s)
}

// textContent is the collapsed visible text under n (for link labels).
func textContent(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return strings.TrimSpace(reSpace.ReplaceAllString(Sanitize(b.String()), " "))
}

func (r *renderer) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.walk(c)
	}
}

func (r *renderer) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		r.text(n.Data)
		return
	case html.DocumentNode:
		r.children(n)
		return
	case html.ElementNode:
	default:
		return // comments (incl. Outlook conditionals), doctype
	}
	if hidden(n) || r.stopped {
		return
	}
	if r.o.StripQuotes && isQuote(n) {
		if attr(n, "id") == "divRplyFwdMsg" || attr(n, "id") == "appendonsend" {
			r.stopped = true // Outlook: everything after this is the quoted original
		}
		return
	}
	switch n.DataAtom {
	case atom.Head, atom.Style, atom.Script, atom.Title, atom.Template, atom.Noscript, atom.Svg:
		return
	case atom.Br:
		r.write("\n")
	case atom.Hr:
		r.blank()
		r.out = append(r.out, strings.Repeat("─", max(3, min(r.o.Width, 40))), "")
	case atom.Img:
		r.img(n)
	case atom.B, atom.Strong:
		r.sgr(sgrBold)
		r.children(n)
		r.sgr(sgrBoldOff)
	case atom.I, atom.Em:
		r.sgr(sgrItalic)
		r.children(n)
		r.sgr(sgrItalicOff)
	case atom.U:
		r.sgr(sgrUnder)
		r.children(n)
		r.sgr(sgrUnderOff)
	case atom.A:
		r.link(n)
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		r.blank()
		r.sgr(sgrHead)
		r.children(n)
		r.sgr(sgrHeadOff)
		r.blank()
	case atom.Pre:
		r.blank()
		r.pre++
		r.children(n)
		r.flush() // while still in pre mode, so indentation is kept
		r.pre--
		r.blank()
	case atom.Blockquote:
		r.blank()
		if r.depth >= maxDepth {
			r.children(n)
			r.blank()
			return
		}
		bar := "│ "
		if r.o.Styled {
			bar = sgrDim + "│" + sgrDimOff + " "
		}
		for _, l := range r.sub(n, 2) {
			r.out = append(r.out, bar+l)
		}
		r.blank()
	case atom.Ul, atom.Ol:
		r.flush()
		if r.depth >= maxDepth {
			r.children(n)
		} else {
			r.list(n)
		}
		r.blank()
	case atom.Li: // only reached past maxDepth or outside a list
		r.flush()
		r.write("• ")
		r.children(n)
		r.flush()
	case atom.Td, atom.Th:
		// layout tables: cells on one line, separated by spaces
		if !r.atBreak() {
			r.write("  ")
		}
		if n.DataAtom == atom.Th {
			r.sgr(sgrBold)
			r.children(n)
			r.sgr(sgrBoldOff)
		} else {
			r.children(n)
		}
	case atom.Tr:
		r.flush()
		if !r.row(n) {
			r.children(n)
		}
		r.flush()
	case atom.P, atom.Table, atom.Center:
		r.blank()
		r.children(n)
		r.blank()
	case atom.Div, atom.Section, atom.Article, atom.Header, atom.Footer, atom.Main, atom.Nav, atom.Tbody, atom.Thead, atom.Tfoot, atom.Dl, atom.Dt, atom.Dd, atom.Figure, atom.Figcaption, atom.Address:
		r.flush()
		r.children(n)
		r.flush()
	default:
		r.children(n)
	}
}

func (r *renderer) img(n *html.Node) {
	w, _ := strconv.Atoi(strings.TrimSuffix(attr(n, "width"), "px"))
	h, _ := strconv.Atoi(strings.TrimSuffix(attr(n, "height"), "px"))
	if w > 0 && w <= 2 || h > 0 && h <= 2 {
		return // tracking pixel
	}
	if src := strings.TrimSpace(attr(n, "src")); r.o.Image != nil && src != "" {
		if img, token := r.o.Image(src); img != nil {
			b := img.Bounds()
			if b.Dx() <= 2 || b.Dy() <= 2 {
				return
			}
			if w == 0 || h == 0 { // size from the image when the tag doesn't say
				w, h = b.Dx(), b.Dy()
			}
			width, rows := r.o.Width, r.o.ImageRows
			if r.o.ImageWidth > 0 {
				width = r.o.ImageWidth
			}
			if width <= 0 {
				width = 80
			}
			if rows <= 0 {
				rows = 12
			}
			cw, ch := r.o.CellW, r.o.CellH
			if cw <= 0 || ch <= 0 {
				cw, ch = 6, 12
			}
			// natural size: one image pixel per screen pixel, capped to the pane
			var lines []string
			if r.o.BlankImages && token != "" { // no token: the UI can't paint it, so draw blocks
				lines = termimg.Blank(w, h, width, rows, cw, ch)
			} else {
				lines = termimg.Blocks(img, min(width, (w+cw-1)/cw), min(rows, (h+ch-1)/ch))
			}
			if len(lines) == 0 {
				return
			}
			if token != "" {
				lines[0] = fmt.Sprintf("%s%s;%d;%d%s", markerPrefix, token, ansi.StringWidth(lines[0]), len(lines), markerEnd) + lines[0]
			}
			if len(lines) == 1 { // icon-sized: flows with the text
				r.write(lines[0])
				return
			}
			r.flush()
			r.imgAt = len(r.out)
			r.out = append(r.out, lines...)
			return
		}
	}
	if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
		r.text(" [" + alt + "] ")
	}
}

func (r *renderer) link(n *html.Node) {
	href := strings.TrimSpace(attr(n, "href"))
	r.imgAt = -1
	r.sgr(sgrUnder)
	r.children(n)
	r.sgr(sgrUnderOff)
	label := textContent(n) // not sliced from r.line: block children flush it
	idx := r.num(Sanitize(href), label)
	if !r.o.Links || idx == 0 {
		return
	}
	tag := fmt.Sprintf("[%d]", idx)
	if r.o.Styled {
		tag = sgrDim + tag + sgrDimOff
	}
	// an image-only link: put the number beside the image's first row, not under it
	if i := r.imgAt; label == "" && i >= 0 && strings.TrimSpace(ansi.Strip(r.line.String())) == "" &&
		(r.o.Width <= 0 || ansi.StringWidth(r.out[i])+1+ansi.StringWidth(tag) <= r.o.Width) {
		r.out[i] += " " + tag
		return
	}
	r.write(tag)
}

func (r *renderer) list(n *html.Node) {
	i := 0
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.DataAtom != atom.Li {
			continue
		}
		i++
		bullet := "• "
		if n.DataAtom == atom.Ol {
			bullet = fmt.Sprintf("%d. ", i)
		}
		pad := strings.Repeat(" ", ansi.StringWidth(bullet))
		for j, l := range r.sub(c, len(pad)) {
			if j == 0 {
				r.out = append(r.out, bullet+l)
			} else if l != "" {
				r.out = append(r.out, pad+l)
			}
		}
	}
}
