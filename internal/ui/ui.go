// Package ui is the lazygit-style terminal interface.
package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"golang.org/x/sys/unix"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/bg"
	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
	"github.com/daveHylde/lazythunder/internal/termimg"
)

type pane int

const (
	pMessages pane = iota
	pDetail
)

type mode int

const (
	mNormal    mode = iota
	mInput          // search input at the bottom
	mConfirm        // y/n question
	mDraft          // compose mode: the draft and its agent column
	mPicker         // link or attachment picker in the detail pane
	mAttView        // previewing an attachment in the detail pane
	mAccounts       // the accounts screen (ctrl+a)
	mAssistant      // the Assistant screen (: or alt+0)
	mHelp
)

const pageSize = 100

// prog lets background goroutines (the agent) talk to the UI.
var prog *tea.Program

// sendMsg delivers a message from a background goroutine; tests replace it.
var sendMsg = func(msg tea.Msg) { prog.Send(msg) }

// tbReady lets our background worker start once Thunderbird answers.
var tbReady atomic.Bool

// latestList is the newest listSeq, readable from search goroutines.
var latestList atomic.Int64

type Model struct {
	c   *tb.Client
	ag  *agent.Agent
	ctx context.Context

	w, h   int
	focus  pane
	mode   mode
	ready  bool
	status string
	err    string

	folders   []tb.Folder // all folders of all accounts (roots excluded)
	accounts  map[string]tb.Account
	acctOrder []string // account ids in tab order; tab 0 is "All inboxes"
	tab       int
	view      listView
	prevView  listView // restored when leaving search results

	thread    []tb.ThreadItem // conversation of the open message (threadHdr)
	threadHdr string
	unread    bool // unread-only filter

	msgs    []tb.Message
	mcur    int
	hasMore bool
	before  time.Time // date cursor for the next page
	listSeq int

	preview       viewport.Model
	full          *tb.FullMessage
	previewFailed int // message whose preview failed to load (its error is shown)
	images        *imageStore
	st            *store.Store // nil when the database could not be opened: no notes
	storeErr      string
	worker        *bg.Worker
	dataVer       int64                 // store.DataVersion last seen
	meta          map[string]store.Meta // note tags of the listed messages, by Message-ID
	note          noteView              // the open message's note, cached: View never reads the store
	sidebar       bool                  // v toggles the message sidebar (summary, actions, links, attachments)
	attPrev       *attPreview
	attCancel     context.CancelFunc
	cellW         int // terminal cell size in px (0 if unknown)
	cellH         int
	placements    []render.Placement // inline images in the preview, for sixel overlay
	links         []link             // links in the open message, for the picker

	viewGen      int // bumped when the list changes view; stale async results are dropped
	viewCtx      context.Context
	cancelView   context.CancelFunc
	replied      map[int]store.Reply
	reply        *loadedReply
	replyLoading int

	picker     *picker
	accts      *acctScreen // open accounts screen
	attVP      viewport.Model
	attTitle   string
	previewSeq int

	asst       assistant // the Assistant screen: its conversation and what it found
	backToAsst bool      // the open message came from the Assistant's Found list: esc returns there

	input      textinput.Model // search
	confirm    *confirmation
	draft      *tb.Draft
	draftCtx   string
	draftFile  string          // edited file kept after a parse error so the text isn't lost
	submitting bool            // a save/send is in flight
	draftGen   int             // bumped per opened draft: a late revision can't land on another one
	draftUndo  []tb.Draft      // versions before each revision or edit (ctrl+z restores)
	draftAsks  []string        // the instruction behind each version in draftUndo, sent as history
	draftChat  []string        // compose column: "you: …" and "✦ …" notes, never the draft itself
	draftBase  int             // 1 when the draft opened with a body (the assistant's v1)
	draftTop   int             // compose: first draft line shown (pgup/pgdown)
	compose    textinput.Model // compose: the instruction input
	revising   bool            // an agent revision is in flight
	pending    *tb.Draft       // agent draft waiting for the current draft to finish
	moveIDs    []int           // messages the move picker will move
	marked     map[int]bool    // space-selected message ids; actions apply to all of them
}

// listView is what the message list shows: one or more folders, or a search.
type listView struct {
	title     string
	folderIDs []string // nil for search results
	accountID string   // "" when merged across accounts
	query     string
	thread    []tb.ThreadItem // a conversation opened from the thread box
	found     []tb.Message    // the Assistant's Found messages, opened from its list
	focus     string          // thread, found: Message-ID to put the cursor on once listed
	back      *listView       // thread, found: the view it was opened from (esc returns there)
}

type confirmation struct {
	text  string
	reply chan bool // agent confirmations
	onYes tea.Cmd   // local confirmations
	onNo  tea.Cmd   // set: n runs it and esc just closes the question
}

// ---- messages ----

type (
	statusMsg  string
	readyMsg   struct{ err error }
	foldersMsg struct {
		folders  []tb.Folder
		accounts []tb.Account
		err      error
	}
	listMsg struct {
		seq    int
		msgs   []tb.Message
		next   time.Time
		more   bool
		append bool
		// refresh: new mail reloaded the first page in place; the cursor and older pages stay
		refresh bool
		err     error
		// partial: a search batch with more to come
		partial bool
	}
	previewTick struct{ seq, id int }
	readTick    struct{ seq, id int } // the cursor rested on a message for 1s
	previewMsg  struct {
		seq int
		m   tb.FullMessage
		err error
	}
	doneMsg struct {
		text   string
		err    error
		remove []int // drop these ids from the list
		reload bool  // reload the list (folder-wide changes)
	}
	draftMsg  struct{ d tb.Draft }
	editedMsg struct {
		path string
		err  error
	}
	submittedMsg struct {
		sent bool
		err  error
	}
	sendingMsg struct{}
)

