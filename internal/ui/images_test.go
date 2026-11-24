package ui

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidhyldgaard/lazythunder/internal/render"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
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
