// Package ui is the lazygit-style terminal interface.
package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"golang.org/x/sys/unix"

	"github.com/davidhyldgaard/lazythunder/internal/agent"
	"github.com/davidhyldgaard/lazythunder/internal/render"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
	"github.com/davidhyldgaard/lazythunder/internal/termimg"
)

type pane int

const (
	pMessages pane = iota
	pDetail
)

type mode int

const (
	mNormal  mode = iota
	mInput        // text input at the bottom (search or agent prompt)
	mConfirm      // y/n question
	mDraft        // reviewing a draft
	mPicker       // link or attachment picker in the detail pane
	mAttView      // previewing an attachment in the detail pane
	mHelp
)

const pageSize = 100

// prog lets background goroutines (the agent) talk to the UI.
var prog *tea.Program

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
	unread    bool     // unread-only filter

	msgs       []tb.Message
	mcur       int
	hasMore    bool
	before     time.Time // date cursor for the next page
	listSeq    int
	searchLeft int // search halves still running

	preview    viewport.Model
	full       *tb.FullMessage
	images     *imageStore
	sum        *summarizer
	sidebar    bool // b toggles the message sidebar (summary, actions, links, attachments)
	attPrev    *attPreview
	attCancel  context.CancelFunc
	cellW      int // terminal cell size in px (0 if unknown)
	cellH      int
	placements []render.Placement // inline images in the preview, for sixel overlay
	links      []link             // links in the open message, for the picker

	viewGen      int // bumped when the list changes view; stale async results are dropped
	viewCtx      context.Context
	cancelView   context.CancelFunc
	replied      map[int]replyInfo
	reply        *loadedReply
	replyLoading int

	picker     *picker
	attVP      viewport.Model
	attTitle   string
	previewSeq int

	showAgent  bool
	chat       []string
	agentVP    viewport.Model
	agentBusy  bool
	agentState string
	agentStop  context.CancelFunc

	input       textinput.Model
	replyTarget int    // message id the w prompt is about
	inputFor    string // "search" or "agent"
	confirm     *confirmation
	draft       *tb.Draft
	draftCtx    string
	draftFile   string    // edited file kept after a parse error so the text isn't lost
	submitting  bool      // a save/send is in flight
	pending     *tb.Draft // agent draft waiting for the current draft to finish
	moveIDs     []int     // messages the move picker will move
}

// listView is what the message list shows: one or more folders, or a search.
type listView struct {
	title     string
	folderIDs []string // nil for search results
	accountID string   // "" when merged across accounts
	query     string
}