func Run(c *tb.Client, ag *agent.Agent) error {
	ti := textinput.New()
	ti.Prompt = "> "
	m := Model{c: c, ag: ag, ctx: context.Background(), input: ti, accounts: map[string]tb.Account{},
		preview: newViewport(), attVP: newViewport(), status: "connecting to Thunderbird…",
		images: newImageStore(), replied: map[int]store.Reply{}, sidebar: true}
	defer m.images.close()
	if st, err := openStore(); st == nil {
		m.storeErr = "notes unavailable: " + err.Error()
		m.err = m.storeErr
	} else {
		if err != nil { // the store works; only the old cache didn't come along
			m.storeErr = "old summary cache: " + err.Error()
			m.err = m.storeErr
		}
		defer st.Close() // deferred first, so it runs after the worker has stopped
		m.st = st
		m.worker = &bg.Worker{Store: st, Mail: c, AI: ag, Name: fmt.Sprintf("tui-%d", os.Getpid())}
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		defer func() { stop(); <-done }()
		go func() {
			defer close(done)
			m.worker.Run(ctx, func() bool { return tbReady.Load() && daemonDown(st) })
		}()
	}
	termimg.Detect(200 * time.Millisecond) // before bubbletea reads the terminal
	prog = tea.NewProgram(m, tea.WithAltScreen())
	final, err := prog.Run()
	if m.images != nil {
		m.images.mu.Lock()
		deleteKitty(m.images.dropKitty()) // any left (e.g. killed by a signal): free the terminal's copies
		m.images.mu.Unlock()
	}
	if fm, ok := final.(Model); ok && fm.asst.busy {
		fm.setPause(false) // quit mid-chat: free the workers now, not in 10 minutes
	}
	os.Stdout.WriteString(autowrapOn)
	return err
}

// newViewport keeps only scroll keys that don't collide with mail actions (d, f, u, h, l).
func newViewport() viewport.Model {
	v := viewport.New(0, 0)
	v.KeyMap = viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown", " ")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		Down:         key.NewBinding(key.WithKeys("j", "down")),
		Up:           key.NewBinding(key.WithKeys("k", "up")),
	}
	return v
}

func (m Model) Init() tea.Cmd {
	return func() tea.Msg {
		err := m.c.Ensure(m.ctx, func(s string) { go prog.Send(statusMsg(s)) })
		tbReady.Store(err == nil)
		return readyMsg{err}
	}
}

// openStore opens the shared database and takes over the old JSON summary
// cache (idempotent: the file is removed once imported). An import error
// comes with a usable store.
func openStore() (*store.Store, error) {
	path, err := store.DefaultPath()
	if err != nil {
		return nil, err
	}
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return st, st.ImportJSON(bg.LegacyCache())
}

// ---- commands ----

func (m Model) loadFolders() tea.Cmd {
	return func() tea.Msg {
		fs, err := m.c.Folders(m.ctx)
		if err != nil {
			return foldersMsg{err: err}
		}
		as, err := m.c.Accounts(m.ctx)
		return foldersMsg{folders: fs, accounts: as, err: err}
	}
}

func (m *Model) loadList(appendPage bool) tea.Cmd {
	m.listSeq++
	latestList.Store(int64(m.listSeq))
	seq, unread, ctx, c := m.listSeq, m.unread, m.ctx, m.c
	if m.view.thread != nil {
		m.msgs, m.mcur = nil, 0
		return m.loadThreadList(seq)
	}
	if found := m.view.found; found != nil { // already loaded: the Assistant's messages
		m.msgs, m.mcur = nil, 0
		return func() tea.Msg { return listMsg{seq: seq, msgs: found} }
	}
	if m.view.folderIDs == nil {
		q := m.view.query
		m.msgs, m.mcur = nil, 0
		if m.viewCtx != nil {
			ctx = m.viewCtx // show cancels it: a search left behind stops asking Thunderbird
		}
		return func() tea.Msg {
			// shortcut: 200 hits; each costs Thunderbird a folder scan to resolve, page further if people miss older ones
			_, err := c.FullText(ctx, q, tb.SearchOpts{Unread: unread, Limit: 200}, func(ms []tb.Message) bool {
				prog.Send(listMsg{seq: seq, msgs: ms, partial: true})
				return latestList.Load() == int64(seq) // stop once another list replaced it
			})
			return listMsg{seq: seq, err: err}
		}
	}
	ids, before := m.view.folderIDs, time.Time{}
	if appendPage {
		before = m.before
	}
	return func() tea.Msg {
		ms, next, more, err := c.RecentMerged(ctx, ids, before, unread)
		return listMsg{seq: seq, msgs: ms, next: next, more: more, append: appendPage, err: err}
	}
}

// refreshList reloads a folder view's first page for new mail, keeping the
// cursor and the pages scrolled to; other views are left alone.
func (m *Model) refreshList() tea.Cmd {
	if m.view.folderIDs == nil || m.view.thread != nil || m.view.found != nil {
		return nil
	}
	// the current seq: a list load started meanwhile wins, one in flight still lands
	seq, ids, unread, ctx, c := m.listSeq, m.view.folderIDs, m.unread, m.ctx, m.c
	return func() tea.Msg {
		ms, next, more, err := c.RecentMerged(ctx, ids, time.Time{}, unread)
		return listMsg{seq: seq, msgs: ms, next: next, more: more, refresh: true, err: err}
	}
}

// mergeRefresh puts a refreshed first page in place of the list's newest part:
// loaded messages older than the page stay, the cursor stays on its message.
func (m Model) mergeRefresh(msg listMsg) (tea.Model, tea.Cmd) {
	cur, hadCur := m.selected()
	msgs := msg.msgs
	if msg.more && m.before.Before(msg.next) { // older pages were loaded: keep them
		have := map[int]bool{}
		for _, x := range msgs {
			have[x.ID] = true
		}
		for _, x := range m.msgs {
			if !have[x.ID] && !x.Date.After(msg.next) {
				msgs = append(msgs, x)
			}
		}
	} else {
		// hasMore stays false while a page request is in flight
		m.hasMore, m.before = m.hasMore && msg.more, msg.next
	}
	m.msgs, m.mcur = msgs, min(m.mcur, max(len(msgs)-1, 0))
	for i, x := range m.msgs {
		if hadCur && x.ID == cur.ID {
			m.mcur = i
		}
	}
	if len(m.marked) > 0 {
		kept := map[int]bool{}
		for _, x := range m.msgs {
			if m.marked[x.ID] {
				kept[x.ID] = true
			}
		}
		m.marked = kept
	}
	m.loadMeta()
	m.applyStoredReplies()
	status := m.loadReplyStatus(msg.msgs)
	if now, ok := m.selected(); ok && hadCur && now.ID == cur.ID {
		return m, status // same message: the preview stays as it is
	}
	return m, tea.Batch(m.schedulePreview(), status)
}

