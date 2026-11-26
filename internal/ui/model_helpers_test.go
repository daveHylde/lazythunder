package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// testHub is a fake add-on hub on a temp unix socket. It records every
// request as "op params" (params re-marshaled, so keys are sorted) and
// answers from data, or with an error for ops in fail.
type testHub struct {
	mu    sync.Mutex
	calls []string
	data  map[string]any    // op → response data
	fail  map[string]string // op → error message
}

func newTestHub(t *testing.T) (*tb.Client, *testHub) {
	t.Helper()
	h := &testHub{data: map[string]any{}, fail: map[string]string{}}
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var cmu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		l.Close()
		cmu.Lock()
		for _, c := range conns {
			c.Close()
		}
		cmu.Unlock()
	})
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			cmu.Lock()
			conns = append(conns, nc)
			cmu.Unlock()
			go h.serve(nc)
		}
	}()
	// Gloda points nowhere: no test may reach the real profile.
	return &tb.Client{Socket: path, Gloda: filepath.Join(dir, "none.sqlite")}, h
}

func (h *testHub) serve(nc net.Conn) {
	var wmu sync.Mutex
	r := bufio.NewReader(nc)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Op     string          `json:"op"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(line, &req)
		var p any
		json.Unmarshal(req.Params, &p)
		norm, _ := json.Marshal(p)
		h.mu.Lock()
		h.calls = append(h.calls, req.Op+" "+string(norm))
		data := h.data[req.Op]
		msg, failed := h.fail[req.Op]
		h.mu.Unlock()
		resp := map[string]any{"id": req.ID, "ok": true, "data": data}
		if failed {
			resp = map[string]any{"id": req.ID, "ok": false, "error": msg, "code": "TEST"}
		}
		b, _ := json.Marshal(resp)
		wmu.Lock()
		nc.Write(append(b, '\n'))
		wmu.Unlock()
	}
}

func (h *testHub) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *testHub) set(op string, data any) {
	h.mu.Lock()
	h.data[op] = data
	h.mu.Unlock()
}

func (h *testHub) failOp(op, msg string) {
	h.mu.Lock()
	h.fail[op] = msg
	h.mu.Unlock()
}

// testModel is a ready Model sized 120×40, like Run builds it. Without a
// client it gets one dialing a socket that does not exist, so nothing can
// ever reach the real hub.
func testModel(t *testing.T, c *tb.Client) Model {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir()) // draft files from edit()
	if c == nil {
		dir := t.TempDir()
		c = &tb.Client{Socket: filepath.Join(dir, "none.sock"), Gloda: filepath.Join(dir, "none.sqlite")}
	}
	ti := textinput.New()
	ti.Prompt = "> "
	m := Model{c: c, ctx: context.Background(), input: ti, accounts: map[string]tb.Account{},
		preview: newViewport(), agentVP: newViewport(), attVP: newViewport(),
		images: newImageStore(), replied: map[int]store.Reply{}, ready: true}
	t.Cleanup(m.images.close)
	m, _ = update(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

var t0 = time.Date(2024, 5, 10, 12, 0, 0, 0, time.UTC)

func testMsg(id int, acct, subj string, read bool, ageH int) tb.Message {
	return tb.Message{ID: id, Date: t0.Add(-time.Duration(ageH) * time.Hour), Author: "Alice <alice@x.no>",
		Subject: subj, Read: read, Folder: tb.FolderRef{AccountID: acct, Path: "/INBOX"}}
}

func testAccounts() []tb.Account {
	return []tb.Account{
		{ID: "a1", Name: "Work", Identities: []tb.Identity{{Email: "me@work.no"}}},
		{ID: "a2", Name: "Home", Identities: []tb.Identity{{Email: "me@home.no"}}},
		{ID: "a3", Name: "Empty"}, // no folders: gets no tab
	}
}

func testFolders(workUnread, homeUnread int) []tb.Folder {
	return []tb.Folder{
		{ID: "a1://", AccountID: "a1", Name: "Work", Depth: 0}, // account root: not a folder
		{ID: "a1://INBOX", AccountID: "a1", Name: "Inbox", Type: "inbox", Unread: workUnread, Depth: 1},
		{ID: "a1://Archive", AccountID: "a1", Name: "Archive", Type: "archives", Depth: 1},
		{ID: "a2://INBOX", AccountID: "a2", Name: "Inbox", Type: "inbox", Unread: homeUnread, Depth: 1},
	}
}

// testInbox is the All inboxes list, newest first.
func testInbox() []tb.Message {
	return []tb.Message{
		testMsg(1, "a1", "Hello", false, 1),
		testMsg(2, "a2", "Invoice", true, 2),
		testMsg(3, "a1", "Meeting", false, 3),
		testMsg(4, "a1", "Newsletter", true, 4),
	}
}

// loaded is a model past startup: folders loaded, All inboxes listing testInbox.
func loaded(t *testing.T, c *tb.Client) Model {
	t.Helper()
	m := testModel(t, c)
	m, _ = update(m, foldersMsg{folders: testFolders(3, 2), accounts: testAccounts()})
	m, _ = update(m, listMsg{seq: m.listSeq, msgs: testInbox()})
	return m
}

func update(m Model, msg tea.Msg) (Model, tea.Cmd) {
	mm, cmd := m.Update(msg)
	return mm.(Model), cmd
}

// key builds the KeyMsg whose String() is s.
func keyMsg(s string) tea.KeyMsg {
	named := map[string]tea.KeyType{"enter": tea.KeyEnter, "esc": tea.KeyEsc, " ": tea.KeySpace,
		"tab": tea.KeyTab, "delete": tea.KeyDelete, "f5": tea.KeyF5, "ctrl+c": tea.KeyCtrlC,
		"down": tea.KeyDown, "up": tea.KeyUp, "home": tea.KeyHome, "end": tea.KeyEnd,
		"ctrl+s": tea.KeyCtrlS, "ctrl+d": tea.KeyCtrlD, "ctrl+e": tea.KeyCtrlE, "ctrl+z": tea.KeyCtrlZ,
		"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown}
	if t, ok := named[s]; ok {
		return tea.KeyMsg{Type: t}
	}
	if r, ok := strings.CutPrefix(s, "alt+"); ok {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r), Alt: true}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, s := range keys {
		k := keyMsg(s)
		if k.String() != s {
			t.Fatalf("key helper: %q became %q", s, k.String())
		}
		m, _ = update(m, k)
	}
	return m
}

// pressCmd presses one key and returns the command it produced.
func pressCmd(t *testing.T, m Model, s string) (Model, tea.Cmd) {
	t.Helper()
	return update(m, keyMsg(s))
}

// run executes a command that must finish promptly (no ticks).
func run(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(3 * time.Second):
		t.Fatal("command did not finish (a tick or a blocked call?)")
	}
	return nil
}

// subCmds unpacks a tea.Batch or tea.Sequence message into its commands.
func subCmds(t *testing.T, msg tea.Msg) []tea.Cmd {
	t.Helper()
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice || v.Type().Elem() != reflect.TypeOf(tea.Cmd(nil)) {
		t.Fatalf("not a batch/sequence: %T", msg)
	}
	out := make([]tea.Cmd, v.Len())
	for i := range out {
		out[i] = v.Index(i).Interface().(tea.Cmd)
	}
	return out
}

func screen(m Model) string { return ansi.Strip(m.View()) }

// statusLine is the bottom row of the screen.
func statusLine(m Model) string {
	lines := strings.Split(screen(m), "\n")
	return lines[len(lines)-1]
}

func ids(ms []tb.Message) []int {
	var out []int
	for _, x := range ms {
		out = append(out, x.ID)
	}
	return out
}
