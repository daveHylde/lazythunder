package ui

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/tb"
)

func TestStartupNotConnected(t *testing.T) {
	m := testModel(t, nil)
	m.ready = false
	m, _ = update(m, readyMsg{err: errors.New("hub down")})
	if m.ready || m.status != "not connected (F5 to retry, q to quit)" {
		t.Fatalf("ready=%v status=%q", m.ready, m.status)
	}
	if got := statusLine(m); !strings.Contains(got, "hub down") || !strings.Contains(got, "F5 retry · q quit") {
		t.Fatalf("status line %q", got)
	}
	for _, s := range []string{"j", "/", "a", "?"} { // nothing works before Thunderbird answers
		if m2, cmd := pressCmd(t, m, s); cmd != nil || m2.mode != mNormal {
			t.Errorf("%q acted while not connected", s)
		}
	}
	for _, s := range []string{"q", "ctrl+c"} {
		if _, cmd := pressCmd(t, m, s); cmd == nil || !reflect.DeepEqual(run(t, cmd), tea.QuitMsg{}) {
			t.Errorf("%q does not quit", s)
		}
	}
}

func TestStartupConnected(t *testing.T) {
	m := testModel(t, nil)
	m.ready = false
	m, cmd := update(m, readyMsg{})
	if !m.ready || m.status != "connected" || cmd == nil {
		t.Fatalf("ready=%v status=%q cmd=%v", m.ready, m.status, cmd != nil)
	}
}

func TestTerminalTooSmall(t *testing.T) {
	m := testModel(t, nil)
	m, _ = update(m, tea.WindowSizeMsg{Width: 40, Height: 10})
	if got := screen(m); !strings.Contains(got, "terminal too small") {
		t.Fatalf("view %q", got)
	}
}

func TestFirstFolderLoadOpensAllInboxes(t *testing.T) {
	m := testModel(t, nil)
	m, _ = update(m, foldersMsg{folders: testFolders(3, 2), accounts: testAccounts()})
	if !reflect.DeepEqual(m.acctOrder, []string{"a1", "a2"}) {
		t.Fatalf("tabs %v: accounts without folders get no tab", m.acctOrder)
	}
	for _, f := range m.folders {
		if f.Depth == 0 {
			t.Fatalf("account root %q listed as a folder", f.ID)
		}
	}
	if m.view.title != "All inboxes" || !reflect.DeepEqual(m.view.folderIDs, []string{"a1://INBOX", "a2://INBOX"}) || m.view.accountID != "" {
		t.Fatalf("view %+v", m.view)
	}
	if m.status != "loading All inboxes…" {
		t.Fatalf("status %q", m.status)
	}
	tabs := strings.Split(screen(m), "\n")[0]
	for _, want := range []string{"1 All inboxes (5)", "2 Work (3)", "3 Home (2)"} {
		if !strings.Contains(tabs, want) {
			t.Errorf("tab bar %q lacks %q", tabs, want)
		}
	}
	if strings.Contains(tabs, "Empty") {
		t.Errorf("tab bar %q shows an account without folders", tabs)
	}
}

func TestTabSwitching(t *testing.T) {
	m := loaded(t, nil)
	steps := []struct {
		key, title, acct string
		tab              int
	}{
		{"]", "Work › Inbox", "a1", 1},
		{"]", "Home › Inbox", "a2", 2},
		{"]", "All inboxes", "", 0}, // wraps
		{"[", "Home › Inbox", "a2", 2},
		{"alt+2", "Work › Inbox", "a1", 1},
		{"alt+1", "All inboxes", "", 0},
		{"alt+9", "All inboxes", "", 0}, // no such tab: nothing happens
	}
	for _, s := range steps {
		seq := m.listSeq
		m.marked = map[int]bool{1: true}
		m = press(t, m, s.key)
		if m.tab != s.tab || m.view.title != s.title || m.view.accountID != s.acct {
			t.Fatalf("after %q: tab %d view %+v", s.key, m.tab, m.view)
		}
		if s.key == "alt+9" {
			if m.listSeq != seq {
				t.Fatalf("alt+9 reloaded the list")
			}
			continue
		}
		if m.listSeq != seq+1 || m.msgs != nil || m.marked != nil {
			t.Fatalf("after %q: list not reset (seq %d→%d, %d msgs, marked %v)", s.key, seq, m.listSeq, len(m.msgs), m.marked)
		}
	}
}