// schedulePreview debounces preview loads while the cursor moves.
func (m *Model) schedulePreview() tea.Cmd {
	m.previewSeq++
	if len(m.msgs) == 0 {
		m.full, m.placements, m.links = nil, nil, nil
		m.preview.SetContent("")
		return nil
	}
	seq, id := m.previewSeq, m.msgs[m.mcur].ID
	return tea.Batch(
		tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return previewTick{seq, id} }),
		tea.Tick(time.Second, func(time.Time) tea.Msg { return readTick{seq, id} }))
}

func (m Model) act(text string, remove []int, f func() error) tea.Cmd {
	return func() tea.Msg { return doneMsg{text: text, err: f(), remove: remove} }
}

func (m Model) selected() (tb.Message, bool) {
	if m.mcur < len(m.msgs) {
		return m.msgs[m.mcur], true
	}
	return tb.Message{}, false
}

// ---- update ----

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if mm, cmd, ok := m.acctUpdate(msg); ok {
		return mm, cmd
	}
	if mm, cmd, ok := m.asstUpdate(msg); ok {
		return mm, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 && ws.Row > 0 {
			m.cellW, m.cellH = int(ws.Xpixel)/int(ws.Col), int(ws.Ypixel)/int(ws.Row)
		}
		m.layout()
		cmd := m.refreshPreview(false)
		return m, cmd
	case statusMsg:
		m.status = string(msg)
		return m, nil
	case readyMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			m.status = "not connected (F5 to retry, q to quit)"
			return m, nil
		}
		m.ready, m.status, m.err = true, "connected", m.storeErr
		return m, tea.Batch(m.loadFolders(), nextDataCheck())
	case foldersMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.folders = nil // a fresh slice: pollNewMail's goroutine may still read the old one
		for _, f := range msg.folders {
			if f.Depth > 0 {
				m.folders = append(m.folders, f)
			}
		}
		hasFolders := map[string]bool{}
		for _, f := range m.folders {
			hasFolders[f.AccountID] = true
		}
		m.acctOrder = m.acctOrder[:0]
		for _, a := range msg.accounts {
			m.accounts[a.ID] = a
			if hasFolders[a.ID] {
				m.acctOrder = append(m.acctOrder, a.ID)
			}
		}
		if m.view.title == "" { // first load
			mm, cmd := m.openTab(0)
			return mm, tea.Batch(cmd, mm.(Model).pollNewMail(false), m.subscribe())
		}
		m.tab = min(m.tab, len(m.acctOrder))
		if m.viewGone() { // e.g. its account was removed: show the remaining mail instead of an error
			return m.openTab(0)
		}
		return m, nil
	case unsubOptsMsg:
		if x, ok := m.selected(); !ok || x.ID != msg.msg.ID || m.mode != mNormal {
			return m, nil // moved on meanwhile
		}
		return m.confirmUnsubscribe(msg)
	case linkNumTick:
		if m.mode == mPicker && m.picker == msg.p && msg.p.num == msg.num {
			return m.openLinkNumber(msg.p)
		}
		return m, nil
	case listMsg:
		if msg.seq != m.listSeq {
			return m, nil
		}
		if msg.refresh {
			if msg.err != nil || m.view.folderIDs == nil {
				return m, nil // the next new mail or F5 tries again
			}
			return m.mergeRefresh(msg)
		}
		if m.view.folderIDs == nil {
			if !msg.partial {
				m.err = ""
			}
			if msg.err != nil {
				m.err = msg.err.Error()
			}
			return m.searchResults(msg)
		}
		m.err = ""
		if msg.err != nil {
			m.err = msg.err.Error()
			if msg.msgs == nil { // total failure; a merged view may still have partial results
				m.hasMore = msg.append // let the next scroll retry the failed page
				return m, nil
			}
		}
		if msg.append {
			have := map[int]bool{}
			for _, x := range m.msgs {
				have[x.ID] = true
			}
			for _, x := range msg.msgs { // date windows may share a boundary message
				if !have[x.ID] {
					m.msgs = append(m.msgs, x)
				}
			}
		} else {
			m.msgs, m.mcur = msg.msgs, 0
			if len(m.marked) > 0 { // keep the selection for messages still listed
				kept := map[int]bool{}
				for _, x := range m.msgs {
					if m.marked[x.ID] {
						kept[x.ID] = true
					}
				}
				m.marked = kept
			}
		}
		m.hasMore, m.before = msg.more, msg.next
		m.loadMeta()
		m.applyStoredReplies()
		m.status = fmt.Sprintf("%d messages", len(m.msgs))
		if m.hasMore {
			m.status += fmt.Sprintf(" since %s (more below)", m.before.Local().Format("2006-01-02"))
		}
		status := m.loadReplyStatus(msg.msgs)
		if msg.append {
			return m, status
		}
		return m, tea.Batch(m.schedulePreview(), status)
	case previewTick:
		if msg.seq != m.previewSeq {
			return m, nil
		}
		c, ctx := m.c, m.ctx
		return m, func() tea.Msg {
			full, err := c.Read(ctx, msg.id)
			return previewMsg{seq: msg.seq, m: full, err: err}
		}
	case readTick:
		x, ok := m.selected()
		if msg.seq != m.previewSeq || !ok || x.ID != msg.id {
			return m, nil
		}
		note := m.enqueueNote(x.ID, x.HeaderMessageID) // resting on it: summarize this one next
		if x.Read {
			return m, note
		}
		m.msgs[m.mcur].Read = true
		c, ctx := m.c, m.ctx
		return m, tea.Batch(note, m.act("", nil, func() error { return c.Mark(ctx, []int{x.ID}, "read") }))
	case previewMsg:
		if msg.seq != m.previewSeq {
			return m, nil
		}
		if msg.err != nil { // show the failure, never the previous message as if it were this one
			m.full, m.placements, m.links, m.attPrev = nil, nil, nil, nil
			if x, ok := m.selected(); ok {
				m.previewFailed = x.ID
			}
			m.preview.SetContent(errSt.Render("Could not load this message: " + msg.err.Error()))
			m.preview.GotoTop()
			return m, nil
		}
		var free tea.Cmd
		if m.full == nil || m.full.ID != msg.m.ID {
			free = m.images.freeKitty() // the old message's images
		}
		m.full = &msg.m
		m.loadNote()
		cmd := tea.Batch(free, m.refreshPreview(true))
		reply := m.maybeLoadReply()
		att := m.startAttPreview(msg.m)
		return m, tea.Batch(cmd, reply, att, m.loadThread())
	case threadMsg:
		if m.full == nil || m.full.HeaderMessageID != msg.hdr {
			return m, nil
		}
		m.thread, m.threadHdr = msg.items, msg.hdr
		return m, m.refreshPreview(false)
	case sidebarAttMsg:
		if m.full == nil || m.full.ID != msg.p.msgID {
			return m, nil
		}
		p := msg.p
		m.attPrev = &p
		if m.sidebarOn() && m.inlineSharp() {
			if _, thumb := m.sidebarLines(m.sidebarWidth(), m.preview.Height); thumb != nil {
				return m, m.encodeSharp([]sixelReq{*thumb})
			}
		}
		return m, nil
	case replyStatusMsg:
		if msg.gen != m.viewGen {
			return m, nil
		}
		for id, r := range msg.status {
			m.replied[id] = r
		}
		reply := m.maybeLoadReply()
		cmd := m.refreshPreview(false) // forwarded/replied header on the open message
		return m, tea.Batch(reply, cmd)
	case sentReplyMsg:
		if msg.err != nil {
			m.err = "loading your reply: " + msg.err.Error()
			m.replyLoading = 0 // allow a retry next time the message opens
			return m, nil
		}
		m.reply = &loadedReply{forID: msg.forID, msg: msg.msg}
		cmd := m.refreshPreview(false)
		return m, cmd
	case dataTick:
		return m.checkData()
	case noteQueuedMsg:
		if m.loadNote() && m.full != nil {
			return m, m.refreshSummary()
		}
		return m, nil
	case pollTick:
		return m, m.pollNewMail(false)
	case eventsMsg:
		return m, waitEvent(msg.ch)
	case mailEventMsg:
		switch msg.name {
		case "newMail":
			// the list now: a running daemon may have polled this mail already, leaving the pass none
			return m, tea.Batch(waitEvent(msg.ch), m.pollNewMail(true), m.loadFolders(), m.refreshList())
		case "changed": // flags, moves, counts
			return m, tea.Batch(waitEvent(msg.ch), m.loadFolders())
		case "accountsChanged": // tabs, and the accounts screen if open
			return m, tea.Batch(waitEvent(msg.ch), m.loadFolders(), m.loadAccounts())
		}
		return m, waitEvent(msg.ch)
	case newMailMsg:
		if msg.n > 0 {
			m.status = fmt.Sprintf("%d new message(s)", msg.n)
		}
		if msg.event {
			return m, nil // the event refreshed the list already
		}
		if msg.n > 0 {
			return m, tea.Batch(m.loadFolders(), m.refreshList(), nextPoll())
		}
		return m, nextPoll()
	case sixelMsg:
		if msg.failed {
			cmd := m.refreshPreview(false) // draw the failed ones as half-blocks
			return m, cmd
		}
		return m, nil // re-render picks up the encoded images
	case sixelTick:
		if msg.offset != m.preview.YOffset {
			return m, nil // still scrolling
		}
		return m, m.encodeSharp(m.visibleSixels())
	case viewerDoneMsg:
		if msg.err != nil {
			m.err = "image viewer: " + msg.err.Error()
		}
		free := m.images.freeKitty() // the viewer cleared the terminal's images: send them again
		return m, tea.Batch(free, m.refreshPreview(false))
	case noteMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.err, m.status = "", msg.text
		}
		return m, nil
	case attPreviewMsg:
		if m.mode != mPicker || m.picker != msg.picker { // user moved on while it loaded
			return m, nil
		}
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err, m.status = "", ""
		if msg.image != "" {
			return m, tea.Exec(viewerCmd{[]string{msg.image}}, func(err error) tea.Msg { return viewerDoneMsg{err} })
		}
		text := render.Text(msg.text, m.attVP.Width)
		if msg.html {
			text = render.HTML(msg.text, render.Opts{Width: m.attVP.Width, Styled: true})
		}
		m.attTitle = msg.title
		m.attVP.SetContent(text)
		m.attVP.GotoTop()
		m.mode = mAttView
		return m, nil
	case imagesMsg:
		// refresh whatever is open: it may share image URLs with msg.msgID
		cmd := m.refreshPreview(false)
		return m, cmd
	case doneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			if msg.reload { // part of a folder-wide change may have gone through; a list reload would clear the error
				return m, m.loadFolders()
			}
			return m, nil
		}
		m.err = ""
		if msg.text != "" {
			m.status = msg.text
		}
		if msg.reload {
			return m, tea.Batch(m.loadFolders(), m.loadList(false))
		}
		if len(msg.remove) > 0 {
			gone := map[int]bool{}
			for _, id := range msg.remove {
				gone[id] = true
				delete(m.marked, id)
			}
			kept := m.msgs[:0]
			for _, x := range m.msgs {
				if !gone[x.ID] {
					kept = append(kept, x)
				}
			}
			m.msgs = kept
			m.mcur = min(m.mcur, max(0, len(m.msgs)-1))
			return m, tea.Batch(m.schedulePreview(), m.loadFolders())
		}
		return m, m.loadFolders()
	case draftMsg:
		d := msg.d
		if m.draft != nil {
			m.pending = &d
			m.status = m.model() + " has a draft ready; it opens when you finish this one"
			return m, nil
		}
		return m.openDraft(d), nil
	case editedMsg:
		free := m.images.freeKitty() // the editor took the terminal: its images may be gone, send them again
		mm, cmd := m.afterEdit(msg)
		return mm, tea.Batch(free, cmd)
	case sendingMsg:
		m.submitting, m.status = true, "sending…"
		return m, nil
	case revisedMsg:
		return m.applyRevision(msg)
	case saveDraftMsg:
		if msg.gen != m.draftGen || m.draft == nil {
			return m, nil
		}
		return m.submitDraft(false)
	case discardMsg:
		if msg.gen != m.draftGen || m.draft == nil {
			return m, nil
		}
		m = m.closeDraft()
		if m.draft == nil {
			m.status = "draft discarded"
		}
		return m, nil
	case submittedMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err.Error() // the draft stays open so nothing is lost
			return m, nil
		}
		m.err = ""
		m = m.closeDraft()
		m.status = map[bool]string{true: "sent", false: "saved to Drafts"}[msg.sent]
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

