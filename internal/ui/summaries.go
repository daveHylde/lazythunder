package ui

import (
	"encoding/json"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/bg"
	"github.com/daveHylde/lazythunder/internal/store"
)

type sumState int

const (
	sumNone sumState = iota
	sumQueued
	sumRunning
	sumDone
	sumFailed
)

type (
	pollTick      struct{}
	newMailMsg    struct{ n int }
	noteQueuedMsg struct{} // the open message's note state may have changed
	dataTick      struct{} // time to check the store for changes by the workers
)

// noteView is a message's note and where its job stands.
type noteView struct {
	hdr     string
	sum     agent.Summary
	state   sumState
	errText string
}

// sig changes whenever what the preview shows for the note changes.
func (n noteView) sig() string {
	b, _ := json.Marshal(n.sum)
	return n.hdr + "\x00" + strconv.Itoa(int(n.state)) + n.errText + string(b)
}

// lookup reads hdr's note from the store. Only Update may call it (via
// loadNote): View works from the cached m.note.
func (m Model) lookup(hdr string) noteView {
	n := noteView{hdr: hdr}
	if m.st == nil || hdr == "" {
		return n
	}
	data, _, _, ok, err := m.st.Note(hdr)
	if err != nil {
		n.state, n.errText = sumFailed, err.Error()
		return n
	}
	if ok && json.Unmarshal(data, &n.sum) == nil && n.sum.Summary != "" {
		n.state = sumDone
		return n
	}
	n.sum = agent.Summary{}
	state, errText, err := m.st.JobState(hdr)
	switch {
	case err != nil:
		n.state, n.errText = sumFailed, err.Error()
	case state == "queued":
		n.state = sumQueued
	case state == "running":
		n.state = sumRunning
	case state == "failed":
		n.state, n.errText = sumFailed, errText
	}
	return n
}

// loadNote caches the open message's note; it reports whether that changed.
func (m *Model) loadNote() bool {
	var n noteView
	if m.full != nil {
		n = m.lookup(m.full.HeaderMessageID)
	}
	changed := n.sig() != m.note.sig()
	m.note = n
	return changed
}

// noteFor is the cached note if it is hdr's, else none yet.
func (m Model) noteFor(hdr string) noteView {
	if m.note.hdr == hdr {
		return m.note
	}
	return noteView{hdr: hdr}
}

// enqueueNote puts a message the user rests on ahead of new mail.
func (m Model) enqueueNote(id int, hdr string) tea.Cmd {
	st, w := m.st, m.worker
	if st == nil || hdr == "" || !m.ag.Enabled() {
		return nil
	}
	return func() tea.Msg {
		enqueue := st.Enqueue
		var old agent.Summary
		if data, _, _, ok, _ := st.Note(hdr); ok && json.Unmarshal(data, &old) == nil && old.Version < agent.SummaryVersion {
			enqueue = st.Requeue // written by an older prompt: redo it, the old note shows meanwhile
		}
		if err := enqueue(hdr, id, 10); err != nil {
			return statusMsg("queue summary: " + err.Error())
		}
		w.Wake()
		return noteQueuedMsg{}
	}
}

// setPause keeps the workers (ours and the daemon's) off the model while the
// chat agent uses it: one model slot, the chat goes first. The deadline frees
// them if we die mid-chat.
func (m *Model) setPause(on bool) {
	if m.st == nil {
		return
	}
	v := "0"
	if on {
		v = strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	}
	if err := m.st.SetKV(bg.KeyPauseUntil, v); err != nil {
		m.err = "pause summaries: " + err.Error()
	}
}

// daemonDown is true when no daemon is doing the background work, so the
// TUI does it itself.
func daemonDown(st *store.Store) bool {
	v, _, err := st.KV(bg.KeyHeartbeat)
	return err != nil || !bg.DaemonAlive(v, time.Now())
}

// pollNewMail runs a poller pass; fresh inbox mail is queued for notes.
func (m Model) pollNewMail() tea.Cmd {
	if m.st == nil {
		return nil
	}
	p := bg.Poller{Store: m.st, Mail: m.c, AI: m.ag.Enabled()}
	folders, ctx := m.folders, m.ctx
	return func() tea.Msg {
		n, _ := p.Pass(ctx, folders) // shortcut: failed folders are retried next minute, unreported
		return newMailMsg{n}
	}
}

func nextPoll() tea.Cmd {
	return tea.Tick(time.Minute, func(time.Time) tea.Msg { return pollTick{} })
}

func nextDataCheck() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return dataTick{} })
}

// loadMeta caches the category tags of the listed messages for the list view.
func (m *Model) loadMeta() {
	if m.st == nil {
		return
	}
	hdrs := make([]string, 0, len(m.msgs))
	for _, x := range m.msgs {
		hdrs = append(hdrs, x.HeaderMessageID)
	}
	meta, err := m.st.NoteMeta(hdrs)
	if err != nil {
		m.err = "notes: " + err.Error()
		return
	}
	m.meta = meta
}

// checkData picks up what the workers stored since the last check.
func (m Model) checkData() (tea.Model, tea.Cmd) {
	if m.st == nil {
		return m, nil
	}
	v, err := m.st.DataVersion()
	if err != nil || v == m.dataVer {
		return m, nextDataCheck()
	}
	m.dataVer = v
	m.loadMeta()
	replied := m.applyStoredReplies()
	var cmds []tea.Cmd
	if changed := m.loadNote(); m.full != nil && (changed || replied[m.full.ID]) {
		cmds = append(cmds, m.refreshSummary(), m.maybeLoadReply())
	}
	return m, tea.Batch(append(cmds, nextDataCheck())...)
}

// refreshSummary re-renders the preview after the open message's note changed.
func (m *Model) refreshSummary() tea.Cmd {
	before := m.preview.TotalLineCount()
	cmd := m.refreshPreview(false)
	if off := m.preview.YOffset; off > 0 { // the summary sits above the body: keep the text in place
		m.preview.SetYOffset(off + m.preview.TotalLineCount() - before)
	}
	return cmd
}
