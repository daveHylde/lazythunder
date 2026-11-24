package termimg

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-sixel"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Protocol picks the graphics protocol: LAZYTHUNDER_GRAPHICS overrides,
// kitty/ghostty get the kitty protocol, everything else sixel (foot, wezterm, xterm).
func Protocol() string {
	if p := os.Getenv("LAZYTHUNDER_GRAPHICS"); p != "" {
		return p
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("GHOSTTY_RESOURCES_DIR") != "" ||
		strings.Contains(os.Getenv("TERM"), "kitty") || os.Getenv("TERM_PROGRAM") == "ghostty" {
		return "kitty"
	}
	return "sixel"
}

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

// kitty sends img as PNG in 4096-byte base64 chunks (kitty graphics protocol).
func kitty(out io.Writer, img image.Image) {
	var b bytes.Buffer
	png.Encode(&b, img)
	data := base64.StdEncoding.EncodeToString(b.Bytes())
	for first := true; len(data) > 0; first = false {
		chunk := data[:min(4096, len(data))]
		data = data[len(chunk):]
		more := 0
		if len(data) > 0 {
			more = 1
		}
		if first {
			fmt.Fprintf(out, "\x1b_Ga=T,f=100,q=2,m=%d;%s\x1b\\", more, chunk)
		} else {
			fmt.Fprintf(out, "\x1b_Gm=%d;%s\x1b\\", more, chunk)
		}
	}
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
