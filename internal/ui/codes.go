package ui

import (
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/agent"
)

// codeActions are the open message's action items that carry a code to copy.
func codeActions(sum agent.Summary) []agent.Action {
	var out []agent.Action
	for _, a := range sum.Actions {
		if a.Copy != "" {
			out = append(out, a)
		}
	}
	return out
}

// copyCodes copies the open message's code: at once when there is one, else
// through a numbered list (y then the number, like o for links).
func (m Model) copyCodes() (tea.Model, tea.Cmd) {
	codes := codeActions(m.noteFor(m.full.HeaderMessageID).sum)
	switch len(codes) {
	case 0:
		m.status = "no codes in this message"
		return m, nil
	case 1:
		return m.copyText(pasteable(codes[0].Copy), "code")
	}
	p := &picker{kind: "codes", msgID: m.full.ID}
	for i, a := range codes {
		p.items = append(p.items, pickItem{n: i + 1, title: a.Copy, detail: a.Text})
	}
	m.picker, m.mode = p, mPicker
	return m, nil
}

// clipboardCmd reads text on stdin and puts it on the clipboard; tests swap it.
var clipboardCmd = "wl-copy"

// copyText puts s on the clipboard.
func (m Model) copyText(s, what string) (tea.Model, tea.Cmd) {
	m.picker, m.mode = nil, mNormal
	cmd := exec.Command(clipboardCmd)
	cmd.Stdin = strings.NewReader(s)
	if err := cmd.Run(); err != nil {
		m.err = "copy: " + err.Error()
		return m, nil
	}
	m.err, m.status = "", fmt.Sprintf("copied %s %s", what, s)
	return m, nil
}

// pasteable drops the spaces of a digit code written in groups ("731 204"),
// which code fields usually reject.
func pasteable(code string) string {
	if strings.Trim(code, "0123456789 ") == "" {
		return strings.ReplaceAll(code, " ", "")
	}
	return code
}
