package termimg

import (
	"io"
	"os"
	"testing"
	"time"
)

func TestParseReplies(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		done bool
	}{
		{"\x1b_Gi=31;OK\x1b\\\x1b[?62;22;52c", "kitty", true},            // ghostty
		{"\x1b_Gi=31;OK\x1b\\\x1b[?62;22c", "kitty", true},               // herdr in ghostty
		{"\x1b[?62;4;22c", "sixel", true},                                // foot
		{"\x1b[?62;22;52c", "blocks", true},                              // no graphics
		{"\x1b[?64;14c", "blocks", true},                                 // 14 is not 4
		{"\x1b[?62;22c\x1b_Gi=31;OK\x1b\\", "blocks", true},              // OK after DA1 doesn't count
		{"\x1b_Gi=31;ENOTSUPPORTED:no\x1b\\\x1b[?62;4c", "sixel", true},  // kitty refused
		{"garbage\x1b[A", "blocks", false},                               // no DA1 yet
		{"", "blocks", false},                                            // nothing (timeout)
		{"\x1b_Gi=31;OK\x1b\\", "kitty", false},                          // DA1 still coming
		{"\x1b[12;1R\x1b_Gi=31;OK\x1b\\junk\x1b[?1;2;4c", "kitty", true}, // noise around
		{"\x1b[?6", "blocks", false},                                     // DA1 cut short
	} {
		got, done := parseReplies([]byte(tc.in))
		if got != tc.want || done != tc.done {
			t.Errorf("%q: got %q,%v want %q,%v", tc.in, got, done, tc.want, tc.done)
		}
	}
}

func TestQueryReadsUntilDA1OrTimeout(t *testing.T) {
	run := func(replies ...string) (string, time.Duration) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		go func() {
			for _, s := range replies { // split replies arrive in separate reads
				w.WriteString(s)
				time.Sleep(5 * time.Millisecond)
			}
		}()
		start := time.Now()
		p := query(int(r.Fd()), io.Discard, 300*time.Millisecond)
		return p, time.Since(start)
	}
	if p, d := run("\x1b_Gi=31;OK", "\x1b\\\x1b[?62;", "22c"); p != "kitty" || d > 200*time.Millisecond {
		t.Errorf("kitty: %q after %v", p, d)
	}
	if p, _ := run("\x1b[?62;4c"); p != "sixel" {
		t.Errorf("sixel: %q", p)
	}
	if p, d := run(); p != "blocks" || d < 250*time.Millisecond {
		t.Errorf("timeout: %q after %v", p, d)
	}
}
