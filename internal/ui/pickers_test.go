package ui

import (
	"strings"
	"testing"
)

func TestIsTextual(t *testing.T) {
	yes := [][2]string{{"text/plain", ".txt"}, {"application/json", ""}, {"application/octet-stream", ".csv"}, {"image/svg+xml", ".svg"}}
	no := [][2]string{{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx"}, {"application/octet-stream", ".ml"}, {"application/zip", ".zip"}}
	for _, c := range yes {
		if !isTextual(c[0], c[1]) {
			t.Errorf("%v should be textual", c)
		}
	}
	for _, c := range no {
		if isTextual(c[0], c[1]) {
			t.Errorf("%v should not be textual", c)
		}
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd": "passwd", "..": "attachment", `a\b\c.pdf`: "c.pdf", "": "attachment"} {
		if got := safeName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", 300) + ".pdf"
	if got := safeName(long); len(got) > 150 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name: %d bytes %q", len(got), got[len(got)-8:])
	}
}