// refreshPreview re-renders the open message and starts fetching any images
// it references that aren't loaded yet; imagesMsg triggers another refresh.
func (m *Model) refreshPreview(top bool) tea.Cmd {
	if m.full == nil {
		return nil
	}
	content, misses, links := m.renderMessage(*m.full)
	content, m.placements = render.Placements(content)
	m.links = links
	off := m.preview.YOffset
	m.preview.SetContent(content)
	if top {
		m.preview.GotoTop()
	} else {
		m.preview.SetYOffset(off)
	}
	var cmds []tea.Cmd
	if m.inlineSharp() {
		reqs := m.visibleSixels()
		if m.sidebarOn() {
			if _, thumb := m.sidebarLines(m.sidebarWidth(), m.preview.Height); thumb != nil {
				reqs = append(reqs, *thumb) // resize/toggle changes its size: re-encode
			}
		}
		cmds = append(cmds, m.encodeSharp(reqs))
	}
	if len(misses) > 0 {
		cmds = append(cmds, fetchImages(m.ctx, m.c, m.images, m.full.ID, misses))
	}
	return tea.Batch(cmds...)
}

// quit deletes the inline kitty images first, while still on their (alt) screen.
func (m Model) quit() tea.Cmd {
	return tea.Sequence(m.images.freeKitty(), tea.Quit)
}

