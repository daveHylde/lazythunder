package termimg

import (
	"image"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestSixelCellsSurvivesLayout(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	s := SixelCells(img, 4, 2, 10, 20, 0, 2)
	if SixelCells(img, 4, 2, 10, 20, 1, 2) != "" {
		t.Fatal("crop past the image accepted")
	}
	if part := SixelCells(img, 4, 2, 10, 20, 1, 1); part == "" || len(part) >= len(s) {
		t.Fatal("cropped sixel should be smaller")
	}
	if !strings.HasPrefix(s, "\x1bP") || !strings.HasSuffix(s, "\x1b\\") {
		t.Fatalf("not a DCS sixel: %q", s[:min(20, len(s))])
	}
	if w := ansi.StringWidth(s); w != 0 {
		t.Fatalf("sixel has width %d", w)
	}
	line := "abcd" + "\x1b7\x1b[1A\x1b[2G" + s + "\x1b8"
	if got := ansi.Truncate(line, 10, "…"); got != line {
		t.Fatal("Truncate altered the sixel line")
	}
	out := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Width(10).Render("title\n" + line)
	if !strings.Contains(out, s) {
		t.Fatal("lipgloss mangled the sixel payload")
	}
}

func TestAPCMarkerIsZeroWidthThroughLayout(t *testing.T) {
	tag := "\x1b_lt5.0.3.1234\x1b\\"
	line := "⠀⠀⠀" + tag
	if ansi.StringWidth(line) != 3 || ansi.Truncate(line, 10, "…") != line {
		t.Fatal("APC tag affects width or truncation")
	}
	if out := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Width(10).Render(line); !strings.Contains(out, tag) {
		t.Fatal("lipgloss dropped the APC tag")
	}
}
