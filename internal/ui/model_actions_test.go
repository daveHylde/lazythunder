package ui

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/tb"
)

func wantCalls(t *testing.T, h *testHub, want ...string) {
	t.Helper()
	if got := h.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("hub calls\n got %q\nwant %q", got, want)
	}
}

func TestMessageActionsCallHub(t *testing.T) {
	for _, c := range []struct {
		name   string
		keys   []string // before the action key (selection, movement)
		action string
		want   string
		status string
		remove []int
	}{
		{"read", nil, "m", `mark {"flag":"read","ids":[1]}`, "marked 1 message read", nil},
		{"unread", []string{"j"}, "m", `mark {"flag":"unread","ids":[2]}`, "marked 1 message unread", nil},
		{"flag", nil, "S", `mark {"flag":"flagged","ids":[1]}`, "marked 1 message flagged", nil},
		{"read selection, mixed", []string{" ", " "}, "m", `mark {"flag":"read","ids":[1,2]}`, "marked 2 messages read", nil},
		{"unread selection, all read", []string{"j", " ", "j", "j", " "}, "m", `mark {"flag":"unread","ids":[2,4]}`, "marked 2 messages unread", nil},
		{"archive", []string{"j"}, "a", `archive {"ids":[2]}`, "archived 1 message", []int{2}},
		{"archive selection", []string{" ", "j", " "}, "a", `archive {"ids":[1,3]}`, "archived 2 messages", []int{1, 3}},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, h := newTestHub(t)
			m := press(t, loaded(t, client), c.keys...)
			m, cmd := pressCmd(t, m, c.action)
			done, ok := run(t, cmd).(doneMsg)
			if !ok || done.err != nil {
				t.Fatalf("got %+v", done)
			}
			wantCalls(t, h, c.want)
			if !reflect.DeepEqual(done.remove, c.remove) {
				t.Fatalf("remove %v, want %v", done.remove, c.remove)
			}
			m, _ = update(m, done)
			if m.status != c.status {
				t.Fatalf("status %q, want %q", m.status, c.status)
			}
			for _, id := range ids(m.msgs) {
				for _, r := range c.remove {
					if id == r {
						t.Fatalf("%d still listed: %v", id, ids(m.msgs))
					}
				}
			}
			if len(c.remove) > 0 && len(m.marked) != 0 {
				t.Fatalf("removed messages still selected: %v", m.marked)
			}
		})
	}
}

func TestMarkUpdatesListAtOnce(t *testing.T) {
	m := loaded(t, nil)
	m = press(t, m, "m", "S")
	if !m.msgs[0].Read || !m.msgs[0].Flagged {
		t.Fatalf("%+v", m.msgs[0])
	}
	m = press(t, m, "S")
	if m.msgs[0].Flagged {
		t.Fatal("S does not unflag")
	}
}

func TestDeleteNeedsConfirmation(t *testing.T) {
	for _, k := range []string{"d", "delete"} {
		t.Run(k, func(t *testing.T) {
			client, h := newTestHub(t)
			m := press(t, loaded(t, client), " ", "j", " ") // 1 and 3 selected
			m, cmd := pressCmd(t, m, k)
			if cmd != nil || m.mode != mConfirm || !strings.Contains(statusLine(m), "Move 2 messages to trash? [y/n]") {
				t.Fatalf("mode %d line %q", m.mode, statusLine(m))
			}
			m = press(t, m, "k") // other keys are ignored while asking
			if m.mode != mConfirm || m.mcur != 3 {
				t.Fatalf("k while confirming: mode %d cur %d", m.mode, m.mcur)
			}
			m, cmd = pressCmd(t, m, "n")
			if cmd != nil || m.mode != mNormal || len(h.Calls()) != 0 {
				t.Fatalf("n: cmd %v mode %d calls %v", cmd != nil, m.mode, h.Calls())
			}
			m, _ = pressCmd(t, m, k)
			m, cmd = pressCmd(t, m, "y")
			done := run(t, cmd).(doneMsg)
			wantCalls(t, h, `delete {"ids":[1,3]}`)
			m, _ = update(m, done)
			if !reflect.DeepEqual(ids(m.msgs), []int{2, 4}) || m.status != "moved 2 messages to trash" || m.mcur > 1 {
				t.Fatalf("msgs %v cur %d status %q", ids(m.msgs), m.mcur, m.status)
			}
		})
	}
}