// inlineSharp: draw sharp images in the preview (sixel or kitty terminals
// that report their cell size). LAZYTHUNDER_GRAPHICS=blocks turns it off.
func (m Model) inlineSharp() bool {
	p := termimg.Protocol()
	return (p == "sixel" || p == "kitty") && m.cellW > 0 && m.cellH > 0
}

// encodeSharp prepares the requests' sharp images: kitty transmits them, sixel encodes.
func (m Model) encodeSharp(reqs []sixelReq) tea.Cmd {
	if termimg.Protocol() == "kitty" {
		return transmitKitty(m.images, reqs, m.cellW, m.cellH)
	}
	return encodeSixels(m.images, reqs, m.cellW, m.cellH)
}

// foldersOf lists an account's folders; "" lists every account's inbox.
func (m Model) foldersOf(acct string) []tb.Folder {
	var out []tb.Folder
	if acct == "" {
		for _, id := range m.acctOrder {
			for _, f := range m.folders {
				if f.AccountID == id && f.Type == "inbox" {
					out = append(out, f)
				}
			}
		}
		return out
	}
	for _, f := range m.folders {
		if f.AccountID == acct {
			out = append(out, f)
		}
	}
	return out
}

// tabAccount is the active tab's account id, "" for All inboxes.
func (m Model) tabAccount() string {
	if m.tab == 0 || m.tab > len(m.acctOrder) {
		return ""
	}
	return m.acctOrder[m.tab-1]
}

func (m Model) inboxUnread(acct string) (n int) {
	for _, f := range m.folders {
		if f.Type == "inbox" && (acct == "" || f.AccountID == acct) {
			n += f.Unread
		}
	}
	return n
}

// openTab switches account tab and shows its inbox (all inboxes on tab 0).
func (m Model) openTab(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i > len(m.acctOrder) {
		return m, nil
	}
	m.tab = i
	if i == 0 {
		ids := []string{} // non-nil: nil means "search results"
		for _, f := range m.foldersOf("") {
			ids = append(ids, f.ID)
		}
		return m.show(listView{title: "All inboxes", folderIDs: ids})
	}
	fs := m.foldersOf(m.tabAccount())
	for _, f := range fs {
		if f.Type == "inbox" {
			return m.openFolder(f)
		}
	}
	if len(fs) > 0 {
		return m.openFolder(fs[0])
	}
	return m, nil
}

func (m Model) openFolder(f tb.Folder) (tea.Model, tea.Cmd) {
	return m.show(listView{title: m.accounts[f.AccountID].Name + " › " + f.Name, folderIDs: []string{f.ID}, accountID: f.AccountID})
}

// searchResults merges one half of a search (header or content hits) into the
// list, newest first, keeping the cursor on the same message.
func (m Model) searchResults(msg listMsg) (tea.Model, tea.Cmd) {
	var cur int
	if x, ok := m.selected(); ok {
		cur = x.ID
	}
	m.msgs = tb.MergeNewest(m.msgs, msg.msgs)
	m.mcur = 0
	for i, x := range m.msgs {
		if x.ID == cur {
			m.mcur = i
		}
	}
	if f := m.view.focus; f != "" { // a thread view: the message picked in the thread box
		for i, x := range m.msgs {
			if sameMsgID(x.HeaderMessageID, f) {
				m.mcur, m.view.focus = i, ""
			}
		}
	}
	m.loadMeta()
	m.applyStoredReplies()
	m.status = fmt.Sprintf("%d messages", len(m.msgs))
	if msg.partial {
		m.status += " · searching…"
	}
	cmds := []tea.Cmd{m.loadReplyStatus(msg.msgs)}
	if cur == 0 || len(m.msgs) > 0 && m.msgs[m.mcur].ID != cur {
		cmds = append(cmds, m.schedulePreview())
	}
	return m, tea.Batch(cmds...)
}

