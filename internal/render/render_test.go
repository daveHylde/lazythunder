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

Se ordren[1] eller https://shop.no[2] og igjen[1].

• en
• to

[Logo]`
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
	if !strings.Contains(got, "click") || !strings.HasSuffix(got, "click\n[1]") || strings.Contains(got, "https://") {
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
	o := Opts{Width: 40, Styled: true, Image: func(src string) (image.Image, string) {
		asked = append(asked, src)
		if src == "cid:logo" {
			return logo, ""
		}
		return nil, ""
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

func TestPlacementMarkers(t *testing.T) {
	logo := image.NewNRGBA(image.Rect(0, 0, 60, 24))
	for i := range logo.Pix {
		logo.Pix[i] = 255
	}
	o := Opts{Width: 40, CellW: 6, CellH: 12, Image: func(string) (image.Image, string) { return logo, "tok1" }}
	got, ps := Placements(HTML(`<p>Hi</p><blockquote><img src="x"></blockquote>`, o))
	if strings.Contains(got, "\x1b_") {
		t.Fatalf("marker left in output: %q", got)
	}
	want := Placement{Token: "tok1", Line: 2, Col: 2, Cols: 10, Rows: 2}
	if len(ps) != 1 || ps[0] != want {
		t.Fatalf("placements %+v, want %+v", ps, want)
	}
}

func TestOnLinkNumbersSafeLinks(t *testing.T) {
	var got []string
	o := Opts{Links: true, OnLink: func(u, l string) { got = append(got, u+"|"+l) },
		SafeLink: func(u, _ string) bool { return !strings.Contains(u, "bad") }}
	out := HTML(`<a href="https://a.no/x">Se</a> <a href="https://bad.no">Bad</a> <a href="https://b.no">B</a> <a href="https://a.no/x">igjen</a> <a href="#top">top</a> <a href="mailto:x@y.no">mail</a>`, o)
	if strings.Join(got, ",") != "https://a.no/x|Se,https://b.no|B" {
		t.Fatalf("html links: %v", got)
	}
	if out != "Se[1] Bad B[2] igjen[1] top mail" {
		t.Fatalf("html text: %q", out)
	}
	got = nil
	out = Body("Les mer: https://c.no/a?b=1. Eller (https://d.no) https://bad.no", "", o)
	if strings.Join(got, ",") != "https://c.no/a?b=1|,https://d.no|" {
		t.Fatalf("text links: %v", got)
	}
	if out != "Les mer: https://c.no/a?b=1 [1]. Eller (https://d.no [2]) https://bad.no" {
		t.Fatalf("text: %q", out)
	}
}

func TestStripQuotes(t *testing.T) {
	o := Opts{StripQuotes: true}
	html := `<div>Takk, det passer fint.</div><div class="moz-cite-prefix">Den 24.11, skrev Ola:</div><blockquote type="cite">Original tekst</blockquote>`
	if got := HTML(html, o); got != "Takk, det passer fint." {
		t.Fatalf("thunderbird: %q", got)
	}
	outlook := `<p>Ja!</p><hr><div id="divRplyFwdMsg">From: Ola</div><div>Original</div>`
	if got := HTML(outlook, o); got != "Ja!" {
		t.Fatalf("outlook: %q", got)
	}
	text := "Hei,\nja.\n\nOn Tue, Ola wrote:\n> old line\n> more\n"
	if got := Body(text, "", o); got != "Hei,\nja." {
		t.Fatalf("text: %q", got)
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

func TestTextLinkParensAndQuoteCut(t *testing.T) {
	var got []string
	Body("See https://en.wikipedia.org/wiki/Foo_(bar). Or (https://x.no/a).", "", Opts{OnLink: func(u, _ string) { got = append(got, u) }})
	if strings.Join(got, " ") != "https://en.wikipedia.org/wiki/Foo_(bar) https://x.no/a" {
		t.Fatalf("links: %v", got)
	}
	sig := "Takk!\n__________\nDavid, Jobbi AS\n"
	if out := Body(sig, "", Opts{StripQuotes: true}); !strings.Contains(out, "Jobbi AS") {
		t.Fatalf("signature divider cut the reply: %q", out)
	}
	outlook := "Ja!\n________________________________\nFrom: Ola\nSent: tirsdag\nold"
	if out := Body(outlook, "", Opts{StripQuotes: true}); out != "Ja!" {
		t.Fatalf("outlook quote kept: %q", out)
	}
}

// tokImg returns an Opts.Image serving a w×h px image per src, tokened by src.
func tokImg(sizes map[string][2]int) func(string) (image.Image, string) {
	return func(src string) (image.Image, string) {
		s, ok := sizes[src]
		if !ok {
			return nil, ""
		}
		return image.NewRGBA(image.Rect(0, 0, s[0], s[1])), src
	}
}

func TestImageCellsSideBySide(t *testing.T) {
	o := Opts{Width: 60, Links: true, BlankImages: true, CellW: 6, CellH: 12,
		Image: tokImg(map[string][2]int{"logo": {120, 36}, "search": {24, 24}})}
	src := `<p>Top</p><table><tr><td><a href="https://shop.no"><img src="logo"></a></td>
		<td align="right"><a href="https://shop.no/s"><img src="search"></a></td></tr></table><p>after</p>`
	got, ps := Placements(HTML(src, o))
	lines := strings.Split(got, "\n")
	// logo: 20×3 cells at col 0; search: 4×2 cells pushed to the right edge, labels beside first rows
	want := []Placement{{Token: "logo", Line: 2, Col: 0, Cols: 20, Rows: 3}, {Token: "search", Line: 2, Col: 52, Cols: 4, Rows: 2}}
	if len(ps) != 2 || ps[0] != want[0] || ps[1] != want[1] {
		t.Fatalf("placements %+v, want %+v\n%s", ps, want, got)
	}
	if !strings.HasSuffix(lines[2], "\u2800 [2]") || !strings.Contains(lines[2], "\u2800 [1]") ||
		ansi.StringWidth(lines[2]) != 60 || lines[5] != "" || lines[6] != "after" {
		t.Fatalf("got %q", lines)
	}
}

func TestTextCellsShareWidth(t *testing.T) {
	got := plain(`<table><tr><td>`+strings.Repeat("left words ", 6)+`</td><td>`+strings.Repeat("right side ", 12)+`</td></tr></table>`, 40)
	lines := strings.Split(got, "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "left words") || !strings.Contains(lines[0], "  right side") {
		t.Fatalf("got %q", lines)
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > 40 {
			t.Fatalf("line too wide (%d): %q", ansi.StringWidth(l), l)
		}
	}
}

func TestInlineIconImage(t *testing.T) {
	o := Opts{Width: 20, Links: true, BlankImages: true, CellW: 6, CellH: 12,
		Image: tokImg(map[string][2]int{"star": {12, 12}})}
	got, ps := Placements(HTML(`<p>one two three four <img src="star"> rated <a href="https://x.no"><img src="star"></a></p>`, o))
	lines := strings.Split(got, "\n")
	want := []Placement{{Token: "star", Line: 1, Col: 0, Cols: 2, Rows: 1}, {Token: "star", Line: 1, Col: 9, Cols: 2, Rows: 1}}
	if len(ps) != 2 || ps[0] != want[0] || ps[1] != want[1] || len(lines) != 2 ||
		lines[1] != "\u2800\u2800 rated \u2800\u2800[1]" {
		t.Fatalf("placements %+v\n%q", ps, lines)
	}
}

func TestLinkedBlockImageLabelBeside(t *testing.T) {
	o := Opts{Width: 40, Links: true, BlankImages: true, CellW: 6, CellH: 12,
		Image: tokImg(map[string][2]int{"banner": {60, 36}})}
	got, ps := Placements(HTML(`<p>Hi <a href="https://x.no"><img src="banner"></a></p>`, o))
	lines := strings.Split(got, "\n")
	if len(ps) != 1 || ps[0].Line != 1 || ps[0].Col != 0 || len(lines) != 4 || lines[0] != "Hi" ||
		lines[1] != strings.Repeat("\u2800", 10)+" [1]" || lines[3] != strings.Repeat("\u2800", 10) {
		t.Fatalf("placements %+v\n%q", ps, lines)
	}
}

func TestNestedCellsStayCheap(t *testing.T) {
	src := strings.Repeat("some words here ", 2000)
	for range 30 { // each level: a 2-cell row whose wide first cell holds the level below
		src = "<table><tr><td>" + src + "</td><td>side text here</td></tr></table>"
	}
	start := time.Now()
	got := plain(src, 120)
	if d := time.Since(start); d > 3*time.Second*budgetScale {
		t.Fatalf("took %v", d)
	}
	for _, l := range strings.Split(got, "\n") {
		if ansi.StringWidth(l) > 120 {
			t.Fatalf("line too wide (%d)", ansi.StringWidth(l))
		}
	}
}
