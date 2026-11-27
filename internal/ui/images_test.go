package ui

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/tb"
	"github.com/daveHylde/lazythunder/internal/termimg"
)

// bombPNG is a tiny file whose header claims 50000×50000 pixels.
func bombPNG() []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 50000)
	binary.BigEndian.PutUint32(ihdr[4:], 50000)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk := func(typ string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return b.Bytes()
}

func TestDecodeRejectsBomb(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bomb.png")
	os.WriteFile(p, bombPNG(), 0o644)
	if _, err := decode(p, 100); err == nil {
		t.Fatal("decompression bomb accepted")
	}
}

func TestImgKeyNoCollision(t *testing.T) {
	if imgKey(5, "cid:logo") == imgKey(7, "5/cid:logo") || imgKey(5, "cid:logo") == imgKey(6, "cid:logo") {
		t.Fatal("keys collide")
	}
}

func TestDownloadRefusesLocalhost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("local server was contacted") }))
	defer srv.Close()
	if err := download(context.Background(), srv.URL+"/x.png", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("fetched from localhost")
	}
}

func TestVisibleSixelsCrop(t *testing.T) {
	t.Setenv("LAZYTHUNDER_GRAPHICS", "sixel")
	m := Model{cellW: 10, cellH: 20, full: &tb.FullMessage{}}
	m.preview.Height, m.preview.YOffset = 10, 5
	m.placements = []render.Placement{
		{Token: "a", Line: 3, Rows: 4},  // rows 3..6: top two scrolled away
		{Token: "b", Line: 8, Rows: 3},  // fully visible
		{Token: "c", Line: 13, Rows: 6}, // rows 13..18: bottom cut at 14
		{Token: "d", Line: 0, Rows: 2},  // off screen
	}
	got := map[string][3]int{}
	for _, r := range m.visibleSixels() {
		got[r.p.Token] = [3]int{r.top, r.n, r.row}
	}
	want := map[string][3]int{"a": {2, 2, 1}, "b": {0, 3, 5}, "c": {0, 2, 9}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got top,n,row %v want %v", k, got[k], w)
		}
	}
}

func TestKittyTransmitThenPlaceholders(t *testing.T) {
	t.Setenv("LAZYTHUNDER_GRAPHICS", "kitty")
	var out bytes.Buffer
	defer func(w io.Writer) { kittyOut = w }(kittyOut)
	kittyOut = &out
	s := newImageStore()
	defer s.close()
	path := filepath.Join(s.dir, "pic")
	f, _ := os.Create(path)
	png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 40, 60)))
	f.Close()
	if err := s.register("k", path); err != nil {
		t.Fatal(err)
	}
	_, token := s.registered("k")

	m := Model{cellW: 10, cellH: 20, full: &tb.FullMessage{}, images: s}
	m.preview.Height, m.preview.YOffset = 4, 2
	m.placements = []render.Placement{{Token: token, Line: 1, Col: 2, Cols: 4, Rows: 3}} // rows 1..3, row 1 scrolled away
	blank := "ab" + strings.Repeat("\u2800", 4) + "cd"
	lines := []string{blank, blank, "tail", ""}
	if got := m.withKitty(append([]string(nil), lines...)); got[0] != blank {
		t.Fatal("placeholders before the image was transmitted")
	}
	msg := m.encodeSharp(m.visibleSixels())()
	if sm, ok := msg.(sixelMsg); !ok || sm.failed {
		t.Fatalf("transmit: %#v", msg)
	}
	if !strings.Contains(out.String(), "a=T,f=100,q=2,U=1,i=1,c=4,r=3,") {
		t.Fatalf("not transmitted: %q", out.String()[:min(60, out.Len())])
	}
	if m.encodeSharp(m.visibleSixels()) != nil {
		t.Fatal("transmitted twice")
	}
	got := m.withKitty(append([]string(nil), lines...))
	for i, row := range []int{1, 2} { // screen rows 0,1 are image rows 1,2
		want := "ab" + termimg.KittyPlaceholder(1, row, 4) + "cd"
		if got[i] != want {
			t.Errorf("line %d: %q want %q", i, got[i], want)
		}
		if ansi.StringWidth(got[i]) != ansi.StringWidth(blank) {
			t.Errorf("line %d width changed", i)
		}
	}
	if got[2] != "tail" {
		t.Fatal("row below the image changed")
	}

	out.Reset()
	if cmd := s.freeKitty(); cmd == nil {
		t.Fatal("nothing to free")
	} else {
		cmd()
	}
	if out.String() != termimg.KittyDelete(1) || m.images.kittyFor(m.visibleSixels()[0], 10, 20) != 0 {
		t.Fatalf("free: %q", out.String())
	}
}