func (m Model) show(v listView) (tea.Model, tea.Cmd) {
	m.view, m.msgs, m.mcur, m.marked, m.backToAsst = v, nil, 0, nil, false
	m.viewGen++
	if m.cancelView != nil {
		m.cancelView() // stop reply-status lookups for the old view
	}
	m.viewCtx, m.cancelView = context.WithCancel(m.ctx)
	m.replied = map[int]store.Reply{}
	m.status = "loading " + v.title + "…"
	return m, tea.Batch(m.loadList(false), m.schedulePreview())
}

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		return m, m.quit()
	}
	switch m.mode {
	case mHelp:
		m.mode = mNormal
		return m, nil
	case mConfirm:
		return m.keyConfirm(s)
	case mInput:
		return m.keyInput(k)
	case mDraft:
		return m.keyDraft(k)
	case mPicker:
		return m.keyPicker(s)
	case mAccounts:
		return m.keyAccounts(k)
	case mAssistant:
		return m.keyAssistant(k)
	case mAttView:
		switch s {
		case "esc", "q", "h", "left":
			m.mode = mPicker
			if m.picker == nil {
				m.mode = mNormal
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.attVP, cmd = m.attVP.Update(k)
		return m, cmd
	}
	if !m.ready {
		switch s {
		case "q":
			return m, m.quit()
		case "f5":
			m.err, m.status = "", "connecting to Thunderbird…"
			return m, m.Init()
		}
		return m, nil
	}

	// global keys
	switch s {
	case "q":
		return m, m.quit()
	case "?":
		m.mode = mHelp
		return m, nil
	case "tab", "shift+tab":
		m.focus = 1 - m.focus
		return m, nil
	case "1", "2":
		m.focus = pane(s[0] - '1')
		return m, nil
	case "l":
		return m.openFolderPicker()
	case "ctrl+a":
		return m.openAccounts()
	case "v":
		m.sidebar = !m.sidebar
		m.layout()
		cmd := m.refreshPreview(false)
		if !m.sidebarOn() && m.sidebar {
			m.status = "sidebar needs a wider terminal"
		}
		return m, cmd
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "alt+7", "alt+8", "alt+9":
		return m.openTab(int(s[4] - '1'))
	case "[", "]":
		n := len(m.acctOrder) + 1
		return m.openTab((m.tab + map[string]int{"[": n - 1, "]": 1}[s]) % n)
	case "/":
		return m.startSearch()
	case ":", "alt+0":
		return m.openAssistant()
	case "c":
		return m.startCompose(tb.Draft{Kind: tb.NewMail, From: m.defaultIdentity()})
	case "u":
		m.unread = !m.unread
		m.status = map[bool]string{true: "unread only", false: "all messages"}[m.unread]
		return m, m.loadList(false)
	case "f5":
		return m, tea.Batch(m.loadFolders(), m.loadList(false))
	case "T":
		m.status = "opening Thunderbird…"
		c, ctx := m.c, m.ctx
		return m, func() tea.Msg {
			err := c.OpenWindow(ctx, func(s string) { go prog.Send(statusMsg(s)) })
			return doneMsg{text: "Thunderbird window open", err: err}
		}
	case "C":
		return m.markAllRead()
	case "n", "p":
		return m.nextUnread(s == "n")
	case "esc":
		if len(m.marked) > 0 {
			m.marked, m.status = nil, "selection cleared"
			return m, nil
		}
		if m.backToAsst { // opened from the Assistant's Found list
			return m.backToAssistant()
		}
		if m.view.back != nil { // leave a thread view for where it was opened
			return m.show(*m.view.back)
		}
		if m.view.folderIDs == nil && m.prevView.title != "" { // leave search results
			return m.show(m.prevView)
		}
	}

	focus := m.focus
	if s == "pgdown" || s == "pgup" { // page keys always scroll the open message
		focus = pDetail
	}
	switch focus {
	case pMessages:
		return m.keyMessages(s)
	default:
		var cmd tea.Cmd
		before := m.preview.YOffset
		m.preview, cmd = m.preview.Update(k)
		if s == "h" || s == "left" {
			m.focus = pMessages
		}
		if m.preview.YOffset != before && m.inlineSharp() {
			off := m.preview.YOffset // encode the newly visible image parts once scrolling pauses
			cmd = tea.Batch(cmd, tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return sixelTick{off} }))
		}
		return m.keyMessageActions(s, cmd)
	}
}

func move(cur, n int, s string, page int) int {
	switch s {
	case "j", "down":
		cur++
	case "k", "up":
		cur--
	case "g", "home":
		cur = 0
	case "G", "end":
		cur = n - 1
	case "ctrl+d", "pgdown":
		cur += page
	case "ctrl+u", "pgup":
		cur -= page
	}
	return max(0, min(cur, n-1))
}

func (m Model) keyMessages(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "enter", "right":
		m.focus = pDetail
		if x, ok := m.selected(); ok && !x.Read {
			m.msgs[m.mcur].Read = true
			c, ctx := m.c, m.ctx
			return m, m.act("", nil, func() error { return c.Mark(ctx, []int{x.ID}, "read") })
		}
		return m, nil
	case " ":
		x, ok := m.selected()
		if !ok {
			return m, nil
		}
		if m.marked == nil {
			m.marked = map[int]bool{}
		}
		if m.marked[x.ID] {
			delete(m.marked, x.ID)
		} else {
			m.marked[x.ID] = true
		}
		s = "j" // and move on, like a file manager
	}
	old := m.mcur
	m.mcur = move(m.mcur, len(m.msgs), s, m.listHeight()/2)
	var cmds []tea.Cmd
	if m.mcur != old {
		cmds = append(cmds, m.schedulePreview())
	}
	if m.mcur == len(m.msgs)-1 && m.hasMore && m.mcur != old {
		m.hasMore = false // one page request in flight at a time
		cmds = append(cmds, m.loadList(true))
	}
	mm, cmd := m.keyMessageActions(s, nil) // movement and action keys don't overlap
	return mm, tea.Batch(append(cmds, cmd)...)
}