func TestDeleteLastMessageClampsCursor(t *testing.T) {
	m := press(t, loaded(t, nil), "G")
	m, _ = update(m, doneMsg{remove: []int{4}})
	if m.mcur != 2 || len(m.msgs) != 3 {
		t.Fatalf("cur %d msgs %v", m.mcur, ids(m.msgs))
	}
	m, _ = update(m, doneMsg{remove: []int{1, 2, 3}})
	if m.mcur != 0 || len(m.msgs) != 0 || !strings.Contains(screen(m), "(empty)") {
		t.Fatalf("cur %d msgs %v", m.mcur, ids(m.msgs))
	}
	// actions on an empty list do nothing
	if _, cmd := pressCmd(t, m, "a"); cmd != nil {
		t.Fatal("archive on an empty list")
	}
}

func TestActionFailureShownAndListKept(t *testing.T) {
	client, h := newTestHub(t)
	h.failOp("archive", "folder is read-only")
	m := loaded(t, client)
	m, cmd := pressCmd(t, m, "a")
	done := run(t, cmd).(doneMsg)
	m, _ = update(m, done)
	if len(m.msgs) != 4 {
		t.Fatalf("failed archive removed messages: %v", ids(m.msgs))
	}
	if got := statusLine(m); !strings.Contains(got, "folder is read-only (TEST)") {
		t.Fatalf("status line %q", got)
	}
	// the next success clears the error
	m, _ = update(m, doneMsg{text: "ok"})
	if m.err != "" || m.status != "ok" {
		t.Fatalf("err %q status %q", m.err, m.status)
	}
}

func TestMarkAllReadFolder(t *testing.T) {
	client, h := newTestHub(t)
	m := loaded(t, client)
	m, cmd := pressCmd(t, m, "C")
	if cmd != nil || m.mode != mConfirm || m.confirm.text != "Mark everything in All inboxes as read?" {
		t.Fatalf("mode %d confirm %+v", m.mode, m.confirm)
	}
	m, cmd = pressCmd(t, m, "y")
	done := run(t, cmd).(doneMsg)
	wantCalls(t, h, `markFolderRead {"folder":"a1://INBOX"}`, `markFolderRead {"folder":"a2://INBOX"}`)
	seq := m.listSeq
	m, _ = update(m, done)
	if m.status != "marked all read" || m.listSeq != seq+1 {
		t.Fatalf("status %q, list not reloaded", m.status)
	}
}

func TestMarkAllReadSearchMarksListedOnly(t *testing.T) {
	client, h := newTestHub(t)
	m := search(t, loaded(t, client), "q")
	m, _ = update(m, listMsg{seq: m.listSeq, msgs: testInbox()})
	m, cmd := pressCmd(t, m, "C")
	run(t, cmd)
	wantCalls(t, h, `mark {"flag":"read","ids":[1,3]}`)
	for _, x := range m.msgs {
		if !x.Read {
			t.Fatalf("%d still unread", x.ID)
		}
	}
	if m, cmd = pressCmd(t, m, "C"); cmd != nil || m.status != "nothing unread" {
		t.Fatalf("second C: %q", m.status)
	}
}

func TestMoveRefusesCrossAccountSelection(t *testing.T) {
	m := press(t, loaded(t, nil), " ", " ") // 1 (Work) and 2 (Home)
	m = press(t, m, "M")
	if m.status != "selection spans accounts: move them per account" || m.mode != mNormal {
		t.Fatalf("status %q mode %d", m.status, m.mode)
	}
}

func TestKeysNeedingOpenMessage(t *testing.T) {
	for _, k := range []string{"o", "A", "i"} {
		m := press(t, loaded(t, nil), k)
		if m.status != "open the message first" {
			t.Errorf("%s: status %q", k, m.status)
		}
	}
}

