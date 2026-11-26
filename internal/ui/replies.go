package ui

import (
	"context"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/bg"
	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
)

type (
	replyStatusMsg struct {
		gen    int
		status map[int]store.Reply
	}
	sentReplyMsg struct {
		forID int
		msg   tb.FullMessage
		err   error
	}
)

// loadedReply is the user's own reply shown above the message it answers.
type loadedReply struct {
	forID int
	msg   tb.FullMessage
}

// loadReplyStatus looks in the Sent folders of msgs' accounts for each
// distinct subject in the list (one small search per subject, so the cost
// follows the list size, not how much mail was sent) and matches them.
func (m Model) loadReplyStatus(msgs []tb.Message) tea.Cmd {
	if len(msgs) == 0 {
		return nil
	}
	since := msgs[0].Date
	accts := map[string]bool{}
	for _, x := range msgs {
		if x.Date.Before(since) {
			since = x.Date
		}
		accts[x.Folder.AccountID] = true
	}
	var sentIDs []string
	for _, f := range m.folders {
		if f.Type == "sent" && accts[f.AccountID] {
			sentIDs = append(sentIDs, f.ID)
		}
	}
	if len(sentIDs) == 0 {
		return nil
	}
	subjects := map[string]bool{}
	for _, x := range msgs {
		if raw := bg.SearchSubject(x.Subject); raw != "" {
			subjects[raw] = true
		}
	}
	c, ctx, gen, st := m.c, m.viewCtx, m.viewGen, m.st
	if ctx == nil {
		ctx = m.ctx
	}
	type job struct{ folder, subject string }
	var jobs []job
	for _, id := range sentIDs {
		for subj := range subjects {
			jobs = append(jobs, job{id, subj})
		}
	}
	return func() tea.Msg {
		var mu sync.Mutex
		var sent []tb.Message
		var wg sync.WaitGroup
		queue := make(chan job)
		for w := 0; w < 6; w++ { // fixed pool: each job is a tb subprocess
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range queue {
					p, err := c.Search(ctx, "", tb.SearchOpts{Folder: j.folder, Subject: j.subject, Since: since.UTC().Format(time.RFC3339), Limit: 200, IncludeJunk: true})
					if err == nil { // shortcut: a failed lookup just leaves those messages unmarked
						mu.Lock()
						sent = append(sent, p.Messages...)
						mu.Unlock()
					}
				}
			}()
		}
	feed:
		for _, j := range jobs {
			select {
			case queue <- j:
			case <-ctx.Done(): // the view changed; stop starting tb processes
				break feed
			}
		}
		close(queue)
		wg.Wait()
		status := bg.MatchReplies(msgs, sent)
		if st != nil {
			for _, x := range msgs {
				if r, ok := status[x.ID]; ok && x.HeaderMessageID != "" {
					st.PutReply(x.HeaderMessageID, r) // shortcut: a failed write only costs the next start a lookup
				}
			}
		}
		return replyStatusMsg{gen: gen, status: status}
	}
}

// maybeLoadReply fetches the user's reply to the open message, once.
func (m *Model) maybeLoadReply() tea.Cmd {
	if m.full == nil {
		return nil
	}
	id := m.full.ID
	info, ok := m.replied[id]
	if !ok || info.Kind != 'r' || m.replyLoading == id || m.reply != nil && m.reply.forID == id {
		return nil
	}
	m.replyLoading = id
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		msg, err := c.Read(ctx, info.SentID)
		return sentReplyMsg{forID: id, msg: msg, err: err}
	}
}

// applyStoredReplies marks listed messages with the reply status on record
// (found by the poller), without overriding live lookups, and returns the
// ids it marked.
func (m *Model) applyStoredReplies() map[int]bool {
	if m.st == nil {
		return nil
	}
	ids := map[string][]int{} // one Message-ID can be listed more than once (merged inboxes)
	var hdrs []string
	for _, x := range m.msgs {
		if x.HeaderMessageID != "" {
			ids[x.HeaderMessageID] = append(ids[x.HeaderMessageID], x.ID)
			hdrs = append(hdrs, x.HeaderMessageID)
		}
	}
	rs, err := m.st.Replies(hdrs)
	if err != nil {
		m.err = "reply status: " + err.Error()
		return nil
	}
	added := map[int]bool{}
	for h, r := range rs {
		for _, id := range ids[h] {
			if _, ok := m.replied[id]; !ok {
				m.replied[id], added[id] = r, true
			}
		}
	}
	return added
}