type confirmation struct {
	text  string
	reply chan bool // agent confirmations
	onYes tea.Cmd   // local confirmations
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
		err    error
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
	}
	agentReplyMsg struct {
		text string
		err  error
	}
	agentStatusMsg string
	confirmMsg     struct {
		text  string
		reply chan bool
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
		preview: newViewport(), agentVP: newViewport(), attVP: newViewport(), status: "connecting to Thunderbird…",
		images: newImageStore(), replied: map[int]replyInfo{}, sum: newSummarizer(ag, c), sidebar: true}
	defer m.images.close()
	prog = tea.NewProgram(m, tea.WithAltScreen())
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go m.sum.run(ctx, func(hdr string) { prog.Send(summaryMsg{hdr}) })
	_, err := prog.Run()
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
		return readyMsg{err}
	}
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
	seq, unread, ctx, c := m.listSeq, m.unread, m.ctx, m.c
	if m.view.folderIDs == nil {
		q := m.view.query
		m.msgs, m.mcur, m.searchLeft = nil, 0, 2
		// Sender and subject hits (fast, all time) come first; body hits from the
		// slower full-text index are merged in when they arrive.
		headers := func() tea.Msg {
			ms, err := c.SearchHeaders(ctx, q, unread)
			return listMsg{seq: seq, msgs: ms, err: err}
		}
		content := func() tea.Msg {
			// shortcut: one page of full-text hits in the last 90 days; tb search has no offset paging
			p, err := c.Search(ctx, q, tb.SearchOpts{Unread: unread, Limit: pageSize})
			return listMsg{seq: seq, msgs: p.Messages, err: err}
		}
		return tea.Batch(headers, content)
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
			m.status = "not connected (ctrl+r to retry, q to quit)"
			return m, nil
		}
		m.ready, m.status = true, "connected"
		return m, m.loadFolders()
	case foldersMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.folders = m.folders[:0]
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
			return mm, tea.Batch(cmd, mm.(Model).pollNewMail())
		}
		m.tab = min(m.tab, len(m.acctOrder))
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
		if m.view.folderIDs == nil { // search: the two halves arrive in either order
			if m.searchLeft == 2 {
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
		}
		m.hasMore, m.before = msg.more, msg.next
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
		m.sum.enqueue(x.ID, x.HeaderMessageID, true) // resting on it: summarize this one next
		if x.Read {
			return m, nil
		}
		m.msgs[m.mcur].Read = true
		c, ctx := m.c, m.ctx
		return m, m.act("", nil, func() error { return c.Mark(ctx, []int{x.ID}, "read") })
	case previewMsg:
		if msg.seq != m.previewSeq {
			return m, nil
		}
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.full = &msg.m
		cmd := m.refreshPreview(true)
		reply := m.maybeLoadReply()
		att := m.startAttPreview(msg.m)
		return m, tea.Batch(cmd, reply, att)
	case sidebarAttMsg:
		if m.full == nil || m.full.ID != msg.p.msgID {
			return m, nil
		}
		p := msg.p
		m.attPrev = &p
		if m.sidebarOn() && m.inlineSixel() {
			if _, thumb := m.sidebarLines(m.sidebarWidth(), m.preview.Height); thumb != nil {
				return m, encodeSixels(m.images, []sixelReq{*thumb}, m.cellW, m.cellH)
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
	case summaryMsg:
		return m, nil // the sidebar reads the summarizer on render
	case pollTick:
		return m, m.pollNewMail()
	case newMailMsg:
		if msg.n > 0 {
			m.status = fmt.Sprintf("%d new message(s) · ctrl+r to refresh", msg.n)
			return m, tea.Batch(m.loadFolders(), nextPoll())
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
		return m, encodeSixels(m.images, m.visibleSixels(), m.cellW, m.cellH)
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
			return m, tea.Exec(viewerCmd{[]string{msg.image}}, func(err error) tea.Msg { return noteMsg{"", err} })
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
			return m, nil
		}
		m.err = ""
		if msg.text != "" {
			m.status = msg.text
		}
		if len(msg.remove) > 0 {
			gone := map[int]bool{}
			for _, id := range msg.remove {
				gone[id] = true
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
	case agentStatusMsg:
		m.agentState = string(msg)
		return m, nil
	case agentReplyMsg:
		m.agentBusy, m.agentState = false, ""
		m.sum.paused.Store(false)
		if m.confirm != nil && m.confirm.reply != nil { // agent ended (e.g. cancelled) while asking
			m.confirm, m.mode = nil, m.modeAfterConfirm()
		}
		if msg.err != nil {
			m.say("error: " + msg.err.Error())
		} else {
			m.say("borealis2: " + msg.text)
		}
		return m, nil
	case confirmMsg:
		m.confirm = &confirmation{text: msg.text, reply: msg.reply}
		m.mode = mConfirm
		return m, nil
	case draftMsg:
		d := msg.d
		if m.draft != nil {
			m.pending = &d
			m.status = "borealis2 has a draft ready; it opens when you finish this one"
			return m, nil
		}
		return m.openDraft(d), nil
	case editedMsg:
		return m.afterEdit(msg)
	case sendingMsg:
		m.submitting, m.status = true, "sending…"
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
	if m.inlineSixel() {
		reqs := m.visibleSixels()
		if m.sidebarOn() {
			if _, thumb := m.sidebarLines(m.sidebarWidth(), m.preview.Height); thumb != nil {
				reqs = append(reqs, *thumb) // resize/toggle changes its size: re-encode
			}
		}
		cmds = append(cmds, encodeSixels(m.images, reqs, m.cellW, m.cellH))
	}
	if len(misses) > 0 {
		cmds = append(cmds, fetchImages(m.ctx, m.c, m.images, m.full.ID, misses))
	}
	return tea.Batch(cmds...)
}

// inlineSixel: draw sharp images in the preview (sixel terminals that report
// their cell size). LAZYTHUNDER_GRAPHICS=blocks turns it off.
func (m Model) inlineSixel() bool {
	return termimg.Protocol() == "sixel" && m.cellW > 0 && m.cellH > 0
}

func (m *Model) say(line string) {
	m.chat = append(m.chat, line)
	m.agentVP.SetContent(m.renderChat())
	m.agentVP.GotoBottom()
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
	m.searchLeft--
	m.status = fmt.Sprintf("%d messages", len(m.msgs))
	if m.searchLeft > 0 {
		m.status += " · searching message text…"
	}
	cmds := []tea.Cmd{m.loadReplyStatus(msg.msgs)}
	if cur == 0 || len(m.msgs) > 0 && m.msgs[m.mcur].ID != cur {
		cmds = append(cmds, m.schedulePreview())
	}
	return m, tea.Batch(cmds...)
}

func (m Model) show(v listView) (tea.Model, tea.Cmd) {
	m.view, m.msgs, m.mcur = v, nil, 0
	m.viewGen++
	if m.cancelView != nil {
		m.cancelView() // stop reply-status lookups for the old view
	}
	m.viewCtx, m.cancelView = context.WithCancel(m.ctx)
	m.replied = map[int]replyInfo{}
	m.status = "loading " + v.title + "…"
	return m, tea.Batch(m.loadList(false), m.schedulePreview())
}

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		return m, tea.Quit
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
		return m.keyDraft(s)
	case mPicker:
		return m.keyPicker(s)
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
			return m, tea.Quit
		case "ctrl+r":
			m.err, m.status = "", "connecting to Thunderbird…"
			return m, m.Init()
		}
		return m, nil
	}

	// global keys
	switch s {
	case "q":
		return m, tea.Quit
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
	case "b":
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
		return m.startInput("search", "search all accounts", "")
	case "a":
		m.showAgent, m.focus = true, pDetail
		return m.startInput("agent", "ask borealis2", "")
	case "p":
		m.showAgent = false
		return m, nil
	case "ctrl+l":
		if m.agentBusy {
			m.status = "borealis2 is busy (esc to cancel)"
			return m, nil
		}
		m.ag.Reset()
		m.chat = nil
		m.say("(new conversation)")
		return m, nil
	case "c":
		m = m.openDraft(tb.Draft{Kind: tb.NewMail, From: m.defaultIdentity()})
		return m, m.edit()
	case "u":
		m.unread = !m.unread
		m.status = map[bool]string{true: "unread only", false: "all messages"}[m.unread]
		return m, m.loadList(false)
	case "ctrl+r":
		return m, tea.Batch(m.loadFolders(), m.loadList(false))
	case "esc":
		if m.agentBusy && m.agentStop != nil {
			m.agentStop()
			return m, nil
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
		vp := &m.preview
		if m.showAgent {
			vp = &m.agentVP
		}
		var cmd tea.Cmd
		before := vp.YOffset
		*vp, cmd = vp.Update(k)
		if s == "h" || s == "left" {
			m.focus = pMessages
		}
		if !m.showAgent && m.preview.YOffset != before && m.inlineSixel() {
			off := m.preview.YOffset // encode the newly visible image parts once scrolling pauses
			cmd = tea.Batch(cmd, tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return sixelTick{off} }))
		}
		if !m.showAgent {
			return m.keyMessageActions(s, cmd)
		}
		return m, cmd
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
		m.showAgent = false
		if x, ok := m.selected(); ok && !x.Read {
			m.msgs[m.mcur].Read = true
			c, ctx := m.c, m.ctx
			return m, m.act("", nil, func() error { return c.Mark(ctx, []int{x.ID}, "read") })
		}
		return m, nil
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

// keyMessageActions handles keys that act on the selected message, from the list or preview.
func (m Model) keyMessageActions(s string, passthrough tea.Cmd) (tea.Model, tea.Cmd) {
	x, ok := m.selected()
	if !ok {
		return m, passthrough
	}
	c, ctx, ids := m.c, m.ctx, []int{x.ID}
	switch s {
	case "m":
		flag := map[bool]string{true: "unread", false: "read"}[x.Read]
		m.msgs[m.mcur].Read = !x.Read
		return m, m.act("marked "+flag, nil, func() error { return c.Mark(ctx, ids, flag) })
	case "F", "*":
		flag := map[bool]string{true: "unflagged", false: "flagged"}[x.Flagged]
		m.msgs[m.mcur].Flagged = !x.Flagged
		return m, m.act(flag, nil, func() error { return c.Mark(ctx, ids, flag) })
	case "e":
		return m, m.act("archived", ids, func() error { return c.Archive(ctx, ids) })
	case "d":
		m.confirm = &confirmation{text: fmt.Sprintf("Move %q to trash?", x.Subject),
			onYes: m.act("moved to trash", ids, func() error { return c.Delete(ctx, ids) })}
		m.mode = mConfirm
		return m, nil
	case "M":
		m.moveIDs = ids
		return m.openMovePicker(x.Folder.AccountID)
	case "r", "R", "f":
		kind := map[string]tb.DraftKind{"r": tb.Reply, "R": tb.ReplyAll, "f": tb.Forward}[s]
		m = m.openDraft(tb.Draft{Kind: kind, ReplyTo: x.ID})
		return m, m.edit()
	case "o":
		if m.full == nil || m.full.ID != x.ID {
			m.status = "open the message first"
			return m, nil
		}
		return m.openLinkPicker()
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
		return m, tea.Exec(viewerCmd{files}, func(err error) tea.Msg {
			if err != nil {
				return statusMsg("image viewer: " + err.Error())
			}
			return nil
		})
	case "U":
		return m.startUnsubscribe(x)
	case "s":
		return m.askAgent(fmt.Sprintf("Summarize message id %d. If it is part of a thread, read the thread first and summarize the whole conversation. Then list any action items for me.", x.ID))
	case "w":
		m.replyTarget = x.ID
		return m.startInput("reply", "what should the reply say? (short, e.g. \"yes, friday works\")", "")
	}
	return m, passthrough
}

func (m Model) startInput(purpose, placeholder, value string) (tea.Model, tea.Cmd) {
	if (purpose == "agent" || purpose == "reply") && m.agentBusy {
		m.status = "borealis2 is busy (esc to cancel)"
		return m, nil
	}
	m.mode, m.inputFor = mInput, purpose
	m.input.Placeholder = placeholder
	m.input.SetValue(value)
	m.input.CursorEnd()
	return m, m.input.Focus()
}

func (m Model) keyInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.mode = mNormal
		m.input.Blur()
		return m, nil
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		m.mode = mNormal
		m.input.Blur()
		if v == "" {
			return m, nil
		}
		if m.inputFor == "search" {
			if m.view.folderIDs != nil {
				m.prevView = m.view
			}
			m.focus = pMessages
			return m.show(listView{title: fmt.Sprintf("Search: %s · sender/subject: all · text: last %s (esc back)", v, tb.SearchWindow), query: v})
		}
		if m.inputFor == "reply" {
			return m.askAgent(fmt.Sprintf("Write a reply to message id %d. What I want to say, in short: %s\n"+
				"Read the message first (and the thread if there is one), then call propose_draft with kind \"reply\", "+
				"message_id %d and the finished body, following the reply guide.", m.replyTarget, v, m.replyTarget))
		}
		return m.askAgent(v)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

func (m Model) askAgent(q string) (tea.Model, tea.Cmd) {
	if m.agentBusy {
		m.status = "borealis2 is busy (esc to cancel)"
		return m, nil
	}
	m.showAgent, m.agentBusy = true, true
	m.sum.paused.Store(true) // one model slot: the chat goes first
	m.say("you: " + q)
	ctx, cancel := context.WithCancel(m.ctx)
	m.agentStop = cancel
	view := m.viewContext()
	ag := m.ag
	hooks := agent.Hooks{
		Status:  func(s string) { prog.Send(agentStatusMsg(s)) },
		Propose: func(d tb.Draft) { prog.Send(draftMsg{d}) },
		Confirm: func(text string) bool {
			reply := make(chan bool, 1)
			prog.Send(confirmMsg{text, reply})
			select {
			case ok := <-reply:
				return ok
			case <-ctx.Done():
				return false
			}
		},
	}
	return m, func() tea.Msg {
		defer cancel()
		text, err := ag.Ask(ctx, q, view, hooks)
		return agentReplyMsg{text, err}
	}
}

func (m Model) viewContext() string {
	var b strings.Builder
	switch {
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
	return m, nil
}

func (m Model) modeAfterConfirm() mode {
	switch {
	case m.draft != nil:
		return mDraft // confirmations raised from the draft screen return to it
	case m.picker != nil:
		return mPicker
	}
	return mNormal
}

func (m Model) openDraft(d tb.Draft) Model {
	m.draft, m.draftCtx, m.draftFile, m.mode = &d, m.contextFor(d), "", mDraft
	return m
}

// closeDraft ends the current draft and shows a queued agent draft, if any.
func (m Model) closeDraft() Model {
	if m.draftFile != "" {
		os.Remove(m.draftFile)
	}
	m.draft, m.draftFile, m.mode = nil, "", mNormal
	if p := m.pending; p != nil {
		m.pending = nil
		return m.openDraft(*p)
	}
	return m
}

func (m Model) keyDraft(s string) (tea.Model, tea.Cmd) {
	if m.draft == nil {
		m.mode = mNormal
		return m, nil
	}
	if m.submitting {
		m.status = "still saving…"
		return m, nil
	}
	d := *m.draft
	c, ctx := m.c, m.ctx
	submit := func(send bool) tea.Cmd {
		return func() tea.Msg { return submittedMsg{send, c.Submit(ctx, d, send)} }
	}
	switch s {
	case "e":
		return m, m.edit()
	case "enter", "D", "S":
		if m.draftFile != "" {
			m.err = "fix the draft first (e to edit)"
			return m, nil
		}
		if err := validDraft(d); err != nil {
			m.err = "draft: " + err.Error() + " (e to edit)"
			return m, nil
		}
		if s != "S" {
			m.submitting, m.status = true, "saving draft…"
			return m, submit(false)
		}
		m.confirm = &confirmation{text: "Send this message now?", onYes: tea.Sequence(
			func() tea.Msg { return sendingMsg{} }, submit(true))}
		m.mode = mConfirm
		return m, nil
	case "x", "esc":
		m = m.closeDraft()
		if m.draft == nil {
			m.status = "draft discarded"
		}
		return m, nil
	}
	return m, nil
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
		m.err = "editor: " + msg.err.Error() + " (e to edit)"
		return m, nil
	}
	raw, err := os.ReadFile(msg.path)
	if err != nil {
		m.err = err.Error()
		return m, nil
	}
	d, err := parseDraft(*m.draft, string(raw))
	if err != nil {
		m.err = "draft: " + err.Error() + " (e to edit)"
		return m, nil
	}
	os.Remove(msg.path)
	m.draft, m.draftFile, m.err = &d, "", ""
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