func TestFolderRefreshUpdatesCountsKeepsView(t *testing.T) {
	m := press(t, loaded(t, nil), "alt+3") // Home
	m, _ = update(m, foldersMsg{folders: testFolders(0, 7), accounts: testAccounts()})
	if m.view.title != "Home › Inbox" || m.tab != 2 {
		t.Fatalf("refresh changed the view: tab %d %+v", m.tab, m.view)
	}
	tabs := strings.Split(screen(m), "\n")[0]
	if !strings.Contains(tabs, "3 Home (7)") || !strings.Contains(tabs, "1 All inboxes (7)") || strings.Contains(tabs, "Work (") {
		t.Fatalf("tab bar %q", tabs)
	}
	// Home lost its folders: the tab index stays in range.
	m, _ = update(m, foldersMsg{folders: testFolders(0, 0)[:3], accounts: testAccounts()})
	if m.tab != 1 {
		t.Fatalf("tab %d after an account vanished", m.tab)
	}
	m, _ = update(m, foldersMsg{err: errors.New("folders failed")})
	if m.err != "folders failed" {
		t.Fatalf("err %q", m.err)
	}
}

func TestListLoad(t *testing.T) {
	m := loaded(t, nil)
	if !reflect.DeepEqual(ids(m.msgs), []int{1, 2, 3, 4}) || m.mcur != 0 || m.status != "4 messages" {
		t.Fatalf("msgs %v cur %d status %q", ids(m.msgs), m.mcur, m.status)
	}
	view := screen(m)
	for _, want := range []string{"[1] All inboxes · l folders", "Hello", "Work", "Home", "●"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q", want)
		}
	}

	// a reply to an older list request is dropped
	m2, cmd := update(m, listMsg{seq: m.listSeq - 1, msgs: []tb.Message{testMsg(9, "a1", "old", true, 1)}})
	if cmd != nil || !reflect.DeepEqual(ids(m2.msgs), []int{1, 2, 3, 4}) {
		t.Fatalf("stale list applied: %v", ids(m2.msgs))
	}

	// more pages: the status says how far back the list goes
	m, _ = update(m, listMsg{seq: m.listSeq, msgs: testInbox()[:2], more: true, next: t0.AddDate(0, -1, 0)})
	if !m.hasMore || !strings.HasPrefix(m.status, "2 messages since 2024-04-") || !strings.HasSuffix(m.status, "(more below)") {
		t.Fatalf("hasMore %v status %q", m.hasMore, m.status)
	}

	// next page appends without duplicating a boundary message, cursor stays
	m.mcur = 1
	m, _ = update(m, listMsg{seq: m.listSeq, msgs: testInbox()[1:], append: true})
	if !reflect.DeepEqual(ids(m.msgs), []int{1, 2, 3, 4}) || m.mcur != 1 || m.hasMore {
		t.Fatalf("append: %v cur %d more %v", ids(m.msgs), m.mcur, m.hasMore)
	}
}

func TestListFailure(t *testing.T) {
	m := loaded(t, nil)
	m, _ = update(m, listMsg{seq: m.listSeq, err: errors.New("search failed (TIMEOUT)")})
	if !reflect.DeepEqual(ids(m.msgs), []int{1, 2, 3, 4}) {
		t.Fatalf("failed load wiped the list: %v", ids(m.msgs))
	}
	if got := statusLine(m); !strings.Contains(got, "search failed (TIMEOUT)") {
		t.Fatalf("status line %q", got)
	}
	// a failed next page lets the next scroll retry it
	m, _ = update(m, listMsg{seq: m.listSeq, append: true, err: errors.New("x")})
	if !m.hasMore {
		t.Fatal("failed page not retryable")
	}
	// merged views keep partial results and still show the error
	m, _ = update(m, listMsg{seq: m.listSeq, msgs: testInbox()[:1], err: errors.New("a2://INBOX: broken")})
	if len(m.msgs) != 1 || m.err != "a2://INBOX: broken" {
		t.Fatalf("partial: %v err %q", ids(m.msgs), m.err)
	}
	// the next good load clears the error
	m, _ = update(m, listMsg{seq: m.listSeq, msgs: testInbox()})
	if m.err != "" {
		t.Fatalf("err %q survived a good load", m.err)
	}
}

