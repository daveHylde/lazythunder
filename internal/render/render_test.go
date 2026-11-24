package render

import (
	"image"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func plain(src string, width int) string {
	return HTML(src, Opts{Width: width, Links: true})
}

func TestHTMLBlocksLinksAndHidden(t *testing.T) {
	src := `<html><head><style>p{color:red}</style><title>x</title></head><body>
	<div style="display:none">preheader&#847;&zwnj;</div>
	<!--[if mso]><p>outlook only</p><![endif]-->
	<h1>Ordre   mottatt</h1>
	<p>Hei <b>David</b>,<br>takk for &amp; handelen.</p>
	<table><tr><td>1x</td><td>Melkekanne</td><td>350 ml</td></tr></table>
	<p><a href="https://track.example/abc">Se ordren</a> eller <a href="https://shop.no">https://shop.no</a>
	og <a href="https://track.example/abc">igjen</a>.</p>
	<ul><li>en</li><li>to</li></ul>
	<img src="x.gif" width="1" alt="pixel"><img src="logo.png" alt="Logo">
	</body></html>`
	want := `Ordre mottatt

Hei David,
takk for & handelen.

1x  Melkekanne  350 ml

Se ordren[1] eller https://shop.no og igjen[1].

• en
• to

[Logo]

[1] https://track.example/abc`
	if got := plain(src, 60); got != want {
		t.Fatalf("got:\n%s\n---want:\n%s", got, want)
	}
}

func TestWrapAndBlockquote(t *testing.T) {
	got := plain(`<blockquote><p>one two three four five six</p></blockquote>`, 12)
	for _, l := range strings.Split(got, "\n") {
		if ansi.StringWidth(l) > 12 {
			t.Fatalf("line too wide: %q", l)
		}
		if !strings.HasPrefix(l, "│ ") {
			t.Fatalf("missing quote bar: %q", l)
		}
	}
}

func TestStyledWidthsStayInBounds(t *testing.T) {
	got := HTML(`<p><b>bold words</b> and <a href="https://x.no/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa">a link</a> with a verylongunbreakablewordthatmustbehardwrapped</p>`,
		Opts{Width: 20, Styled: true, Links: true})
	if !strings.Contains(got, "\x1b[1m") {
		t.Fatal("no bold SGR")
	}
	for _, l := range strings.Split(got, "\n") {
		if ansi.StringWidth(l) > 20 {
			t.Fatalf("line too wide (%d): %q", ansi.StringWidth(l), l)
		}
	}
}

func TestBlockInsideLinkDoesNotPanic(t *testing.T) {
	got := plain(`<span>some text before the link <a href="https://x.no/y"><div>click</div></a></span>`, 40)
	if !strings.Contains(got, "click") || !strings.HasSuffix(got, "[1] https://x.no/y") {
		t.Fatalf("got %q", got)
	}
}

func TestManyBreaksIsLinear(t *testing.T) {
	src := "<p>" + strings.Repeat("line of text<br>", 20000) + "</p>"
	start := time.Now()
	HTML(src, Opts{Width: 80, Styled: true, Links: true})
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestStylesDoNotBleedAcrossLines(t *testing.T) {
	got := HTML(`<p><u>line one<br>line two</u> end</p>`, Opts{Width: 40, Styled: true})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "\x1b[0m") || !strings.HasPrefix(lines[1], "\x1b[4m") {
		t.Fatalf("got %q", lines)
	}
}

func TestDeepNestingStaysInWidth(t *testing.T) {
	src := strings.Repeat("<blockquote>", 60) + "deep words here" + strings.Repeat("</blockquote>", 60)
	for _, l := range strings.Split(plain(src, 80), "\n") {
		if ansi.StringWidth(l) > 80 {
			t.Fatalf("line too wide (%d)", ansi.StringWidth(l))
		}
	}
}

func TestPreKeepsIndentAndBidiStripped(t *testing.T) {
	if got := plain("<pre>  indented\n    more</pre>", 40); got != "  indented\n    more" {
		t.Fatalf("pre: %q", got)
	}
	if got := Sanitize("a\u202eb"); got != "ab" {
		t.Fatalf("bidi: %q", got)
	}
}

func TestImages(t *testing.T) {
	logo := image.NewNRGBA(image.Rect(0, 0, 60, 24))
	for i := range logo.Pix {
		logo.Pix[i] = 255
	}
	var asked []string
	o := Opts{Width: 40, Styled: true, Image: func(src string) image.Image {
		asked = append(asked, src)
		if src == "cid:logo" {
			return logo
		}
		return nil
	}}
	got := HTML(`<p>Hi</p><img src="cid:logo"><img src="https://t.example/p.gif" width="1" height="1" alt="px"><img src="https://x/y.png" alt="Banner">`, o)
	lines := strings.Split(got, "\n")
	// 60×24 px at 6×12 px per cell -> 10 cols × 2 rows
	if len(lines) != 5 || lines[0] != "Hi" || lines[1] != "" || ansi.StringWidth(lines[2]) != 10 || !strings.Contains(lines[4], "[Banner]") {
		t.Fatalf("got %q", lines)
	}
	if strings.Join(asked, ",") != "cid:logo,https://x/y.png" {
		t.Fatalf("tracking pixel was requested: %v", asked)
	}
}

func TestBodyPicksPart(t *testing.T) {
	if got := Body("hei\r\n\tder", "<p>html</p>", Opts{}); got != "hei\n der" {
		t.Fatalf("text part: %q", got)
	}
	if got := Body("hei", "<p>html</p>", Opts{PreferHTML: true}); got != "html" {
		t.Fatalf("prefer html: %q", got)
	}
	if got := Body("<div>a<br>b</div><p>c</p>", "", Opts{}); got != "a\nb\n\nc" {
		t.Fatalf("html in text part: %q", got)
	}
}
