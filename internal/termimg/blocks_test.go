package termimg

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func TestBlocksFitsWithoutUpscaling(t *testing.T) {
	red := color.NRGBA{255, 0, 0, 255}
	if l := Blocks(solid(600, 200, red), 100, 12); len(l) != 12 || ansi.StringWidth(l[0]) != 72 {
		t.Fatalf("banner: %d rows × %d cols", len(l), ansi.StringWidth(l[0]))
	}
	if l := Blocks(solid(10, 6, red), 100, 12); len(l) != 3 || ansi.StringWidth(l[0]) != 10 {
		t.Fatalf("icon upscaled: %d rows × %d cols", len(l), ansi.StringWidth(l[0]))
	}
	if !strings.Contains(Blocks(solid(4, 2, red), 10, 10)[0], "38;2;255;0;0;48;2;255;0;0m▀") {
		t.Fatal("expected red fg+bg half block")
	}
}

func TestBlocksTransparency(t *testing.T) {
	img := solid(2, 2, color.NRGBA{})               // fully transparent
	img.SetNRGBA(0, 1, color.NRGBA{0, 0, 255, 255}) // bottom-left opaque
	l := Blocks(img, 10, 10)
	if len(l) != 1 || !strings.Contains(l[0], "49m▄") || !strings.Contains(l[0], "\u2800") {
		t.Fatalf("got %q", l)
	}
}
