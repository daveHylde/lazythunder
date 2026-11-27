package termimg

import (
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestKittyPlaceholder(t *testing.T) {
	s := KittyPlaceholder(0x010203, 2, 3)
	want := "\x1b[38;2;1;2;3m" +
		"\U0010EEEE\u030E\u0305" + "\U0010EEEE\u030E\u030D" + "\U0010EEEE\u030E\u030E" + "\x1b[39m"
	if s != want {
		t.Fatalf("got %q\nwant %q", s, want)
	}
	if w := ansi.StringWidth(s); w != 3 {
		t.Fatalf("width %d, want 3", w)
	}
	// wide images: every cell one column, also through lipgloss and truncation
	wide := KittyPlaceholder(1, MaxPlaceholderCells-1, MaxPlaceholderCells)
	if ansi.StringWidth(wide) != MaxPlaceholderCells || lipgloss.Width(wide) != MaxPlaceholderCells {
		t.Fatal("placeholder width is not one per cell")
	}
	if ansi.StringWidth(ansi.Truncate(wide, 10, "")) != 10 {
		t.Fatal("truncation cut a cell apart")
	}
	if KittyPlaceholder(1, MaxPlaceholderCells, 1) != "" || KittyPlaceholder(1, 0, MaxPlaceholderCells+1) != "" || KittyPlaceholder(1, 0, 0) != "" {
		t.Fatal("out-of-table placeholder accepted")
	}
	if !strings.HasPrefix(KittyPlaceholder(1, 0, 1), "\x1b[38;2;0;0;1m") {
		t.Fatal("id 1 colour")
	}
}

func TestKittyTransmitVirtualPlacement(t *testing.T) {
	s := KittyTransmit(solid(300, 100, color.NRGBA{0, 0, 255, 255}), 7, 5, 4, 10, 20)
	first := kittyChunk.FindStringSubmatch(s)
	if first == nil || !strings.HasPrefix(first[1], "a=T,f=100,q=2,U=1,i=7,c=5,r=4,") {
		t.Fatalf("control %q", first)
	}
	img := decodeKitty(t, s)
	if b := img.Bounds(); b.Dx() != 50 || b.Dy() != 80 { // canvas = the cell box
		t.Fatalf("size %v", b)
	}
	if _, _, _, a := img.At(10, 5).RGBA(); a == 0 {
		t.Fatal("image not at the top left")
	}
	if _, _, _, a := img.At(10, 60).RGBA(); a != 0 {
		t.Fatal("padding below the fitted image not transparent")
	}
	if KittyTransmit(solid(4, 4, color.NRGBA{}), 1, MaxPlaceholderCells+1, 1, 10, 20) != "" {
		t.Fatal("too many columns for placeholders accepted")
	}
	if KittyDelete(7) != "\x1b_Ga=d,d=I,i=7,q=2\x1b\\" {
		t.Fatal("delete")
	}
}
