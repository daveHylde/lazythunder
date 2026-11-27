// Package termimg draws images in a terminal: as truecolor half-block text
// (works everywhere, embeds in any text view), as kitty Unicode placeholders
// or sixel inline, or full resolution in a full-screen viewer.
package termimg

import (
	"fmt"
	"image"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// Blocks draws img with "▀" cells (two pixels per cell, top = foreground,
// bottom = background), fitting maxCols × maxRows without upscaling.
// Transparent pixels keep the terminal background.
func Blocks(img image.Image, maxCols, maxRows int) []string {
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 || maxCols <= 0 || maxRows <= 0 {
		return nil
	}
	w, h := fit(b.Dx(), b.Dy(), maxCols, maxRows*2)
	dst := image.NewNRGBA(image.Rect(0, 0, w, h+h%2))
	xdraw.ApproxBiLinear.Scale(dst, image.Rect(0, 0, w, h), img, b, xdraw.Src, nil)

	lines := make([]string, 0, (h+1)/2)
	var sb strings.Builder
	for y := 0; y < h; y += 2 {
		sb.Reset()
		for x := 0; x < w; x++ {
			top, bot := dst.NRGBAAt(x, y), dst.NRGBAAt(x, y+1)
			topOn, botOn := top.A >= 128, bot.A >= 128 && y+1 < h
			switch {
			case topOn && botOn:
				fmt.Fprintf(&sb, "\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm▀", top.R, top.G, top.B, bot.R, bot.G, bot.B)
			case topOn:
				fmt.Fprintf(&sb, "\x1b[38;2;%d;%d;%d;49m▀", top.R, top.G, top.B)
			case botOn:
				fmt.Fprintf(&sb, "\x1b[38;2;%d;%d;%d;49m▄", bot.R, bot.G, bot.B)
			default: // braille blank: looks empty but isn't whitespace, so text cleanup keeps the row
				sb.WriteString("\x1b[39;49m\u2800")
			}
		}
		sb.WriteString("\x1b[0m")
		lines = append(lines, sb.String())
	}
	return lines
}

// Blank returns empty rows (braille blanks, so text cleanup keeps them) the
// size a w×h px image takes at cw×ch px per cell, fitted into maxCols×maxRows.
func Blank(w, h, maxCols, maxRows, cw, ch int) []string {
	pw, ph := fit(w, h, maxCols*cw, maxRows*ch)
	cols, rows := max(1, (pw+cw-1)/cw), max(1, (ph+ch-1)/ch)
	line := strings.Repeat("\u2800", cols)
	out := make([]string, rows)
	for i := range out {
		out[i] = line
	}
	return out
}

// fit scales w×h down (never up) to fit within maxW×maxH, keeping aspect.
func fit(w, h, maxW, maxH int) (int, int) {
	if w > maxW {
		h, w = max(1, h*maxW/w), maxW
	}
	if h > maxH {
		w, h = max(1, w*maxH/h), maxH
	}
	return w, h
}

// Shrink returns img scaled so its width is at most maxW (for caching).
func Shrink(img image.Image, maxW int) image.Image {
	b := img.Bounds()
	if b.Dx() <= maxW {
		return img
	}
	w, h := fit(b.Dx(), b.Dy(), maxW, 1<<30)
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	return dst
}