func TestListNavigation(t *testing.T) {
	for _, c := range []struct {
		keys []string
		want int
	}{
		{[]string{"j"}, 1},
		{[]string{"down", "down", "up"}, 1},
		{[]string{"j", "j", "j", "j", "j", "j"}, 3}, // clamps at the end
		{[]string{"k"}, 0},                          // clamps at the top
		{[]string{"G"}, 3},
		{[]string{"G", "g"}, 0},
		{[]string{"end", "home"}, 0},
		{[]string{"ctrl+d"}, 3},
	} {
		m := loaded(t, nil)
		m = press(t, m, c.keys...)
		if m.mcur != c.want {
			t.Errorf("%v: cursor %d, want %d", c.keys, m.mcur, c.want)
		}
	}
}

func TestPaneFocus(t *testing.T) {
	m := loaded(t, nil)
	for _, c := range []struct {
		key  string
		want pane
	}{{"tab", pDetail}, {"tab", pMessages}, {"2", pDetail}, {"1", pMessages}, {"2", pDetail}, {"h", pMessages}} {
		m = press(t, m, c.key)
		if m.focus != c.want {
			t.Fatalf("after %q focus %d, want %d", c.key, m.focus, c.want)
		}
	}
	// j in the message pane scrolls it, not the list
	m = press(t, m, "2", "j")
	if m.mcur != 0 {
		t.Fatalf("j in the preview moved the list cursor to %d", m.mcur)
	}
}

func TestScrollingToEndLoadsNextPageOnce(t *testing.T) {
	m := loaded(t, nil)
	m.hasMore = true
	seq := m.listSeq
	m = press(t, m, "G")
	if m.listSeq != seq+1 || m.hasMore {
		t.Fatalf("seq %d→%d hasMore %v", seq, m.listSeq, m.hasMore)
	}
	m = press(t, m, "k", "G")
	if m.listSeq != seq+1 {
		t.Fatal("second page request while one is in flight")
	}
}

func TestNextPrevUnread(t *testing.T) {
	m := loaded(t, nil) // unread: 1 (idx 0), 3 (idx 2)
	m = press(t, m, "n")
	if m.mcur != 2 {
		t.Fatalf("n → %d", m.mcur)
	}
	m = press(t, m, "n")
	if m.mcur != 2 || m.status != "no more unread" {
		t.Fatalf("n at last unread → %d %q", m.mcur, m.status)
	}
	m = press(t, m, "p")
	if m.mcur != 0 {
		t.Fatalf("p → %d", m.mcur)
	}
	m.hasMore = true
	seq := m.listSeq
	m = press(t, m, "n", "n")
	if m.listSeq != seq+1 || m.status != "loading more… press n again" {
		t.Fatalf("n past the loaded page: seq %d status %q", m.listSeq, m.status)
	}
}

func TestSpaceSelection(t *testing.T) {
	m := loaded(t, nil)
	m = press(t, m, " ", "j", " ")
	if !reflect.DeepEqual(m.marked, map[int]bool{1: true, 3: true}) || m.mcur != 3 {
		t.Fatalf("marked %v cur %d", m.marked, m.mcur)
	}
	if !strings.Contains(screen(m), "2 selected (esc clears)") {
		t.Fatal("selection count not shown")
	}
	m = press(t, m, "k", " ") // space on a selected message unselects it
	if !reflect.DeepEqual(m.marked, map[int]bool{1: true}) {
		t.Fatalf("toggle: %v", m.marked)
	}
	// a reload keeps the selection of messages still listed
	m.marked[3] = true
	m, _ = update(m, listMsg{seq: m.listSeq, msgs: testInbox()[1:]})
	if !reflect.DeepEqual(m.marked, map[int]bool{3: true}) {
		t.Fatalf("after reload: %v", m.marked)
	}
	m = press(t, m, "esc")
	if len(m.marked) != 0 || m.status != "selection cleared" {
		t.Fatalf("esc: %v %q", m.marked, m.status)
	}
}

func TestUnreadFilterToggle(t *testing.T) {
	m := loaded(t, nil)
	seq := m.listSeq
	m = press(t, m, "u")
	if !m.unread || m.status != "unread only" || m.listSeq != seq+1 || !strings.Contains(screen(m), "· unread") {
		t.Fatalf("u: unread %v status %q", m.unread, m.status)
	}
	m = press(t, m, "u")
	if m.unread || m.status != "all messages" {
		t.Fatalf("u again: unread %v status %q", m.unread, m.status)
	}
}

func TestHelpScreen(t *testing.T) {
	m := press(t, loaded(t, nil), "?")
	if m.mode != mHelp || !strings.Contains(screen(m), "lazythunder — keys") {
		t.Fatal("? does not show help")
	}
	m = press(t, m, "j") // any key closes it and does nothing else
	if m.mode != mNormal || m.mcur != 0 {
		t.Fatalf("mode %d cur %d", m.mode, m.mcur)
	}
}

