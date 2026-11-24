package ui

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/davidhyldgaard/lazythunder/internal/tb"
)

// replyInfo says the user replied to ('r') or forwarded ('f') a message, with
// the sent message that did it.
type replyInfo struct {
	kind   byte
	sentID int
	date   time.Time
	to     string
}

type (
	replyStatusMsg struct {
		gen    int
		status map[int]replyInfo
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

var rePrefix = regexp.MustCompile(`(?i)^\s*((re|sv|aw|antw|fw|fwd|vs|wg|tr|rv)(\[\d+\])?\s*:\s*)+`)

// splitSubject strips reply/forward prefixes. kind is 'r', 'f' or 0 by the first prefix.
func splitSubject(s string) (kind byte, norm string) {
	p := rePrefix.FindString(s)
	norm = strings.ToLower(strings.TrimSpace(s[len(p):]))
	switch first := strings.ToLower(strings.TrimSpace(p)); {
	case first == "":
	case strings.HasPrefix(first, "re"), strings.HasPrefix(first, "sv"), strings.HasPrefix(first, "aw"), strings.HasPrefix(first, "antw"):
		kind = 'r'
	default:
		kind = 'f'
	}
	return kind, norm
}

// addrOf extracts the lower-cased email address from "Name <a@b>".
func addrOf(a string) string {
	if i, j := strings.LastIndex(a, "<"), strings.LastIndex(a, ">"); i >= 0 && j > i {
		a = a[i+1 : j]
	}
	return strings.ToLower(strings.TrimSpace(a))
}

// matchReplies marks msgs answered by a sent message: a later message with the
// same subject (Re: or no prefix: some clients drop it) sent to the message's
// author is a reply (wins); a later Fwd: of it is a forward. shortcut: subject matching, so generic subjects can misfire;
// exact matching would need each message's References via `tb thread`.
func matchReplies(msgs, sent []tb.Message) map[int]replyInfo {
	type cand struct {
		kind byte
		m    tb.Message
	}
	bySubj := map[string][]cand{}
	for _, s := range sent {
		k, n := splitSubject(s.Subject)
		if k == 0 {
			k = 'r'
		}
		if n != "" {
			bySubj[n] = append(bySubj[n], cand{k, s})
		}
	}
	out := map[int]replyInfo{}
	for _, m := range msgs {
		_, n := splitSubject(m.Subject)
		from := addrOf(m.Author)
		for _, c := range bySubj[n] {
			if !c.m.Date.After(m.Date) || c.m.ID == m.ID {
				continue
			}
			to := ""
			if len(c.m.Recipients) > 0 {
				to = c.m.Recipients[0]
			}
			if c.kind == 'r' {
				hit := false
				for _, r := range append(c.m.Recipients, c.m.CcList...) {
					hit = hit || addrOf(r) == from
				}
				if !hit {
					continue
				}
			}
			cur, seen := out[m.ID]
			// the first answer after the message is the one that replied to it
			if !seen || c.kind == 'r' && cur.kind == 'f' || c.kind == cur.kind && c.m.Date.Before(cur.date) {
				out[m.ID] = replyInfo{kind: c.kind, sentID: c.m.ID, date: c.m.Date, to: to}
			}
		}
	}
	return out
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
		// tb's subject filter is case-sensitive: search the original spelling
		if raw := strings.TrimSpace(x.Subject[len(rePrefix.FindString(x.Subject)):]); len(raw) >= 3 { // tiny subjects would match everything
			subjects[raw] = true
		}
	}
	c, ctx, gen := m.c, m.viewCtx, m.viewGen
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
		return replyStatusMsg{gen: gen, status: matchReplies(msgs, sent)}
	}
}

// maybeLoadReply fetches the user's reply to the open message, once.
func (m *Model) maybeLoadReply() tea.Cmd {
	if m.full == nil {
		return nil
	}
	id := m.full.ID
	info, ok := m.replied[id]
	if !ok || info.kind != 'r' || m.replyLoading == id || m.reply != nil && m.reply.forID == id {
		return nil
	}
	m.replyLoading = id
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		msg, err := c.Read(ctx, info.sentID)
		return sentReplyMsg{forID: id, msg: msg, err: err}
	}
}
