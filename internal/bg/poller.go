package bg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// Poller finds mail that arrived since the last pass. Both the TUI and the
// daemon may run it: both may handle the same message, which is harmless
// (Enqueue and PutReply are idempotent), and MarkSeen counts it for one.
type Poller struct {
	Store *store.Store
	Mail  Mail
	AI    bool // queue fresh inbox mail for notes
}

// window is how far back each pass looks: Date headers can lag arrival.
const window = 48 * time.Hour

// sentKey keeps the Sent baseline apart from the inbox one: mail to yourself
// has the same Message-ID in both.
func sentKey(s string) string { return "sent:" + s }

// Pass polls every inbox and Sent folder in folders once and returns how many
// inbox messages this pass newly marked seen. A folder's first pass only
// records what is there (its baseline): older mail is not backfilled.
func (p Poller) Pass(ctx context.Context, folders []tb.Folder) (int, error) {
	var errs []error
	n := 0
	for _, f := range folders {
		switch f.Type {
		case "inbox":
			k, err := p.handle(ctx, f.ID, func(s string) string { return s }, func(x tb.Message) error {
				if !p.AI {
					return nil
				}
				return p.Store.Enqueue(x.HeaderMessageID, x.ID, 0)
			})
			n += k
			errs = append(errs, err)
		case "sent":
			inboxes := inboxesOf(folders, f.AccountID)
			_, err := p.handle(ctx, f.ID, sentKey, func(s tb.Message) error { return p.matchSent(ctx, s, inboxes) })
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// handle runs do on folder's unseen messages and marks seen only those it
// succeeded on, so a failed one is handled again next pass. It returns how
// many this call newly marked.
func (p Poller) handle(ctx context.Context, folder string, key func(string) string, do func(tb.Message) error) (int, error) {
	todo, err := p.unseen(ctx, folder, key)
	if err != nil || len(todo) == 0 {
		return 0, err
	}
	var done []string
	var errs []error
	for _, x := range todo {
		if err := do(x); err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, key(x.HeaderMessageID))
	}
	marked, err := p.Store.MarkSeen(done)
	return len(marked), errors.Join(append(errs, err)...)
}

// unseen returns folder's recent messages not seen before (by key(hdr)); none
// on the folder's first pass, which marks them all seen instead.
func (p Poller) unseen(ctx context.Context, folder string, key func(string) string) ([]tb.Message, error) {
	pg, err := p.Mail.Search(ctx, "", tb.SearchOpts{Folder: folder, Since: time.Now().Add(-window).UTC().Format(time.RFC3339), Limit: 500})
	if err != nil {
		return nil, fmt.Errorf("poll %s: %w", folder, err) // this folder gets its baseline on a later pass
	}
	byKey := map[string]tb.Message{}
	var keys []string
	for _, x := range pg.Messages {
		if k := key(x.HeaderMessageID); x.HeaderMessageID != "" {
			byKey[k] = x
			keys = append(keys, k)
		}
	}
	polled, err := p.Store.Polled(key(folder))
	if err != nil {
		return nil, err
	}
	if !polled {
		_, err := p.Store.MarkSeen(keys)
		if err != nil {
			return nil, err // no baseline yet: retried next pass
		}
		return nil, p.Store.SetPolled(key(folder))
	}
	fresh, err := p.Store.Unseen(keys)
	if err != nil {
		return nil, err
	}
	out := make([]tb.Message, 0, len(fresh))
	for _, k := range fresh {
		out = append(out, byKey[k])
	}
	return out, nil
}

func inboxesOf(folders []tb.Folder, acct string) []string {
	var out []string
	for _, f := range folders {
		if f.Type == "inbox" && f.AccountID == acct {
			out = append(out, f.ID)
		}
	}
	return out
}

// matchSent looks for the messages a freshly sent one answers, in its
// account's inboxes, and records the reply. An earlier reply already on
// record keeps its place.
func (p Poller) matchSent(ctx context.Context, s tb.Message, inboxes []string) error {
	subj := SearchSubject(s.Subject)
	if subj == "" {
		return nil
	}
	var cands []tb.Message
	for _, id := range inboxes {
		pg, err := p.Mail.Search(ctx, "", tb.SearchOpts{Folder: id, Subject: subj, Limit: 200, IncludeJunk: true})
		if err != nil {
			return fmt.Errorf("reply lookup in %s: %w", id, err)
		}
		cands = append(cands, pg.Messages...)
	}
	byID := map[int]string{}
	var hdrs []string
	for _, x := range cands {
		byID[x.ID] = x.HeaderMessageID
		hdrs = append(hdrs, x.HeaderMessageID)
	}
	have, err := p.Store.Replies(hdrs)
	if err != nil {
		return err
	}
	var errs []error
	for id, r := range MatchReplies(cands, []tb.Message{s}) {
		h := byID[id]
		if old, ok := have[h]; h == "" || ok && !better(r.Kind, r.Date, old) {
			continue
		}
		errs = append(errs, p.Store.PutReply(h, r))
	}
	return errors.Join(errs...)
}