func TestAIOffKeys(t *testing.T) {
	for _, k := range []string{":", "z"} {
		m := press(t, loaded(t, nil), k) // nil agent: AI off
		if m.status != "AI is off (LAZYTHUNDER_LLM_URL=off)" || m.mode != mNormal || m.asst.busy {
			t.Errorf("%s: status %q mode %d", k, m.status, m.mode)
		}
	}
}

func TestEnterOpensAndMarksRead(t *testing.T) {
	client, h := newTestHub(t)
	m := loaded(t, client)
	m, cmd := pressCmd(t, m, "enter")
	if m.focus != pDetail || !m.msgs[0].Read {
		t.Fatalf("focus %d read %v", m.focus, m.msgs[0].Read)
	}
	run(t, cmd)
	wantCalls(t, h, `mark {"flag":"read","ids":[1]}`)
	// an already read message is not marked again
	m = press(t, m, "h", "j")
	if _, cmd = pressCmd(t, m, "enter"); cmd != nil {
		t.Fatal("enter on a read message calls the hub")
	}
}

func TestPreviewLoads(t *testing.T) {
	client, h := newTestHub(t)
	h.set("read", map[string]any{"id": 1, "subject": "Hello", "author": "Alice <alice@x.no>",
		"recipients": []string{"me@work.no"}, "parts": map[string]any{"text": "Body text here"}})
	m := loaded(t, client)
	if !strings.Contains(screen(m), "[2] loading Hello…") {
		t.Fatal("no loading title before the preview arrives")
	}
	// a tick from an older cursor position does nothing
	if _, cmd := update(m, previewTick{seq: m.previewSeq - 1, id: 1}); cmd != nil {
		t.Fatal("stale preview tick loads")
	}
	_, cmd := update(m, previewTick{seq: m.previewSeq, id: 1})
	pm := run(t, cmd).(previewMsg)
	wantCalls(t, h, `read {"id":1}`)
	if stale, _ := update(m, previewMsg{seq: m.previewSeq - 1, m: pm.m}); stale.full != nil {
		t.Fatal("stale preview shown")
	}
	m, _ = update(m, pm)
	if m.full == nil || m.full.ID != 1 {
		t.Fatalf("full %+v", m.full)
	}
	view := screen(m)
	for _, want := range []string{"[2] Hello", "Body text here", "From: Alice <alice@x.no>", "To: me@work.no"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q", want)
		}
	}
	// moving on clears and re-schedules
	seq := m.previewSeq
	if m = press(t, m, "j"); m.previewSeq != seq+1 || !strings.Contains(screen(m), "[2] loading Invoice…") {
		t.Fatalf("preview not rescheduled: seq %d→%d", seq, m.previewSeq)
	}
}

func TestPreviewFailure(t *testing.T) {
	m := loaded(t, nil)
	m, _ = update(m, previewMsg{seq: m.previewSeq, err: errors.New("message gone")})
	view := screen(m)
	if m.full != nil || !strings.Contains(view, "Could not load this message: message gone") || strings.Contains(view, "loading Hello") {
		t.Fatalf("view:\n%s", view)
	}
}

func TestRestingMarksRead(t *testing.T) {
	client, h := newTestHub(t)
	m := loaded(t, client)
	if _, cmd := update(m, readTick{seq: m.previewSeq, id: 99}); cmd != nil {
		t.Fatal("tick for another message marks read")
	}
	m, cmd := update(m, readTick{seq: m.previewSeq, id: 1})
	if !m.msgs[0].Read {
		t.Fatal("not marked read locally")
	}
	run(t, cmd)
	wantCalls(t, h, `mark {"flag":"read","ids":[1]}`)
	m = press(t, m, "j") // message 2 is read already
	if _, cmd = update(m, readTick{seq: m.previewSeq, id: 2}); cmd != nil {
		t.Fatal("read message marked again")
	}
}

