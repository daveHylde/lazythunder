package termimg

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// detected is the protocol Detect found; "" until it ran (then: blocks).
var detected string

// Protocol is the graphics protocol to use: kitty, sixel or blocks (half-blocks).
// LAZYTHUNDER_GRAPHICS overrides what Detect found.
func Protocol() string {
	if p := os.Getenv("LAZYTHUNDER_GRAPHICS"); p != "" {
		return p
	}
	if detected == "" {
		return "blocks"
	}
	return detected
}

// kitty graphics query (a 1×1 RGB image, query only), then DA1: every
// terminal answers DA1, so its reply ends the wait.
const probe = "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\\x1b[c"

// Detect asks the terminal which graphics it draws and caches the answer for
// Protocol. Call it before bubbletea takes the terminal. Not a tty, or no
// answer within timeout: blocks.
func Detect(timeout time.Duration) string {
	if os.Getenv("LAZYTHUNDER_GRAPHICS") != "" {
		return Protocol()
	}
	detected = "blocks"
	in, out := int(os.Stdin.Fd()), os.Stdout
	if !term.IsTerminal(in) || !term.IsTerminal(int(out.Fd())) {
		return detected
	}
	old, err := term.MakeRaw(in)
	if err != nil {
		return detected
	}
	defer term.Restore(in, old)
	detected = query(in, out, timeout)
	return detected
}

// query writes probe to out and reads the replies from fd in until the DA1
// reply or the timeout.
func query(in int, out io.Writer, timeout time.Duration) string {
	if _, err := io.WriteString(out, probe); err != nil {
		return "blocks"
	}
	var got []byte
	buf := make([]byte, 256)
	deadline := time.Now().Add(timeout)
	for {
		p, done := parseReplies(got)
		left := time.Until(deadline)
		if done {
			return p
		}
		if left <= 0 {
			// shortcut: replies arriving after this flush still reach bubbletea as keys; only slow links hit it
			unix.IoctlSetInt(in, unix.TCFLSH, unix.TCIFLUSH)
			return p
		}
		fds := []unix.PollFd{{Fd: int32(in), Events: unix.POLLIN}}
		if n, err := unix.Poll(fds, int(left.Milliseconds())+1); err != nil || n <= 0 {
			continue // timeout or EINTR: the loop gives up on the deadline
		}
		n, err := unix.Read(in, buf)
		if err != nil || n <= 0 {
			p, _ := parseReplies(got)
			return p
		}
		got = append(got, buf[:n]...)
	}
}

var da1 = regexp.MustCompile(`\x1b\[\?([0-9;]*)c`)

// parseReplies reads the terminal's answers to probe: kitty if the graphics
// query got OK before DA1, else sixel if DA1 lists 4, else blocks. done is
// true once the DA1 reply is in.
func parseReplies(b []byte) (proto string, done bool) {
	loc := da1.FindSubmatchIndex(b)
	head := b
	if loc != nil {
		head = b[:loc[0]]
	}
	if bytes.Contains(head, []byte("\x1b_Gi=31;OK")) {
		return "kitty", loc != nil
	}
	if loc == nil {
		return "blocks", false
	}
	if slices.Contains(strings.Split(string(b[loc[2]:loc[3]]), ";"), "4") {
		return "sixel", true
	}
	return "blocks", true
}
