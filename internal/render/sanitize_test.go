package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Characters that would break width maths or spoof text are dropped.
func TestSanitizeDropsInvisibles(t *testing.T) {
	for in, want := range map[string]string{
		"a\tb\r\nc":                       "a b\nc",
		"no\u00a0break":                   "no break",
		"zero\u200bwidth\u200c\u200d":     "zerowidth",
		"soft\u00adhyphen\u2060\ufeff":    "softhyphen",
		"cgj\u034fmvs\u180e":              "cgjmvs",
		"\u2764\ufe0f":                    "\u2764",
		"\U0001F44D\U0001F3FD":            "\U0001F44D",
		"evil\u202egpj.exe":               "evilgpj.exe",
		"\u2066iso\u2069 \u202aemb\u202c": "iso emb",
		"bell\a esc\x1b[31m":              "bell esc[31m",
		"norsk æøå 日本":                    "norsk æøå 日本",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTextWrapsAndSanitizes(t *testing.T) {
	got := Text("one two three four five six seven\r\n\tindented\u200b", 10)
	for _, l := range strings.Split(got, "\n") {
		if ansi.StringWidth(l) > 10 {
			t.Errorf("line too wide: %q", l)
		}
		if strings.ContainsAny(l, "\r\t\u200b") {
			t.Errorf("unsanitized: %q", l)
		}
	}
	if !strings.Contains(got, "indented") {
		t.Errorf("lost text: %q", got)
	}
}

func TestListsNumberAndIndent(t *testing.T) {
	got := plain(`<ol><li>first</li><li>second item that wraps onto more lines</li><li>third</li></ol>
		<ul><li>a<ul><li>nested</li></ul></li></ul>`, 20)
	lines := strings.Split(got, "\n")
	if lines[0] != "1. first" || !strings.HasPrefix(lines[1], "2. second") {
		t.Fatalf("ordered list:\n%s", got)
	}
	// Continuation lines of item 2 align under its text, not the number.
	if !strings.HasPrefix(lines[2], "   ") || strings.TrimSpace(lines[2]) == "" {
		t.Errorf("continuation not indented: %q\n%s", lines[2], got)
	}
	if !strings.Contains(got, "3. third") || !strings.Contains(got, "• a") || !strings.Contains(got, "  • nested") {
		t.Errorf("list rendering:\n%s", got)
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > 20 {
			t.Errorf("line too wide: %q", l)
		}
	}
}