func TestNewMailStatus(t *testing.T) {
	for _, c := range []struct {
		msg     newMailMsg
		status  string
		wantCmd bool
	}{
		{newMailMsg{n: 2, event: true}, "2 new message(s) · F5 to refresh", true},
		{newMailMsg{n: 0, event: true}, "4 messages", false},
		{newMailMsg{n: 3}, "3 new message(s) · F5 to refresh", true},
		{newMailMsg{n: 0}, "4 messages", true}, // the minute poll goes on
	} {
		m, cmd := update(loaded(t, nil), c.msg)
		if m.status != c.status || (cmd != nil) != c.wantCmd {
			t.Errorf("%+v: status %q cmd %v", c.msg, m.status, cmd != nil)
		}
	}
}

func TestMailEvents(t *testing.T) {
	client, h := newTestHub(t)
	h.set("folders", testFolders(9, 1))
	h.set("accounts", testAccounts())
	m := loaded(t, client)
	ch := make(chan tb.Event, 4)

	// subscribing starts the wait; no channel (events unsupported) waits for nothing
	if _, cmd := update(m, eventsMsg{nil}); cmd != nil {
		t.Fatal("waiting on a nil channel")
	}
	ch <- tb.Event{Name: "newMail"}
	_, cmd := update(m, eventsMsg{ch})
	ev := run(t, cmd).(mailEventMsg)
	if ev.name != "newMail" {
		t.Fatalf("got %+v", ev)
	}

	// newMail re-arms the wait (no store: no poll pass)
	ch <- tb.Event{Name: "something"}
	_, cmd = update(m, ev)
	ev = run(t, cmd).(mailEventMsg)
	if ev.name != "something" {
		t.Fatalf("got %+v", ev)
	}
	// unknown events just re-arm
	ch <- tb.Event{Name: "changed"}
	_, cmd = update(m, ev)
	ev = run(t, cmd).(mailEventMsg)

	// changed: re-arm and reload the folders (unread counts)
	ch <- tb.Event{Name: "newMail"}
	m, cmd = update(m, ev)
	var folders foldersMsg
	var next mailEventMsg
	for _, sub := range subCmds(t, run(t, cmd)) {
		switch msg := run(t, sub).(type) {
		case foldersMsg:
			folders = msg
		case mailEventMsg:
			next = msg
		default:
			t.Fatalf("unexpected %T", msg)
		}
	}
	if next.name != "newMail" || folders.err != nil || len(folders.folders) != 4 {
		t.Fatalf("next %+v folders %+v", next, folders)
	}
	m, _ = update(m, folders)
	if tabs := strings.Split(screen(m), "\n")[0]; !strings.Contains(tabs, "2 Work (9)") {
		t.Fatalf("tab bar %q", tabs)
	}

	// a closed channel ends the loop
	close(ch)
	for range ch {
	}
	if msg := run(t, waitEvent(ch)); msg != nil {
		t.Fatalf("closed channel gave %+v", msg)
	}
}

func TestStatusMsg(t *testing.T) {
	m, _ := update(loaded(t, nil), statusMsg("waiting for the Thunderbird add-on"))
	if !strings.Contains(statusLine(m), "waiting for the Thunderbird add-on") {
		t.Fatalf("status line %q", statusLine(m))
	}
}

// editDraft simulates the editor: it writes text to a file and feeds editedMsg back.
func editDraft(t *testing.T, m Model, text string) Model {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "draft-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(text)
	f.Close()
	m, _ = update(m, editedMsg{path: f.Name()})
	return m
}