// search drives "/" + query + enter.
func search(t *testing.T, m Model, q string) Model {
	t.Helper()
	m = press(t, m, "/")
	if m.mode != mInput || m.inputFor != "search" {
		t.Fatalf("/ mode %d for %q", m.mode, m.inputFor)
	}
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(q)})
	return press(t, m, "enter")
}

func TestSearchFlow(t *testing.T) {
	m := loaded(t, nil)
	m.focus = pDetail
	m = search(t, m, "invoice")
	if m.view.folderIDs != nil || m.view.query != "invoice" || m.view.title != "Search: invoice · all mail (esc back)" {
		t.Fatalf("view %+v", m.view)
	}
	if m.mode != mNormal || m.focus != pMessages || len(m.msgs) != 0 || m.prevView.title != "All inboxes" {
		t.Fatalf("mode %d focus %d msgs %d prev %q", m.mode, m.focus, len(m.msgs), m.prevView.title)
	}
	seq := m.listSeq

	// batches merge newest first; the status says more is coming
	m, _ = update(m, listMsg{seq: seq, partial: true, msgs: []tb.Message{testMsg(10, "a1", "b", true, 5), testMsg(11, "a1", "d", true, 20)}})
	if !reflect.DeepEqual(ids(m.msgs), []int{10, 11}) || m.status != "2 messages · searching…" {
		t.Fatalf("batch 1: %v %q", ids(m.msgs), m.status)
	}
	m = press(t, m, "j") // on 11
	m, _ = update(m, listMsg{seq: seq, partial: true, msgs: []tb.Message{testMsg(12, "a2", "a", true, 1), testMsg(10, "a1", "b", true, 5), testMsg(13, "a1", "c", true, 10)}})
	if !reflect.DeepEqual(ids(m.msgs), []int{12, 10, 13, 11}) {
		t.Fatalf("batch 2: %v", ids(m.msgs))
	}
	if m.msgs[m.mcur].ID != 11 {
		t.Fatalf("cursor jumped to %d", m.msgs[m.mcur].ID)
	}

	// a batch from a superseded search is ignored
	m2, _ := update(m, listMsg{seq: seq - 1, partial: true, msgs: []tb.Message{testMsg(99, "a1", "old", true, 0)}})
	if len(m2.msgs) != 4 {
		t.Fatalf("stale batch merged: %v", ids(m2.msgs))
	}

	// the final message ends the search
	m, _ = update(m, listMsg{seq: seq})
	if m.status != "4 messages" || m.err != "" {
		t.Fatalf("final: %q err %q", m.status, m.err)
	}

	// esc goes back to the folder view
	m = press(t, m, "esc")
	if m.view.title != "All inboxes" || m.view.folderIDs == nil || m.listSeq != seq+1 {
		t.Fatalf("esc: view %+v", m.view)
	}
}

func TestSearchErrorAndCancel(t *testing.T) {
	m := search(t, loaded(t, nil), "x")
	m, _ = update(m, listMsg{seq: m.listSeq, partial: true, msgs: []tb.Message{testMsg(10, "a1", "b", true, 5)}})
	m, _ = update(m, listMsg{seq: m.listSeq, err: errors.New("full-text search: no gloda")})
	if len(m.msgs) != 1 || !strings.Contains(statusLine(m), "full-text search: no gloda") {
		t.Fatalf("msgs %v status line %q", ids(m.msgs), statusLine(m))
	}

	// a new search supersedes the old one, and esc still returns to the folder, not the old search
	old := m.listSeq
	m = search(t, m, "y")
	m, _ = update(m, listMsg{seq: old, partial: true, msgs: []tb.Message{testMsg(77, "a1", "late", true, 1)}})
	if len(m.msgs) != 0 {
		t.Fatalf("late batch of the old search shown: %v", ids(m.msgs))
	}
	if m = press(t, m, "esc"); m.view.title != "All inboxes" {
		t.Fatalf("esc went to %q", m.view.title)
	}

	// esc in the prompt, or an empty query, leaves the view alone
	m = press(t, m, "/", "esc")
	if m.mode != mNormal || m.view.title != "All inboxes" {
		t.Fatalf("esc in prompt: mode %d view %q", m.mode, m.view.title)
	}
	m = press(t, m, "/", "enter")
	if m.mode != mNormal || m.view.title != "All inboxes" {
		t.Fatalf("empty search: mode %d view %q", m.mode, m.view.title)
	}
}
