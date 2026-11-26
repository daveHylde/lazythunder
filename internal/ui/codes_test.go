package ui

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// withCodes opens the first listed message with a stored note carrying codes.
func withCodes(t *testing.T, codes ...string) Model {
	t.Helper()
	clipboardCmd = "true" // never touch the real clipboard
	t.Cleanup(func() { clipboardCmd = "wl-copy" })
	st, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	m := loaded(t, nil)
	m.st, m.sidebar = st, true
	m, _ = update(m, tea.WindowSizeMsg{Width: 200, Height: 50})
	m.msgs[0].HeaderMessageID = "h1"
	sum := agent.Summary{Version: agent.SummaryVersion, Summary: "Your code"}
	for _, c := range codes {
		sum.Actions = append(sum.Actions, agent.Action{Text: "Enter the code", Copy: c})
	}
	data, _ := json.Marshal(sum)
	if err := st.PutNote("h1", data, "", ""); err != nil {
		t.Fatal(err)
	}
	m, _ = update(m, previewMsg{seq: m.previewSeq, m: tb.FullMessage{Message: m.msgs[0]}})
	return m
}

func TestOneCodeCopiesWithY(t *testing.T) {
	m := withCodes(t, "483920")
	if s := screen(m); !strings.Contains(s, "483920 · y") || !strings.Contains(s, "· y copies") {
		t.Fatalf("code not shown in the action items:\n%s", s)
	}
	m = press(t, m, "y")
	if m.picker != nil || !strings.Contains(m.status, "copied code 483920") {
		t.Fatalf("status %q picker %v", m.status, m.picker)
	}
}

func TestSeveralCodesPickByNumber(t *testing.T) {
	m := withCodes(t, "483920", "SAVE50")
	if s := screen(m); !strings.Contains(s, "SAVE50 · y2") || !strings.Contains(s, "y+n copies") {
		t.Fatalf("numbered codes missing:\n%s", s)
	}
	m = press(t, m, "y")
	if m.picker == nil || m.picker.kind != "codes" {
		t.Fatalf("y with 2 codes should open the list, got %v", m.picker)
	}
	m, _ = update(m, keyMsg("2"))
	if m.picker != nil || !strings.Contains(m.status, "copied code SAVE50") {
		t.Fatalf("status %q", m.status)
	}
}

func TestNoCodes(t *testing.T) {
	m := withCodes(t)
	m = press(t, m, "y")
	if m.status != "no codes in this message" {
		t.Fatalf("status %q", m.status)
	}
}

func openTestStore(t *testing.T) (*store.Store, error) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err == nil {
		t.Cleanup(func() { st.Close() })
	}
	return st, err
}

func TestPasteable(t *testing.T) {
	for in, want := range map[string]string{"731 204": "731204", "SAVE 50": "SAVE 50", "483920": "483920"} {
		if got := pasteable(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