func TestReplyDraftSave(t *testing.T) {
	client, h := newTestHub(t)
	m := loaded(t, client)
	m, cmd := pressCmd(t, m, "r")
	if cmd == nil || m.mode != mDraft || m.draft.Kind != tb.Reply || m.draft.ReplyTo != 1 {
		t.Fatalf("mode %d draft %+v", m.mode, m.draft)
	}
	if !strings.Contains(m.draftCtx, "Reply to: Alice <alice@x.no>") || !strings.Contains(m.draftCtx, "Subject: Hello") {
		t.Fatalf("context %q", m.draftCtx)
	}
	m = editDraft(t, m, renderDraft(*m.draft, m.draftCtx)+"Takk!\n")
	if m.err != "" || m.draft.Body != "Takk!\n" || m.draftFile != "" || m.mode != mDraft {
		t.Fatalf("err %q draft %+v file %q", m.err, m.draft, m.draftFile)
	}
	view := screen(m)
	for _, want := range []string{"[2] Reply · v1", "edited in your editor (v1)", "AI is off", "v1 · u undo · e editor · ctrl+s send · ctrl+d save draft · esc close", "Takk!"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q", want)
		}
	}
	m, cmd = pressCmd(t, m, "ctrl+d")
	if !m.submitting || m.status != "saving draft…" {
		t.Fatalf("submitting %v status %q", m.submitting, m.status)
	}
	if m2 := press(t, m, "esc"); m2.draft == nil || m2.status != "still saving…" {
		t.Fatal("draft discarded while saving")
	}
	sub := run(t, cmd).(submittedMsg)
	wantCalls(t, h, `submit {"body":"Takk!\n","kind":"reply","replyTo":1,"send":false}`)
	m, _ = update(m, sub)
	if m.draft != nil || m.mode != mNormal || m.status != "saved to Drafts" {
		t.Fatalf("draft %+v mode %d status %q", m.draft, m.mode, m.status)
	}
}

func TestForwardDraftSend(t *testing.T) {
	client, h := newTestHub(t)
	m := loaded(t, client)
	m, _ = pressCmd(t, m, "f")
	text := strings.Replace(renderDraft(*m.draft, m.draftCtx), "To: \n", "To: bob@y.no\n", 1) + "fyi\n"
	m = editDraft(t, m, text)
	if m.err != "" || m.draft.To != "bob@y.no" {
		t.Fatalf("err %q draft %+v", m.err, m.draft)
	}
	m, cmd := pressCmd(t, m, "ctrl+s")
	if cmd != nil || m.mode != mConfirm || m.confirm.text != "Send this message now?" {
		t.Fatalf("mode %d", m.mode)
	}
	if m2 := press(t, m, "n"); m2.mode != mDraft || m2.draft == nil {
		t.Fatal("n on send did not return to the draft")
	}
	m, cmd = pressCmd(t, m, "y")
	steps := subCmds(t, run(t, cmd)) // sending…, then submit
	if len(steps) != 2 {
		t.Fatalf("%d steps", len(steps))
	}
	m, _ = update(m, run(t, steps[0]))
	if m.status != "sending…" || !m.submitting {
		t.Fatalf("status %q", m.status)
	}
	m, _ = update(m, run(t, steps[1]))
	wantCalls(t, h, `submit {"body":"fyi\n","kind":"forward","replyTo":1,"send":true,"to":"bob@y.no"}`)
	if m.status != "sent" || m.draft != nil {
		t.Fatalf("status %q draft %+v", m.status, m.draft)
	}
}

func TestSubmitFailureKeepsDraft(t *testing.T) {
	client, h := newTestHub(t)
	h.failOp("submit", "SMTP down")
	m := loaded(t, client)
	m = openTestDraft(m, tb.Draft{Kind: tb.Reply, ReplyTo: 1, Body: "hi\n"})
	m, cmd := pressCmd(t, m, "ctrl+d")
	m, _ = update(m, run(t, cmd))
	if m.draft == nil || m.mode != mDraft || m.submitting || !strings.Contains(statusLine(m), "SMTP down (TEST)") {
		t.Fatalf("draft %+v mode %d line %q", m.draft, m.mode, statusLine(m))
	}
}

// discard closes compose mode: esc, then n on "Save to Drafts?".
func discard(t *testing.T, m Model) Model {
	t.Helper()
	m = press(t, m, "esc")
	if m.mode != mConfirm || m.confirm.onNo == nil {
		t.Fatalf("esc did not ask to save (mode %d)", m.mode)
	}
	m, cmd := pressCmd(t, m, "n")
	m, _ = update(m, run(t, cmd))
	return m
}

func openTestDraft(m Model, d tb.Draft) Model {
	m, _ = update(m, draftMsg{d})
	return m
}

