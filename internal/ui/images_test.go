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
