package termimg

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/image/bmp"
)

func TestProtocolFromEnv(t *testing.T) {
	vars := []string{"LAZYTHUNDER_GRAPHICS", "KITTY_WINDOW_ID", "GHOSTTY_RESOURCES_DIR", "TERM", "TERM_PROGRAM"}
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "sixel"},
		{map[string]string{"TERM": "foot"}, "sixel"},
		{map[string]string{"TERM": "xterm-256color", "TERM_PROGRAM": "WezTerm"}, "sixel"},
		{map[string]string{"TERM": "xterm-kitty"}, "kitty"},
		{map[string]string{"KITTY_WINDOW_ID": "1"}, "kitty"},
		{map[string]string{"GHOSTTY_RESOURCES_DIR": "/usr/share/ghostty"}, "kitty"},
		{map[string]string{"TERM_PROGRAM": "ghostty"}, "kitty"},
		{map[string]string{"LAZYTHUNDER_GRAPHICS": "sixel", "TERM": "xterm-kitty"}, "sixel"},
		{map[string]string{"LAZYTHUNDER_GRAPHICS": "kitty", "TERM": "foot"}, "kitty"},
	} {
		for _, v := range vars {
			t.Setenv(v, tc.env[v])
		}
		if got := Protocol(); got != tc.want {
			t.Errorf("env %v: Protocol() = %q, want %q", tc.env, got, tc.want)
		}
	}
}

var kittyChunk = regexp.MustCompile(`\x1b_G([^;]*);([A-Za-z0-9+/=]*)\x1b\\`)