func TestComposeUsesSelectedAccountIdentity(t *testing.T) {
	m := press(t, loaded(t, nil), "j", "c") // message 2 is in Home
	if m.mode != mDraft || m.draft.Kind != tb.NewMail || m.draft.From != "me@home.no" {
		t.Fatalf("draft %+v", m.draft)
	}
	m = editDraft(t, m, "--- write below this line ---\nno To\n")
	if m.err != "draft: To: is empty (ctrl+e to edit)" || m.draftFile == "" {
		t.Fatalf("err %q file %q", m.err, m.draftFile)
	}
	file := m.draftFile
	m = press(t, m, "ctrl+d") // the broken file must be fixed first
	if m.err != "fix the draft first (ctrl+e to edit)" || m.submitting {
		t.Fatalf("err %q", m.err)
	}
	m = discard(t, m)
	if m.draft != nil || m.mode != mNormal || m.status != "draft discarded" {
		t.Fatalf("draft %+v status %q", m.draft, m.status)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("discarded draft file left behind")
	}
}

func TestDraftEditorErrors(t *testing.T) {
	m := openTestDraft(loaded(t, nil), tb.Draft{Kind: tb.Reply, ReplyTo: 1})
	m, _ = update(m, editedMsg{path: "/nonexistent", err: errors.New("exit status 1")})
	if m.err != "editor: exit status 1 (ctrl+e to edit)" || m.mode != mDraft {
		t.Fatalf("err %q", m.err)
	}
	m = openTestDraft(loaded(t, nil), tb.Draft{Kind: tb.Reply, ReplyTo: 1}) // empty body
	m = press(t, m, "ctrl+d")
	if m.err != "draft: body is empty (ctrl+e to edit)" || m.submitting {
		t.Fatalf("err %q", m.err)
	}
	m = editDraft(t, m, "no separator\n")
	if !strings.Contains(m.err, "separator line") {
		t.Fatalf("err %q", m.err)
	}
	// edit finishing after the draft was discarded removes the file, changes nothing
	m = discard(t, m)
	f, _ := os.CreateTemp(t.TempDir(), "late")
	f.Close()
	m, _ = update(m, editedMsg{path: f.Name()})
	if _, err := os.Stat(f.Name()); !os.IsNotExist(err) || m.draft != nil || m.mode != mNormal {
		t.Fatal("late edit result not dropped")
	}
}

func TestAgentDraftQueuedBehindOpenDraft(t *testing.T) {
	m := openTestDraft(loaded(t, nil), tb.Draft{Kind: tb.Reply, ReplyTo: 1, Body: "mine\n"})
	m = openTestDraft(m, tb.Draft{Kind: tb.Reply, ReplyTo: 3, Body: "agent's\n"})
	if m.draft.Body != "mine\n" || m.pending == nil || !strings.Contains(m.status, "has a draft ready") {
		t.Fatalf("draft %+v pending %+v status %q", m.draft, m.pending, m.status)
	}
	m = discard(t, m)
	if m.draft == nil || m.draft.Body != "agent's\n" || m.mode != mDraft || m.pending != nil {
		t.Fatalf("queued draft not opened: %+v", m.draft)
	}
	if !strings.Contains(m.draftCtx, "Subject: Meeting") {
		t.Fatalf("context %q", m.draftCtx)
	}
}

func TestReplyKindsFromKeys(t *testing.T) {
	for k, want := range map[string]tb.DraftKind{"r": tb.Reply, "R": tb.ReplyAll, "f": tb.Forward} {
		m := press(t, loaded(t, nil), "j", "j", k)
		if m.draft == nil || m.draft.Kind != want || m.draft.ReplyTo != 3 {
			t.Errorf("%s: %+v", k, m.draft)
		}
	}
}

func TestQuit(t *testing.T) {
	_, cmd := pressCmd(t, loaded(t, nil), "q")
	if !reflect.DeepEqual(run(t, cmd), tea.QuitMsg{}) {
		t.Fatal("q does not quit")
	}
}
