package tb

import (
	"html"
	"regexp"
	"strings"
)

var (
	reDrop   = regexp.MustCompile(`(?is)<(head|style|script)\b.*?</(head|style|script)>`)
	reBreak  = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|tr|h[1-6]|li|blockquote)>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlanks = regexp.MustCompile(`\n[ \t]*(\n[ \t]*){2,}`)
)

// PlainText returns the text body, or the HTML body crudely converted to text.
// shortcut: regex HTML stripping loses tables/links; swap in an html-to-text lib if it matters.
func (m FullMessage) PlainText() string {
	s := m.Parts.Text
	if strings.TrimSpace(s) == "" {
		s = HTMLToText(m.Parts.HTML)
	}
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

func HTMLToText(h string) string {
	h = reDrop.ReplaceAllString(h, "")
	h = reBreak.ReplaceAllString(h, "\n")
	h = reTag.ReplaceAllString(h, "")
	h = html.UnescapeString(strings.ReplaceAll(h, "\r\n", "\n"))
	lines := strings.Split(h, "\n")
	for i, l := range lines { // drop HTML source indentation
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return reBlanks.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
}
