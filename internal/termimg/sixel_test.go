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
	s := SixelCells(img, 4, 2, 10, 20)
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