// targets is what bulk-capable actions apply to: the space-selected messages
// in list order, or else the message under the cursor.
func (m Model) targets() (idx []int, ids []int) {
	for i, x := range m.msgs {
		if m.marked[x.ID] {
			idx, ids = append(idx, i), append(ids, x.ID)
		}
	}
	if len(ids) == 0 && m.mcur < len(m.msgs) {
		idx, ids = []int{m.mcur}, []int{m.msgs[m.mcur].ID}
	}
	return idx, ids
}

// count describes n messages for status and confirm texts.
func count(n int) string {
	if n == 1 {
		return "1 message"
	}
	return fmt.Sprintf("%d messages", n)
}

// markAllRead marks every message in the current folder(s) read; for search
// results, just the listed ones.
func (m Model) markAllRead() (tea.Model, tea.Cmd) {
	c, ctx := m.c, m.ctx
	if m.view.folderIDs == nil {
		var ids []int
		for i, x := range m.msgs {
			if !x.Read {
				ids = append(ids, x.ID)
				m.msgs[i].Read = true
			}
		}
		if len(ids) == 0 {
			m.status = "nothing unread"
			return m, nil
		}
		return m, m.act(fmt.Sprintf("marked %s read", count(len(ids))), nil, func() error { return c.Mark(ctx, ids, "read") })
	}
	folders := m.view.folderIDs
	m.confirm = &confirmation{text: fmt.Sprintf("Mark everything in %s as read?", m.view.title),
		onYes: func() tea.Msg {
			for _, f := range folders {
				if err := c.MarkFolderRead(ctx, f); err != nil {
					return doneMsg{err: err, reload: true}
				}
			}
			return doneMsg{text: "marked all read", reload: true}
		}}
	m.mode = mConfirm
	return m, nil
}

// nextUnread moves the cursor to the next (or previous) unread loaded message.
func (m Model) nextUnread(forward bool) (tea.Model, tea.Cmd) {
	step := map[bool]int{true: 1, false: -1}[forward]
	for i := m.mcur + step; i >= 0 && i < len(m.msgs); i += step {
		if !m.msgs[i].Read {
			m.mcur = i
			return m, m.schedulePreview()
		}
	}
	if forward && m.hasMore {
		m.hasMore = false // one page request in flight at a time
		m.status = "loading more… press n again"
		return m, m.loadList(true)
	}
	m.status = "no more unread"
	return m, nil
}

// keyMessageActions handles keys that act on the selected message, from the list or preview.
func (m Model) keyMessageActions(s string, passthrough tea.Cmd) (tea.Model, tea.Cmd) {
	x, ok := m.selected()
	if !ok {
		return m, passthrough
	}
	c, ctx := m.c, m.ctx
	idx, ids := m.targets()
	switch s {
	case "m", "S":
		field := func(y *tb.Message) *bool { return &y.Read }
		names := [2]string{"read", "unread"}
		if s == "S" {
			field, names = func(y *tb.Message) *bool { return &y.Flagged }, [2]string{"flagged", "unflagged"}
		}
		set := false // any not set yet: set them all, else clear them all
		for _, i := range idx {
			set = set || !*field(&m.msgs[i])
		}
		flag := names[1]
		if set {
			flag = names[0]
		}
		for _, i := range idx {
			*field(&m.msgs[i]) = set
		}
		return m, m.act("marked "+count(len(ids))+" "+flag, nil, func() error { return c.Mark(ctx, ids, flag) })
	case "a":
		return m, m.act("archived "+count(len(ids)), ids, func() error { return c.Archive(ctx, ids) })
	case "d", "delete":
		text := fmt.Sprintf("Move %q to trash?", m.msgs[idx[0]].Subject) // the target, not the cursor: space moves it on
		if len(ids) > 1 {
			text = fmt.Sprintf("Move %s to trash?", count(len(ids)))
		}
		m.confirm = &confirmation{text: text,
			onYes: m.act("moved "+count(len(ids))+" to trash", ids, func() error { return c.Delete(ctx, ids) })}
		m.mode = mConfirm
		return m, nil
	case "M":
		acct := m.msgs[idx[0]].Folder.AccountID
		for _, i := range idx {
			if m.msgs[i].Folder.AccountID != acct {
				m.status = "selection spans accounts: move them per account"
				return m, nil
			}
		}
		m.moveIDs = ids
		return m.openMovePicker(acct)
	case "r", "R", "f":
		kind := map[string]tb.DraftKind{"r": tb.Reply, "R": tb.ReplyAll, "f": tb.Forward}[s]
		return m.startCompose(tb.Draft{Kind: kind, ReplyTo: x.ID})
	case "o":
		if m.full == nil || m.full.ID != x.ID {
			m.status = "open the message first"
			return m, nil
		}
		return m.openLinkPicker()
	case "y":
		if m.full == nil || m.full.ID != x.ID {
			m.status = "open the message first"
			return m, nil
		}
		return m.copyCodes()
	case "t":
		if m.full == nil || m.full.ID != x.ID {
			m.status = "open the message first"
			return m, nil
		}
		return m.openThreadPicker()
	case "A":
		if m.full == nil || m.full.ID != x.ID {
			m.status = "open the message first"
			return m, nil
		}
		return m.openAttachmentPicker()
	case "i":
		if m.full == nil || m.full.ID != x.ID {
			m.status = "open the message first"
			return m, nil
		}
		files := m.images.files(x.ID)
		if len(files) == 0 {
			m.status = "no images in this message"
			return m, nil
		}
		return m, tea.Exec(viewerCmd{files}, func(err error) tea.Msg { return viewerDoneMsg{err} })
	case "U":
		return m.startUnsubscribe(x)
	case "z":
		mm, cmd := m.openAssistant()
		if mm.mode != mAssistant {
			return mm, cmd
		}
		return mm.ask(fmt.Sprintf("summarize %q", bg.OneLine(x.Subject)),
			fmt.Sprintf("Summarize message id %d. If it is part of a thread, read the thread first and summarize the whole conversation. Then list any action items for me.", x.ID))
	}
	return m, passthrough
}

