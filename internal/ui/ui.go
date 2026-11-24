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

	"github.com/davidhyldgaard/lazythunder/internal/agent"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
)

type pane int

const (
	pFolders pane = iota
	pMessages
	pDetail
)

type mode int

const (
	mNormal   mode = iota
	mInput         // text input at the bottom (search or agent prompt)
	mConfirm       // y/n question
	mDraft         // reviewing a draft
	mMovePick      // picking a target folder in the folder pane
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
	fcur      int // index into visibleFolders()
	view      listView
	prevView  listView // restored when leaving search results
	unread    bool     // unread-only filter

	msgs    []tb.Message
	mcur    int
	hasMore bool
	before  time.Time // date cursor for the next page
	listSeq int

	preview    viewport.Model
	full       *tb.FullMessage
	images     *imageStore
	previewSeq int

	showAgent  bool
	chat       []string
	agentVP    viewport.Model
	agentBusy  bool
	agentState string
	agentStop  context.CancelFunc

	input      textinput.Model
	inputFor   string // "search" or "agent"
	confirm    *confirmation
	draft      *tb.Draft
	draftCtx   string
	draftFile  string    // edited file kept after a parse error so the text isn't lost
	submitting bool      // a save/send is in flight
	pending    *tb.Draft // agent draft waiting for the current draft to finish
	moveIDs    []int
	moveAcct   string // account whose folders the move picker shows
	moveFcur   int    // folder cursor to restore after moving
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
		preview: newViewport(), agentVP: newViewport(), status: "connecting to Thunderbird…", images: newImageStore()}
	defer m.images.close()
	prog = tea.NewProgram(m, tea.WithAltScreen())
	_, err := prog.Run()
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
		return func() tea.Msg {
			// shortcut: one page of search hits; tb search has no offset paging
			p, err := c.Search(ctx, q, tb.SearchOpts{Unread: unread, Limit: pageSize})
			return listMsg{seq: seq, msgs: p.Messages, err: err}
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

// schedulePreview debounces preview loads while the cursor moves.
func (m *Model) schedulePreview() tea.Cmd {
	m.previewSeq++
	if len(m.msgs) == 0 {
		m.full = nil
		m.preview.SetContent("")
		return nil
	}
	seq, id := m.previewSeq, m.msgs[m.mcur].ID
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return previewTick{seq, id} })
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
			return m.openTab(0)
		}
		m.tab = min(m.tab, len(m.acctOrder))
		m.fcur = min(m.fcur, max(0, len(m.visibleFolders())-1))
		return m, nil
	case listMsg:
		if msg.seq != m.listSeq {
			return m, nil
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
		if msg.append {
			return m, nil
		}
		return m, m.schedulePreview()
	case previewTick:
		if msg.seq != m.previewSeq {
			return m, nil
		}
		c, ctx := m.c, m.ctx
		return m, func() tea.Msg {
			full, err := c.Read(ctx, msg.id)
			return previewMsg{seq: msg.seq, m: full, err: err}
		}
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
		return m, cmd
	case imagesMsg:
		// refresh whatever is open: it may share image URLs with msg.msgID
		cmd := m.refreshPreview(false)
		return m, cmd
	case doneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err, m.status = "", msg.text
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
	content, misses := m.renderMessage(*m.full)
	off := m.preview.YOffset
	m.preview.SetContent(content)
	if top {
		m.preview.GotoTop()
	} else {
		m.preview.SetYOffset(off)
	}
	if len(misses) == 0 {
		return nil
	}
	return fetchImages(m.ctx, m.c, m.images, m.full.ID, misses)
}

func (m *Model) say(line string) {
	m.chat = append(m.chat, line)
	m.agentVP.SetContent(m.renderChat())
	m.agentVP.GotoBottom()
}

// visibleFolders is the folder pane's content: the move target account's
// folders while picking, every inbox on the All tab, else the tab's account.
func (m Model) visibleFolders() []tb.Folder {
	var out []tb.Folder
	acct := m.tabAccount()
	if m.mode == mMovePick {
		acct = m.moveAcct
	}
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
	m.tab, m.fcur = i, 0
	if i == 0 {
		ids := []string{} // non-nil: nil means "search results"
		for _, f := range m.visibleFolders() {
			ids = append(ids, f.ID)
		}
		return m.show(listView{title: "All inboxes", folderIDs: ids})
	}
	for j, f := range m.visibleFolders() {
		if f.Type == "inbox" {
			m.fcur = j
			break
		}
	}
	return m.openFolder()
}

