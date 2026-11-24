// Package render turns email bodies (HTML or plain text) into terminal text.
package render

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type Opts struct {
	Width      int  // wrap width; 0 = no wrapping
	Styled     bool // ANSI bold/italic/underline/colour
	Links      bool // number links as [n] and list the URLs at the end
	PreferHTML bool // render the HTML part even when a text part exists
}

// Body picks the best part of a message and renders it.
func Body(text, htmlPart string, o Opts) string {
	switch {
	case htmlPart != "" && (o.PreferHTML || strings.TrimSpace(text) == ""):
		return HTML(htmlPart, o)
	case LooksLikeHTML(text): // some senders put HTML in the text/plain part
		return HTML(text, o)
	}
	return Text(text, o.Width)
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
	o     Opts
	links *[]string
	out   []string        // finished, wrapped lines
	line  strings.Builder // inline content not yet wrapped
	tail  rune            // last visible rune in line; 0 at line start
	pre   int
	depth int
}

// HTML renders an HTML document or fragment.
func HTML(src string, o Opts) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return Text(src, o.Width)
	}
	var links []string
	r := &renderer{o: o, links: &links}
	r.walk(doc)
	body := tidy(r.finish())
	if o.Links && len(links) > 0 {
		var b strings.Builder
		for i, l := range links {
			fmt.Fprintf(&b, "[%d] %s\n", i+1, l)
		}
		foot := Text(b.String(), o.Width)
		if o.Styled {
			foot = sgrDim + strings.ReplaceAll(foot, "\n", sgrDimOff+"\n"+sgrDim) + sgrDimOff
		}
		body += "\n\n" + foot
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
	s := &renderer{o: r.o, links: r.links, pre: r.pre, depth: r.depth + 1}
	if r.o.Width > 0 {
		s.o.Width = max(10, r.o.Width-indent)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		s.walk(c)
	}
	return strings.Split(tidy(s.finish()), "\n")
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
	if hidden(n) {
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
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" && attr(n, "width") != "1" {
			r.text(" [" + alt + "] ")
		}
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
		r.children(n)
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

func (r *renderer) link(n *html.Node) {
	href := strings.TrimSpace(attr(n, "href"))
	r.sgr(sgrUnder)
	r.children(n)
	r.sgr(sgrUnderOff)
	label := textContent(n) // not sliced from r.line: block children flush it
	if !r.o.Links || href == "" || strings.HasPrefix(href, "#") || label == "" {
		return
	}
	bare := func(s string) string {
		for _, p := range []string{"mailto:", "tel:", "https://", "http://"} {
			s = strings.TrimPrefix(s, p)
		}
		return strings.TrimRight(s, "/")
	}
	if bare(href) == bare(label) || strings.HasPrefix(href, "mailto:") && strings.Contains(label, "@") {
		return // the label already shows the target
	}
	idx := -1
	for i, l := range *r.links {
		if l == href {
			idx = i
		}
	}
	if idx < 0 {
		*r.links = append(*r.links, href)
		idx = len(*r.links) - 1
	}
	r.sgr(sgrDim)
	r.write(fmt.Sprintf("[%d]", idx+1))
	r.sgr(sgrDimOff)
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