func (m Model) startSearch() (tea.Model, tea.Cmd) {
	m.mode = mInput
	m.input.Placeholder = "search all accounts"
	m.input.SetValue("")
	return m, m.input.Focus()
}

func (m Model) keyInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.mode = m.modeAfterConfirm()
		m.input.Blur()
		return m, nil
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		m.mode = m.modeAfterConfirm()
		m.input.Blur()
		if v == "" {
			return m, nil
		}
		if m.view.folderIDs != nil {
			m.prevView = m.view
		}
		m.focus = pMessages
		return m.show(listView{title: fmt.Sprintf("Search: %s · all mail (esc back)", v), query: v})
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

func (m Model) viewContext() string {
	var b strings.Builder
	switch {
	case m.view.thread != nil:
		fmt.Fprintf(&b, "a conversation (%d messages): %s", len(m.view.thread), m.view.title)
	case m.view.found != nil:
		fmt.Fprintf(&b, "the %d messages you found earlier", len(m.view.found))
	case m.view.folderIDs == nil:
		fmt.Fprintf(&b, "search results for %q", m.view.query)
	default:
		fmt.Fprintf(&b, "%s (folders %s)", m.view.title, strings.Join(m.view.folderIDs, ", "))
	}
	if x, ok := m.selected(); ok {
		fmt.Fprintf(&b, "; selected message id %d from %s, subject %q", x.ID, x.Author, x.Subject)
	}
	return b.String()
}

func (m Model) keyConfirm(s string) (tea.Model, tea.Cmd) {
	c := m.confirm
	yes := s == "y" || s == "Y"
	if !yes && s != "n" && s != "N" && s != "esc" {
		return m, nil
	}
	m.confirm = nil
	m.mode = m.modeAfterConfirm()
	if c.reply != nil {
		c.reply <- yes // buffered: never blocks even if the agent already gave up
		return m, nil
	}
	if yes {
		return m, c.onYes
	}
	if s != "esc" { // with onNo, esc only closes the question
		return m, c.onNo
	}
	return m, nil
}

func (m Model) modeAfterConfirm() mode {
	switch {
	case m.draft != nil:
		return mDraft // confirmations raised from compose mode return to it
	case m.picker != nil:
		return mPicker
	case m.accts != nil:
		return mAccounts
	case m.asst.open:
		return mAssistant // the Assistant's y/n, or compose opened from it
	}
	return mNormal
}

// closeDraft ends the current draft and shows a queued agent draft, if any.
func (m Model) closeDraft() Model {
	if m.draftFile != "" {
		os.Remove(m.draftFile)
	}
	m.draft, m.draftFile = nil, ""
	m.mode = m.modeAfterConfirm() // back to a picker or the accounts screen it opened over
	if p := m.pending; p != nil {
		m.pending = nil
		return m.openDraft(*p)
	}
	return m
}

// edit opens the draft in $VISUAL/$EDITOR, reusing a file that failed to parse.
func (m *Model) edit() tea.Cmd {
	path := m.draftFile
	if path == "" {
		f, err := os.CreateTemp("", "lazythunder-*.eml.txt")
		if err != nil {
			m.err = err.Error()
			return nil
		}
		f.WriteString(renderDraft(*m.draft, m.draftCtx))
		f.Close()
		path = f.Name()
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	// the editor gets normal autowrap; the next frame turns it off again
	parts := append([]string{"-c", `printf '\033[?7h'; exec "$@"`, "sh"}, append(strings.Fields(editor), path)...)
	return tea.ExecProcess(exec.Command("sh", parts...), func(err error) tea.Msg {
		return editedMsg{path, err}
	})
}

func (m Model) afterEdit(msg editedMsg) (tea.Model, tea.Cmd) {
	if m.draft == nil { // discarded meanwhile
		os.Remove(msg.path)
		return m, nil
	}
	m.mode, m.draftFile = mDraft, msg.path // keep the file until it parses
	if msg.err != nil {
		m.err = "editor: " + msg.err.Error() + " (ctrl+e to edit)"
		return m, nil
	}
	raw, err := os.ReadFile(msg.path)
	if err != nil {
		m.err = err.Error()
		return m, nil
	}
	d, err := parseDraft(*m.draft, string(raw))
	if err != nil {
		m.err = "draft: " + err.Error() + " (ctrl+e to edit)"
		return m, nil
	}
	os.Remove(msg.path)
	m.draftFile, m.err = "", ""
	if d != *m.draft { // a hand edit is a version too: ctrl+z undoes it, the agent builds on it
		m.draftUndo, m.draftAsks = append(m.draftUndo, *m.draft), append(m.draftAsks, editedByHand)
		m.draft = &d
		m.draftChat = append(m.draftChat, fmt.Sprintf("✦ edited in your editor (v%d)", m.version()))
	}
	return m, nil
}

// contextFor describes the message a draft responds to, for the editor comments.
func (m Model) contextFor(d tb.Draft) string {
	if d.Kind == tb.NewMail {
		return ""
	}
	for _, x := range m.msgs {
		if x.ID == d.ReplyTo {
			return fmt.Sprintf("%s to: %s\nSubject: %s\nDate: %s", kindName(d.Kind), x.Author, x.Subject, x.Date.Local().Format("2006-01-02 15:04"))
		}
	}
	return fmt.Sprintf("%s to message %d", kindName(d.Kind), d.ReplyTo)
}

// defaultIdentity sends from the selected message's account, else the tab's.
func (m Model) defaultIdentity() string {
	acct := m.view.accountID
	if x, ok := m.selected(); ok {
		acct = x.Folder.AccountID
	}
	if acct == "" {
		acct = m.tabAccount()
	}
	if a, ok := m.accounts[acct]; ok && len(a.Identities) > 0 {
		return a.Identities[0].Email
	}
	return ""
}

func (m Model) model() string { return m.ag.Name() }