// decodeKitty reassembles a kitty transmission and checks the chunk framing.
func decodeKitty(t *testing.T, s string) image.Image {
	t.Helper()
	chunks := kittyChunk.FindAllStringSubmatch(s, -1)
	if len(chunks) == 0 || strings.Join(kittyChunkStrings(chunks), "") != s {
		t.Fatalf("not a sequence of kitty APC chunks: %q", s[:min(60, len(s))])
	}
	var data strings.Builder
	for i, c := range chunks {
		ctrl, payload := c[1], c[2]
		wantM := "m=1"
		if i == len(chunks)-1 {
			wantM = "m=0"
		}
		if i == 0 && !strings.HasPrefix(ctrl, "a=T,f=100,") || i > 0 && strings.Contains(ctrl, "a=") {
			t.Fatalf("chunk %d control %q", i, ctrl)
		}
		if !strings.HasSuffix(ctrl, wantM) || len(payload) > 4096 || len(payload) == 0 {
			t.Fatalf("chunk %d control %q len %d", i, ctrl, len(payload))
		}
		data.WriteString(payload)
	}
	raw, err := base64.StdEncoding.DecodeString(data.String())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func kittyChunkStrings(m [][]string) []string {
	out := make([]string, len(m))
	for i, c := range m {
		out[i] = c[0]
	}
	return out
}

func TestKittyEncoderRoundTrips(t *testing.T) {
	for _, size := range []int{1, 4, 120} { // 120×120 noise needs several 4096-byte chunks
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		rand.New(rand.NewSource(1)).Read(img.Pix)
		var b bytes.Buffer
		kitty(&b, img)
		got := decodeKitty(t, b.String())
		if got.Bounds() != img.Bounds() {
			t.Fatalf("size %d: decoded bounds %v", size, got.Bounds())
		}
		if size == 120 && strings.Count(b.String(), "\x1b_G") < 2 {
			t.Fatal("large image not chunked")
		}
	}
}

func TestFitNeverUpscalesAndKeepsAspect(t *testing.T) {
	for _, tc := range []struct{ w, h, maxW, maxH, ww, wh int }{
		{10, 10, 100, 100, 10, 10},
		{200, 100, 100, 100, 100, 50},
		{100, 200, 100, 100, 50, 100},
		{1000, 1, 10, 10, 10, 1}, // thin strips stay at least 1 px
		{1, 1000, 10, 10, 1, 10},
		{400, 300, 80, 1 << 30, 80, 60},
	} {
		if w, h := fit(tc.w, tc.h, tc.maxW, tc.maxH); w != tc.ww || h != tc.wh {
			t.Errorf("fit(%d,%d,%d,%d) = %d×%d, want %d×%d", tc.w, tc.h, tc.maxW, tc.maxH, w, h, tc.ww, tc.wh)
		}
	}
}

func TestShrink(t *testing.T) {
	small := solid(50, 20, color.NRGBA{1, 2, 3, 255})
	if Shrink(small, 50) != image.Image(small) {
		t.Error("image within width was copied")
	}
	if b := Shrink(solid(400, 100, color.NRGBA{A: 255}), 100).Bounds(); b.Dx() != 100 || b.Dy() != 25 {
		t.Errorf("shrunk to %v", b)
	}
}

func TestBlankMatchesImageCells(t *testing.T) {
	for _, tc := range []struct{ w, h, maxC, maxR, cw, ch, cols, rows int }{
		{80, 32, 100, 100, 8, 16, 10, 2},
		{81, 33, 100, 100, 8, 16, 11, 3}, // partial cells round up
		{8000, 1600, 50, 50, 8, 16, 50, 5},
		{1, 1, 10, 10, 8, 16, 1, 1},
	} {
		l := Blank(tc.w, tc.h, tc.maxC, tc.maxR, tc.cw, tc.ch)
		if len(l) != tc.rows || ansi.StringWidth(l[0]) != tc.cols || strings.TrimSpace(l[0]) == "" {
			t.Errorf("Blank(%v) = %d rows × %d cols, want %d×%d", tc, len(l), ansi.StringWidth(l[0]), tc.rows, tc.cols)
		}
	}
}

func TestBlocksEdgeCases(t *testing.T) {
	red := color.NRGBA{255, 0, 0, 255}
	for _, tc := range []struct {
		img        image.Image
		cols, rows int
	}{
		{image.NewNRGBA(image.Rect(0, 0, 0, 5)), 10, 10},
		{solid(5, 5, red), 0, 10},
		{solid(5, 5, red), 10, 0},
	} {
		if l := Blocks(tc.img, tc.cols, tc.rows); l != nil {
			t.Errorf("Blocks(%v, %d, %d) = %q, want nil", tc.img.Bounds(), tc.cols, tc.rows, l)
		}
	}
	// Odd height: the last row's bottom half is the terminal background.
	l := Blocks(solid(3, 3, red), 10, 10)
	if len(l) != 2 || !strings.Contains(l[1], "49m▀") || strings.Contains(l[1], "48;2") {
		t.Fatalf("odd height: %q", l)
	}
	for _, line := range l {
		if !strings.HasSuffix(line, "\x1b[0m") || ansi.StringWidth(line) != 3 {
			t.Fatalf("row not reset or wrong width: %q", line)
		}
	}
	// Non-zero origin (sub-images) render too.
	sub := solid(10, 10, red).SubImage(image.Rect(5, 5, 9, 9))
	if l := Blocks(sub, 10, 10); len(l) != 2 || ansi.StringWidth(l[0]) != 4 {
		t.Fatalf("sub-image: %q", l)
	}
}

func TestSixelCellsBounds(t *testing.T) {
	img := solid(40, 10, color.NRGBA{9, 9, 9, 255})
	for _, tc := range []struct{ cols, rows, cw, ch, top, n int }{
		{0, 2, 10, 20, 0, 1},
		{4, 2, 10, 0, 0, 1},
		{4, 2, 10, 20, -1, 1},
		{4, 2, 10, 20, 0, 0},
		{4, 2, 10, 20, 0, 3},
		{4, 2, 10, 5, 0, 1}, // one 5 px row is less than a sixel band
	} {
		if s := SixelCells(img, tc.cols, tc.rows, tc.cw, tc.ch, tc.top, tc.n); s != "" {
			t.Errorf("SixelCells(%v) = %d bytes, want empty", tc, len(s))
		}
	}
	// Height is a whole number of 6 px bands: 20 px rows -> 18 px -> 3 bands.
	s := SixelCells(img, 4, 2, 10, 20, 0, 1)
	if !strings.HasPrefix(s, "\x1bP") || !strings.HasSuffix(s, "\x1b\\") {
		t.Fatalf("not DCS: %q", s[:min(len(s), 20)])
	}
	if bands := strings.Count(s, "-") + 1; bands != 3 {
		t.Errorf("got %d sixel bands, want 3", bands)
	}
}

func TestDecodeFileFormats(t *testing.T) {
	dir := t.TempDir()
	img := solid(3, 2, color.NRGBA{10, 200, 30, 255})
	for name, enc := range map[string]func(*os.File) error{
		"a.png": func(f *os.File) error { return png.Encode(f, img) },
		"a.jpg": func(f *os.File) error { return jpeg.Encode(f, img, nil) },
		"a.gif": func(f *os.File) error { return gif.Encode(f, img, nil) },
		"a.bmp": func(f *os.File) error { return bmp.Encode(f, img) },
	} {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := enc(f); err != nil {
			t.Fatal(err)
		}
		f.Close()
		got, err := decodeFile(p)
		if err != nil || got.Bounds().Dx() != 3 || got.Bounds().Dy() != 2 {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("not an image"), 0o644)
	if _, err := decodeFile(filepath.Join(dir, "x.txt")); err == nil {
		t.Error("text decoded as image")
	}
	if _, err := decodeFile(filepath.Join(dir, "missing.png")); err == nil {
		t.Error("missing file decoded")
	}
}

func TestShowDrawsWithChosenProtocol(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pic.png")
	f, _ := os.Create(p)
	png.Encode(f, solid(16, 16, color.NRGBA{255, 0, 0, 255}))
	f.Close()

	t.Setenv("LAZYTHUNDER_GRAPHICS", "kitty")
	var b bytes.Buffer
	show(&b, p, 1, 3)
	out := b.String()
	if !strings.HasPrefix(out, "\x1b_Ga=d,d=A\x1b\\\x1b[2J") || !strings.Contains(out, "\x1b_Ga=T,f=100") || !strings.Contains(out, "image 2/3") {
		t.Errorf("kitty show: %q", out[:min(len(out), 80)])
	}

	t.Setenv("LAZYTHUNDER_GRAPHICS", "sixel")
	b.Reset()
	show(&b, p, 0, 1)
	if out := b.String(); !strings.Contains(out, "\x1bP") || strings.Contains(out, "\x1b_Ga=T") || !strings.Contains(out, "image 1/1") {
		t.Errorf("sixel show: %q", out[:min(len(out), 80)])
	}

	b.Reset()
	show(&b, filepath.Join(dir, "gone.png"), 0, 1)
	if !strings.Contains(b.String(), "cannot show") {
		t.Errorf("missing file: %q", b.String())
	}
}

func TestTruncateFooter(t *testing.T) {
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"bilde æøå.png", 6, "bilde…"},
		{"abc", 0, "…"},
	} {
		if got := truncate(tc.s, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}