func (m Model) openFolder() (tea.Model, tea.Cmd) {
	vis := m.visibleFolders()
	if m.fcur >= len(vis) {
		return m, nil
	}
	f := vis[m.fcur]
	return m.show(listView{title: m.accounts[f.AccountID].Name + " › " + f.Name, folderIDs: []string{f.ID}, accountID: f.AccountID})
}

func (m Model) show(v listView) (tea.Model, tea.Cmd) {
	m.view, m.msgs, m.mcur = v, nil, 0
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
		if m.mode == mMovePick {
			m.mode = mNormal
			return m, nil
		}
		return m, tea.Quit
	case "?":
		m.mode = mHelp
		return m, nil
	case "tab", "L":
		m.focus = (m.focus + 1) % 3
		return m, nil
	case "shift+tab", "H":
		m.focus = (m.focus + 2) % 3
		return m, nil
	case "1", "2", "3":
		m.focus = pane(s[0] - '1')
		return m, nil
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "alt+7", "alt+8", "alt+9":
		if m.mode == mMovePick {
			return m, nil
		}
		return m.openTab(int(s[4] - '1'))
	case "[", "]":
		if m.mode == mMovePick {
			return m, nil
		}
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
		if m.mode == mMovePick {
			m = m.endMove()
			m.status = "move cancelled"
			return m, nil
		}
		if m.agentBusy && m.agentStop != nil {
			m.agentStop()
			return m, nil
		}
		if m.view.folderIDs == nil && m.prevView.title != "" { // leave search results
			return m.show(m.prevView)
		}
	}

	switch m.focus {
	case pFolders:
		return m.keyFolders(s)
	case pMessages:
		return m.keyMessages(s)
	default:
		vp := &m.preview
		if m.showAgent {
			vp = &m.agentVP
		}
		var cmd tea.Cmd
		*vp, cmd = vp.Update(k)
		if s == "h" || s == "left" {
			m.focus = pMessages
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

func (m Model) keyFolders(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "enter", "l", "right":
		vis := m.visibleFolders()
		if m.fcur >= len(vis) {
			return m, nil
		}
		if m.mode == mMovePick {
			to := vis[m.fcur]
			ids := m.moveIDs
			m = m.endMove()
			m.focus = pMessages
			c, ctx := m.c, m.ctx
			return m, m.act("moved to "+to.Name, ids, func() error { return c.Move(ctx, ids, to.ID) })
		}
		m.focus = pMessages
		return m.openFolder()
	}
	m.fcur = move(m.fcur, len(m.visibleFolders()), s, m.listHeight()/2)
	return m, nil
}

func (m Model) keyMessages(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "enter", "l", "right":
		m.focus = pDetail
		m.showAgent = false
		if x, ok := m.selected(); ok && !x.Read {
			m.msgs[m.mcur].Read = true
			c, ctx := m.c, m.ctx
			return m, m.act("", nil, func() error { return c.Mark(ctx, []int{x.ID}, "read") })
		}
		return m, nil
	case "h", "left":
		m.focus = pFolders
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
		if m.mode == mMovePick {
			return m, nil
		}
		m.mode, m.moveIDs, m.focus = mMovePick, ids, pFolders
		m.moveAcct, m.moveFcur, m.fcur = x.Folder.AccountID, m.fcur, 0
		m.status = "pick a folder and press enter (esc cancels)"
		return m, nil
	case "r", "R", "f":
		kind := map[string]tb.DraftKind{"r": tb.Reply, "R": tb.ReplyAll, "f": tb.Forward}[s]
		m = m.openDraft(tb.Draft{Kind: kind, ReplyTo: x.ID})
		return m, m.edit()
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
	case "s":
		return m.askAgent(fmt.Sprintf("Summarize message id %d. If it is part of a thread, read the thread first and summarize the whole conversation. Then list any action items for me.", x.ID))
	case "w":
		return m.startInput("agent", "how should borealis2 reply?", fmt.Sprintf("Draft a reply to message id %d: ", x.ID))
	}
	return m, passthrough
}

// endMove leaves the move picker and restores the folder cursor.
func (m Model) endMove() Model {
	m.mode, m.moveIDs, m.fcur = mNormal, nil, m.moveFcur
	return m
}

func (m Model) startInput(purpose, placeholder, value string) (tea.Model, tea.Cmd) {
	if m.mode == mMovePick {
		m = m.endMove()
	}
	if purpose == "agent" && m.agentBusy {
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
			return m.show(listView{title: fmt.Sprintf("Search: %s (esc to go back)", v), query: v})
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
	if m.draft != nil {
		return mDraft // confirmations raised from the draft screen return to it
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
	parts := append(strings.Fields(editor), path)
	return tea.ExecProcess(exec.Command(parts[0], parts[1:]...), func(err error) tea.Msg {
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
