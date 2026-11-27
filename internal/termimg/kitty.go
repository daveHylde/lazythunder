package termimg

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// MaxPlaceholderCells is the largest row/column count a kitty Unicode
// placeholder can address (the diacritics table's length).
var MaxPlaceholderCells = len(diacritics)

// kittyChunks sends img as PNG in 4096-byte base64 chunks (kitty graphics
// protocol), ctrl being the first chunk's control keys.
func kittyChunks(out io.Writer, ctrl string, img image.Image) error {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return err
	}
	data := base64.StdEncoding.EncodeToString(b.Bytes())
	for first := true; len(data) > 0; first = false {
		chunk := data[:min(4096, len(data))]
		data = data[len(chunk):]
		more := 0
		if len(data) > 0 {
			more = 1
		}
		if first {
			fmt.Fprintf(out, "\x1b_G%s,m=%d;%s\x1b\\", ctrl, more, chunk)
		} else {
			fmt.Fprintf(out, "\x1b_Gm=%d;%s\x1b\\", more, chunk)
		}
	}
	return nil
}

// KittyTransmit encodes img fitted (aspect kept, top-left, transparent rest)
// into cols×rows cells of cellW×cellH px as image id with a virtual placement
// of cols×rows cells, which KittyPlaceholder cells then show.
func KittyTransmit(img image.Image, id uint32, cols, rows, cellW, cellH int) string {
	if cols <= 0 || rows <= 0 || cellW <= 0 || cellH <= 0 || cols > MaxPlaceholderCells || rows > MaxPlaceholderCells {
		return ""
	}
	var b strings.Builder
	if kittyChunks(&b, fmt.Sprintf("a=T,f=100,q=2,U=1,i=%d,c=%d,r=%d", id, cols, rows), boxed(img, cols*cellW, rows*cellH)) != nil {
		return ""
	}
	return b.String()
}

// KittyDelete frees image id (placements and data) in the terminal.
func KittyDelete(id uint32) string {
	return fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
}

// KittyPlaceholder is row `row` of image id's virtual placement, cols cells
// wide: U+10EEEE cells carrying row and column diacritics, the id in the
// foreground colour. Each cell is one column wide, so it lays out like text.
func KittyPlaceholder(id uint32, row, cols int) string {
	if row < 0 || row >= len(diacritics) || cols <= 0 || cols > len(diacritics) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm", id>>16&0xff, id>>8&0xff, id&0xff)
	for c := range cols {
		b.WriteRune(0x10EEEE)
		b.WriteRune(diacritics[row])
		b.WriteRune(diacritics[c])
	}
	b.WriteString("\x1b[39m")
	return b.String()
}

// boxed scales img to fit w×h (aspect kept) at the top left of a transparent
// w×h canvas.
func boxed(img image.Image, w, h int) *image.NRGBA {
	iw, ih := img.Bounds().Dx(), img.Bounds().Dy()
	fw, fh := w, ih*w/max(1, iw)
	if fh > h {
		fw, fh = iw*h/max(1, ih), h
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, image.Rect(0, 0, max(1, fw), max(1, fh)), img, img.Bounds(), xdraw.Src, nil)
	return dst
}
