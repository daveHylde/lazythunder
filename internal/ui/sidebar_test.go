package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/davidhyldgaard/lazythunder/internal/agent"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
)

func TestSummarizerQueueOrder(t *testing.T) {
	s := &summarizer{cache: map[string]agent.Summary{"done": {Summary: "x"}}, failed: map[string]string{},
		queued: map[string]bool{}, seen: map[string]bool{}, wake: make(chan struct{}, 1)}
	s.enqueue(1, "a", false)
	s.enqueue(2, "b", false)
	s.enqueue(3, "c", true)    // jumps ahead
	s.enqueue(2, "b", true)    // already queued: moves to the front
	s.enqueue(4, "done", true) // cached: ignored
	s.enqueue(5, "", true)     // no Message-ID: ignored
	var got []string
	for _, j := range s.queue {
		got = append(got, j.hdr)
	}
	if strings.Join(got, ",") != "b,c,a" {
		t.Fatalf("queue %v", got)
	}
}

func TestSidebarFitsHeightAndWidth(t *testing.T) {
	m := Model{sum: &summarizer{cache: map[string]agent.Summary{"h": {Summary: strings.Repeat("ord ", 40), Actions: []string{"Gjør noe viktig i dag", "Svar Ola"}}}}}
	full := tb.FullMessage{}
	full.HeaderMessageID = "h"
	full.Parts.Attachments = []tb.Attachment{{Name: "faktura.pdf.exe", ContentType: "application/pdf", Size: 1000}, {Name: "bilde.png", ContentType: "image/png", Size: 5000}}
	m.full = &full
	for i := 0; i < 40; i++ {
		m.links = append(m.links, link{url: "https://x.no/" + strings.Repeat("a", 80), label: ""})
	}
	for _, h := range []int{8, 20, 40} {
		lines, _ := m.sidebarLines(32, h)
		if len(lines) > h {
			t.Fatalf("h=%d: %d lines", h, len(lines))
		}
		joined := strings.Join(lines, "\n")
		for _, l := range lines {
			if ansi.StringWidth(l) > 32 {
				t.Fatalf("h=%d: line too wide (%d): %q", h, ansi.StringWidth(l), l)
			}
		}
		if !strings.Contains(ansi.Strip(joined), "⚠ faktura.pdf.exe") { // attachments survive trimming
			t.Fatalf("risky attachment not flagged:\n%s", ansi.Strip(joined))
		}
	}
}
