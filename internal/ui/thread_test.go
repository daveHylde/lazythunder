package ui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
	"time"

	"github.com/daveHylde/lazythunder/internal/gloda"
	"github.com/daveHylde/lazythunder/internal/tb"
)

func threadItem(hdr, author string, h int) tb.ThreadItem {
	return tb.ThreadItem{Hit: gloda.Hit{HeaderMessageID: hdr, Date: t0.Add(time.Duration(h) * time.Hour)}, Author: author, Subject: "Meeting"}
}

// openWithThread opens the list's message at index cur and gives it a conversation.
func openWithThread(t *testing.T, cur int, items []tb.ThreadItem) Model {
	t.Helper()
	m := loaded(t, nil)
	m.sidebar = true
	m, _ = update(m, tea.WindowSizeMsg{Width: 200, Height: 50}) // wide enough for the sidebar
	for i := range m.msgs {
		m.msgs[i].HeaderMessageID = []string{"h1", "h2", "h3", "h4"}[i]
	}
	m.mcur = cur
	full := tb.FullMessage{Message: m.msgs[cur]}
	m, _ = update(m, previewMsg{seq: m.previewSeq, m: full})
	m, _ = update(m, threadMsg{hdr: full.HeaderMessageID, items: items})
	return m
}

func TestThreadBoxListsConversationAboveActionItems(t *testing.T) {
	items := []tb.ThreadItem{threadItem("x0", `"Bob B" <bob@x.no>`, -5), threadItem("h3", "Alice <alice@x.no>", -3), threadItem("h1", "carol@x.no", -1)}
	m := openWithThread(t, 2, items) // h3 "Meeting"
	s := screen(m)
	if !strings.Contains(s, "Thread (3) · t+n opens") {
		t.Fatalf("no thread box:\n%s", s)
	}
	for _, want := range []string{" 1 ", "Bob B", " 2 ", "Alice", " 3 ", "carol@x.no"} {
		if !strings.Contains(s, want) {
			t.Fatalf("thread box lacks %q:\n%s", want, s)
		}
	}
	if m.threadIndex(m.threadItems()) != 1 {
		t.Fatalf("open message not found in its thread")
	}
	// One message is no thread: no box.
	single := openWithThread(t, 2, items[1:2])
	if strings.Contains(screen(single), "Thread (") {
		t.Fatal("single message shows a thread box")
	}
}

func TestThreadKeyJumpsWithinList(t *testing.T) {
	items := []tb.ThreadItem{threadItem("h3", "Alice", -3), threadItem("h1", "Alice", -1)}
	m := openWithThread(t, 2, items)
	m = press(t, m, "t")
	if m.picker == nil || m.picker.kind != "thread" || m.picker.cur != 0 {
		t.Fatalf("t did not open the thread picker on the open message: %+v", m.picker)
	}
	m, _ = update(m, keyMsg("2")) // 2 items: one digit is final
	if m.picker != nil || m.mcur != 0 {
		t.Fatalf("t2 should move the cursor to h1 (index 0); mcur %d picker %v", m.mcur, m.picker)
	}
}

func TestThreadItemOutsideListOpensThreadView(t *testing.T) {
	items := []tb.ThreadItem{threadItem("sent@x", "Me", -4), threadItem("h3", "Alice", -3)}
	m := openWithThread(t, 2, items)
	prev := m.view.title
	m = press(t, m, "t")
	m, _ = update(m, keyMsg("1"))
	if m.view.thread == nil || !strings.HasPrefix(m.view.title, "Thread: Meeting") || m.view.focus != "sent@x" {
		t.Fatalf("view %+v", m.view)
	}
	// The resolved conversation arrives; the cursor lands on the picked message.
	m, _ = update(m, listMsg{seq: m.listSeq, partial: true, msgs: []tb.Message{
		{ID: 30, HeaderMessageID: "h3", Date: t0.Add(-3 * time.Hour)}, {ID: 31, HeaderMessageID: "sent@x", Date: t0.Add(-4 * time.Hour)}}})
	if x, ok := m.selected(); !ok || x.ID != 31 {
		t.Fatalf("cursor on %+v, want the picked message 31", x)
	}
	m = press(t, m, "esc")
	if m.view.title != prev {
		t.Fatalf("esc went to %q, want %q", m.view.title, prev)
	}
}

func TestThreadViewFromSearchReturnsToSearch(t *testing.T) {
	items := []tb.ThreadItem{threadItem("sent@x", "Me", -4), threadItem("h3", "Alice", -3)}
	m := openWithThread(t, 2, items)
	m.view = listView{title: "Search: meeting · all mail (esc back)", query: "meeting"}
	m = press(t, m, "t")
	m, _ = update(m, keyMsg("1"))
	if m.view.thread == nil {
		t.Fatalf("no thread view: %+v", m.view)
	}
	m = press(t, m, "esc")
	if m.view.query != "meeting" {
		t.Fatalf("esc went to %+v, want the search", m.view)
	}
}

func TestThreadBoxShrinksOnShortSidebar(t *testing.T) {
	var items []tb.ThreadItem
	for i := range 20 {
		items = append(items, threadItem(fmt.Sprintf("x%d", i), "Bob", i))
	}
	items[10].HeaderMessageID = "h3"
	m := openWithThread(t, 2, items)
	sec := func(s string) string { return s }
	if got := len(m.threadLines(40, 3, sec)); got > 1+3+2 { // heading, 3 rows, older/newer markers
		t.Fatalf("%d lines for 3 rows", got)
	}
}
