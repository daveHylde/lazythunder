package termimg

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"os"

	"github.com/mattn/go-sixel"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// View shows the image files full screen, one at a time, until q/esc.
// It owns the terminal while running (called via tea.ExecProcess).
func View(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	out := os.Stdout
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l") // alt screen, hide cursor
	defer fmt.Fprint(out, "\x1b_Ga=d,d=A\x1b\\\x1b[2J\x1b[?25h\x1b[?1049l")

	i, shown := 0, -1
	buf := make([]byte, 8)
	for {
		if i != shown {
			show(out, paths[i], i, len(paths))
			shown = i
		}
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return err
		}
		switch k := string(buf[:n]); k {
		case "q", "\x1b", "i", "\r":
			return nil
		case "n", "j", "l", " ", "\x1b[C", "\x1b[B":
			i = (i + 1) % len(paths)
		case "p", "k", "h", "\x1b[D", "\x1b[A":
			i = (i + len(paths) - 1) % len(paths)
		case "r", "\x0c": // redraw, e.g. after a resize
			shown = -1
		}
	}
}

func show(out io.Writer, path string, i, n int) {
	fmt.Fprint(out, "\x1b_Ga=d,d=A\x1b\\\x1b[2J\x1b[H")
	ws, _ := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	cols, rows, pxW, pxH := 80, 24, 0, 0
	if ws != nil && ws.Col > 0 && ws.Row > 0 {
		cols, rows, pxW, pxH = int(ws.Col), int(ws.Row), int(ws.Xpixel), int(ws.Ypixel)
	}
	if pxW == 0 || pxH == 0 { // terminal didn't report pixels: assume 8×16 cells
		pxW, pxH = cols*8, rows*16
	}
	footer := fmt.Sprintf("image %d/%d · n/p next/prev · r redraw · q back · %s", i+1, n, path)
	img, err := decodeFile(path)
	if err != nil {
		fmt.Fprintf(out, "cannot show %s: %v\r\n", path, err)
	} else if Protocol() == "blocks" {
		for _, l := range Blocks(img, cols, rows-2) {
			fmt.Fprint(out, l+"\r\n")
		}
	} else {
		cellH := pxH / max(1, rows)
		w, h := fit(img.Bounds().Dx(), img.Bounds().Dy(), pxW, pxH-2*cellH)
		dst := image.NewNRGBA(image.Rect(0, 0, w, h))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Over, nil)
		if Protocol() == "kitty" {
			kitty(out, dst)
		} else {
			sixel.NewEncoder(out).Encode(dst)
		}
	}
	fmt.Fprintf(out, "\x1b[%d;1H\x1b[2m%s\x1b[0m", rows, truncate(footer, cols))
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:max(0, n-1)]) + "…"
	}
	return s
}

// kitty sends img as PNG, displayed at the cursor (direct placement).
func kitty(out io.Writer, img image.Image) {
	kittyChunks(out, "a=T,f=100,q=2", img)
}

func decodeFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// SixelCells encodes img fitted (aspect kept) into cols×rows cells of
// cellW×cellH px, returning only cell rows [top, top+n) — the part of the
// image that is on screen. The height is rounded down to a 6 px sixel band so
// it never spills into the row below; transparent pixels stay transparent.
func SixelCells(img image.Image, cols, rows, cellW, cellH, top, n int) string {
	w, h := cols*cellW, rows*cellH
	if w <= 0 || h <= 0 || top < 0 || n <= 0 || top+n > rows {
		return ""
	}
	dst := boxed(img, w, h) // aspect kept inside the cell box; the rest stays transparent
	y0 := top * cellH
	y1 := y0 + (n*cellH)/6*6
	if y1 <= y0 {
		return ""
	}
	var b bytes.Buffer
	enc := sixel.NewEncoder(&b)
	enc.Transparent = true
	if enc.Encode(dst.SubImage(image.Rect(0, y0, w, y1))) != nil {
		return ""
	}
	return b.String()
}
