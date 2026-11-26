// Package tb talks to Thunderbird through the lazythunder add-on: requests go
// over the hub's unix socket (see addon/PROTOCOL.md), and Ensure starts
// Thunderbird headless when the add-on isn't reachable.
package tb

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daveHylde/lazythunder/internal/gloda"
	"github.com/daveHylde/lazythunder/internal/hub"
	"github.com/daveHylde/lazythunder/internal/profile"
)

// Client sends requests to the add-on over one lazily (re)dialed hub
// connection shared by all callers. The zero value uses hub.SocketPath().
type Client struct {
	Socket string // hub socket; empty means hub.SocketPath()
	Gloda  string // gloda database; empty means the default profile's

	folderOf sync.Map // gloda folder URI → add-on folder id, learned by FullText

	mu   sync.Mutex
	conn *conn
	next int64
}

func New() *Client { return &Client{} }

// Error is a failed response ({"ok":false,"error":...,"code":...}).
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Msg + " (" + e.Code + ")" }

func Code(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

type reply struct {
	ID    *int64          `json:"id"`
	Event string          `json:"event"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	Code  string          `json:"code"`
}

func disconnected(err error) error {
	return &Error{Code: "EXTENSION_DISCONNECTED", Msg: "lazythunder hub not reachable: " + err.Error()}
}

func (c *Client) socket() string {
	if c.Socket != "" {
		return c.Socket
	}
	return hub.SocketPath()
}

type conn struct {
	nc      net.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan reply
	dead    chan struct{} // closed when the read loop ends
}

func (k *conn) readLoop() {
	defer close(k.dead)
	defer k.nc.Close()
	r := bufio.NewReader(k.nc)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var rp reply
		if json.Unmarshal(line, &rp) != nil || rp.ID == nil {
			continue // events and junk: this connection only waits for replies
		}
		k.mu.Lock()
		ch := k.pending[*rp.ID]
		delete(k.pending, *rp.ID)
		k.mu.Unlock()
		if ch != nil {
			ch <- rp
		}
	}
}

// dial returns the live shared connection, dialing a new one if needed.
func (c *Client) dial(ctx context.Context) (*conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		select {
		case <-c.conn.dead:
		default:
			return c.conn, nil
		}
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", c.socket())
	if err != nil {
		return nil, disconnected(err)
	}
	c.conn = &conn{nc: nc, pending: map[int64]chan reply{}, dead: make(chan struct{})}
	go c.conn.readLoop()
	return c.conn, nil
}

// callTimeout bounds one request so a wedged add-on never hangs a caller.
func callTimeout(op string) time.Duration {
	if op == "markFolderRead" { // one update per message: slow on big folders
		return 10 * time.Minute
	}
	return 2 * time.Minute
}

// call sends op with params and decodes the response data into out (if non-nil).
func (c *Client) call(ctx context.Context, op string, params, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout(op))
	defer cancel()
	k, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()
	req := map[string]any{"id": id, "op": op}
	if params != nil {
		req["params"] = params
	}
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ch := make(chan reply, 1)
	k.mu.Lock()
	k.pending[id] = ch
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		delete(k.pending, id)
		k.mu.Unlock()
	}()
	k.wmu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		k.nc.SetWriteDeadline(dl)
	}
	_, err = k.nc.Write(append(line, '\n'))
	k.wmu.Unlock()
	if err != nil {
		k.nc.Close() // a partial line would corrupt the stream
		return disconnected(err)
	}
	select {
	case rp := <-ch:
		if !rp.OK {
			return &Error{Code: rp.Code, Msg: rp.Error}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(rp.Data, out); err != nil {
			return fmt.Errorf("%s: bad response: %w", op, err)
		}
		return nil
	case <-k.dead:
		return disconnected(errors.New("connection closed"))
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s: no answer from Thunderbird: %w", op, ctx.Err())
		}
		return ctx.Err()
	}
}

// Health checks that the add-on answers.
func (c *Client) Health(ctx context.Context) error { return c.call(ctx, "health", nil, nil) }

// Event is an add-on event such as newMail or changed (see addon/PROTOCOL.md).
type Event struct {
	Name string
	Data json.RawMessage
}

// Events subscribes to add-on events on a dedicated connection, redialing
// whenever it drops, until ctx ends (then the channel closes). Only the first
// dial's failure is returned.
func (c *Client) Events(ctx context.Context) (<-chan Event, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", c.socket())
	if err != nil {
		return nil, disconnected(err)
	}
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		for {
			cur := nc
			stop := context.AfterFunc(ctx, func() { cur.Close() })
			r := bufio.NewReader(nc)
			for {
				line, err := r.ReadBytes('\n')
				if err != nil {
					break
				}
				var rp reply
				if json.Unmarshal(line, &rp) != nil || rp.Event == "" {
					continue
				}
				select {
				case ch <- Event{Name: rp.Event, Data: rp.Data}:
				case <-ctx.Done():
				}
			}
			stop()
			nc.Close()
			for nc = nil; nc == nil; {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				nc, _ = d.DialContext(ctx, "unix", c.socket())
			}
		}
	}()
	return ch, nil
}

type Identity struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type Account struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Identities []Identity `json:"identities"`
}

type Folder struct {
	ID        string `json:"id"`
	AccountID string `json:"accountId"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Type      string `json:"type"`
	Unread    int    `json:"unreadMessageCount"`
	Total     int    `json:"totalMessageCount"`
	Depth     int    `json:"depth"`
}

type FolderRef struct {
	AccountID string `json:"accountId"`
	Path      string `json:"path"`
	Name      string `json:"name"`
}

// ID returns the folder id in tb's "accountN://path" form.
func (f FolderRef) ID() string { return f.AccountID + ":/" + f.Path }

type Message struct {
	ID         int       `json:"id"`
	Date       time.Time `json:"date"`
	Author     string    `json:"author"`
	Subject    string    `json:"subject"`
	Read       bool      `json:"read"`
	Flagged    bool      `json:"flagged"`
	Junk       bool      `json:"junk"`
	Tags       []string  `json:"tags"`
	Folder     FolderRef `json:"folder"`
	Recipients []string  `json:"recipients"`
	CcList     []string  `json:"ccList"`
	// HeaderMessageID is the RFC Message-ID: stable across folders, so caches key on it.
	HeaderMessageID string `json:"headerMessageId"`
}

type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	PartName    string `json:"partName"`
	Size        int    `json:"size"`
	ContentID   string `json:"contentId"` // only from Attachments(), for cid: references
}

// Attachments lists a message's parts including inline ones with content ids.
func (c *Client) Attachments(ctx context.Context, id int) (a []Attachment, err error) {
	return a, c.call(ctx, "attachments", map[string]any{"id": id}, &a)
}

// Download saves one part of a message to path.
func (c *Client) Download(ctx context.Context, id int, part, path string) error {
	var r struct {
		Data string `json:"data"`
	}
	if err := c.call(ctx, "download", map[string]any{"id": id, "part": part}, &r); err != nil {
		return err
	}
	b, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	return os.WriteFile(path, b, 0o600)
}

type FullMessage struct {
	Message
	Parts struct {
		Text        string       `json:"text"`
		HTML        string       `json:"html"`
		Attachments []Attachment `json:"attachments"`
	} `json:"parts"`
}

type Page struct {
	Messages []Message `json:"messages"`
	Total    int       `json:"total"`
	HasMore  bool      `json:"hasMore"`
}

func (c *Client) Accounts(ctx context.Context) (a []Account, err error) {
	return a, c.call(ctx, "accounts", nil, &a)
}

func (c *Client) Folders(ctx context.Context) (f []Folder, err error) {
	return f, c.call(ctx, "folders", nil, &f)
}

type SearchOpts struct {
	From, Folder, Since, Until, Subject string
	Unread, IncludeJunk                 bool
	Limit                               int
}

// Search returns messages matching o, newest first. A non-empty query is a
// full-text search over all time in Thunderbird's gloda index (sender,
// recipients, subject, attachment names and body, any case); the header
// filters in o then narrow the hits. An empty query is a header search in the
// add-on. Since and Until take RFC 3339, a date, today, yesterday or Nd/Nw/Nm/Ny.
func (c *Client) Search(ctx context.Context, query string, o SearchOpts) (Page, error) {
	since, err := ParseDate(o.Since, time.Now())
	if err != nil {
		return Page{}, err
	}
	until, err := ParseDate(o.Until, time.Now())
	if err != nil {
		return Page{}, err
	}
	if strings.TrimSpace(query) != "" {
		return c.fullText(ctx, query, o)
	}
	params := map[string]any{}
	for k, v := range map[string]string{"from": o.From, "subject": o.Subject, "folder": o.Folder} {
		if v != "" {
			params[k] = v
		}
	}
	if !since.IsZero() {
		params["since"] = since.UTC().Format(time.RFC3339)
	}
	if !until.IsZero() {
		params["until"] = until.UTC().Format(time.RFC3339)
	}
	if o.Unread {
		params["unread"] = true
	}
	if o.IncludeJunk {
		params["includeJunk"] = true
	}
	if o.Limit > 0 {
		params["limit"] = o.Limit
	}
	var p Page
	return p, c.call(ctx, "search", params, &p)
}

// fullText collects FullText's batches into one page, newest first.
func (c *Client) fullText(ctx context.Context, query string, o SearchOpts) (Page, error) {
	var out []Message
	more, err := c.FullText(ctx, query, o, func(ms []Message) bool { out = append(out, ms...); return true })
	if err != nil {
		return Page{}, err
	}
	sortNewestFirst(out)
	return Page{Messages: out, Total: len(out), HasMore: more}, nil
}

// FullText finds query in gloda (all time unless o bounds it) and resolves
// the hits to add-on messages newest first, passing each resolved batch to
// emit (which returns false to stop early), until o.Limit (0: 500) messages passed o's other filters. more
// reports that further matches exist. Resolving a hit costs Thunderbird a
// scan of its folder (~40 ms for 28k messages) once the account is known
// (learned from earlier hits in the same gloda folder), one per account with
// that folder path before.
func (c *Client) FullText(ctx context.Context, query string, o SearchOpts, emit func([]Message) bool) (more bool, err error) {
	since, err := ParseDate(o.Since, time.Now())
	if err != nil {
		return false, err
	}
	until, err := ParseDate(o.Until, time.Now())
	if err != nil {
		return false, err
	}
	db := c.Gloda
	if db == "" {
		dir, err := profile.Dir()
		if err != nil {
			return false, err
		}
		db = filepath.Join(dir, "global-messages-db.sqlite")
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 500
	}
	fetch := limit + 1
	if o.From != "" || o.Folder != "" || o.Unread || o.Subject != "" {
		fetch = max(fetch, 2000) // filters applied after resolving drop some hits
	}
	hits, err := gloda.Search(ctx, db, query, gloda.Opts{Since: since, Until: until, Limit: fetch})
	if err != nil {
		return false, fmt.Errorf("full-text search: %w", err)
	}
	if len(hits) == 0 {
		return false, nil
	}
	folders, err := c.Folders(ctx)
	if err != nil {
		return false, err
	}
	byPath := map[string][]string{}
	for _, f := range folders {
		if o.Folder == "" || f.ID == o.Folder {
			byPath[f.Path] = append(byPath[f.Path], f.ID)
		}
	}
	type lookup struct {
		ID      string   `json:"id"`
		Folders []string `json:"folders,omitempty"`
	}
	var lookups []lookup
	want := map[string]gloda.Hit{} // by Message-ID
	for _, h := range hits {
		if _, dup := want[h.HeaderMessageID]; dup {
			continue
		}
		cand := byPath[h.FolderPath]
		if id, ok := c.folderOf.Load(h.FolderURI); ok && slices.Contains(cand, id.(string)) {
			cand = []string{id.(string)}
		}
		if o.Folder != "" && len(cand) == 0 {
			continue // indexed in another folder
		}
		want[h.HeaderMessageID] = h
		lookups = append(lookups, lookup{h.HeaderMessageID, cand})
	}
	from, subj := strings.ToLower(o.From), strings.ToLower(o.Subject)
	n := 0
	const size = 20 // small batches: results show sooner and other requests interleave
	for i := 0; i < len(lookups); i += size {
		if n >= limit {
			return true, nil
		}
		var copies []Message
		if err := c.call(ctx, "byMessageId", map[string]any{"lookups": lookups[i:min(i+size, len(lookups))]}, &copies); err != nil {
			return false, err
		}
		// One copy per Message-ID: the one in gloda's folder, else the first.
		pick := map[string]Message{}
		inPath := map[string]int{} // copies in gloda's folder path, per Message-ID
		for _, m := range copies {
			path := want[m.HeaderMessageID].FolderPath
			if m.Folder.Path == path {
				inPath[m.HeaderMessageID]++
			}
			if have, ok := pick[m.HeaderMessageID]; !ok || (have.Folder.Path != path && m.Folder.Path == path) {
				pick[m.HeaderMessageID] = m
			}
		}
		var batch []Message
		for _, m := range pick {
			// Learn the account only when unambiguous: the same path (e.g. /INBOX) exists in every account.
			if h := want[m.HeaderMessageID]; inPath[m.HeaderMessageID] == 1 && m.Folder.Path == h.FolderPath {
				c.folderOf.Store(h.FolderURI, m.Folder.ID())
			}
			switch {
			case m.Junk && !o.IncludeJunk,
				o.Unread && m.Read,
				o.Folder != "" && m.Folder.ID() != o.Folder,
				from != "" && !strings.Contains(strings.ToLower(m.Author), from),
				subj != "" && !strings.Contains(strings.ToLower(m.Subject), subj):
				continue
			}
			batch = append(batch, m)
		}
		sortNewestFirst(batch)
		batch = batch[:min(len(batch), limit-n)]
		n += len(batch)
		if len(batch) > 0 && !emit(batch) {
			return true, nil
		}
	}
	return len(hits) >= fetch, nil
}

// ParseDate reads s as RFC 3339, a YYYY-MM-DD date (local midnight), today,
// yesterday, or Nd/Nw/Nm/Ny before now. Empty gives the zero time.
func ParseDate(s string, now time.Time) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch s {
	case "":
		return time.Time{}, nil
	case "today":
		return midnight, nil
	case "yesterday":
		return midnight.AddDate(0, 0, -1), nil
	}
	if n, err := strconv.Atoi(s[:len(s)-1]); err == nil && n >= 0 {
		switch s[len(s)-1] {
		case 'd':
			return now.AddDate(0, 0, -n), nil
		case 'w':
			return now.AddDate(0, 0, -7*n), nil
		case 'm':
			return now.AddDate(0, -n, 0), nil
		case 'y':
			return now.AddDate(-n, 0, 0), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, strings.ToUpper(s)); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("bad date %q: want RFC 3339, YYYY-MM-DD, today, yesterday or e.g. 7d, 2w, 3m, 1y", s)
}

// MergeNewest adds more to have without duplicates (by id), newest first.
func MergeNewest(have, more []Message) []Message {
	seen := map[int]bool{}
	out := make([]Message, 0, len(have)+len(more))
	for _, m := range append(append([]Message{}, have...), more...) {
		if !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	sortNewestFirst(out)
	return out
}

// Recent returns a folder's messages older than before (zero = now), newest
// first, plus the cursor for the next page. `tb list --sort` sorts the whole
// folder in the add-on (~20s for 26k messages), while date-bounded searches
// take well under a second, so this pages by date windows instead.
func (c *Client) Recent(ctx context.Context, folderID string, before time.Time, unread bool) (msgs []Message, next time.Time, more bool, err error) {
	const want, max = 50, 1000
	floor := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	until := before
	if until.IsZero() {
		until = time.Now().Add(24 * time.Hour)
	}
	span := 30 * 24 * time.Hour
	shrunk := false // once shrunk, never widen again, or the two branches can oscillate
	for {
		since := until.Add(-span)
		p, err := c.Search(ctx, "", SearchOpts{Folder: folderID, Unread: unread, IncludeJunk: true, Limit: max,
			Since: since.UTC().Format(time.RFC3339), Until: until.UTC().Format(time.RFC3339)})
		if err != nil {
			return nil, time.Time{}, false, err
		}
		// shortcut: a window under a minute with >1000 messages is returned truncated; fine for real mailboxes.
		if p.HasMore && span > time.Minute { // window too dense: shrink it
			span /= 4
			shrunk = true
			continue
		}
		if !shrunk && len(p.Messages) < want && since.After(floor) { // too sparse: widen it
			span *= 4
			continue
		}
		sortNewestFirst(p.Messages)
		return p.Messages, since, since.After(floor), nil
	}
}

// RecentMerged pages several folders as one newest-first list (e.g. all
// inboxes). Each folder is fetched from before; the page ends at the newest
// cursor among folders that have more, so nothing older than it is shown yet.
// Messages from other folders past that cursor are refetched on the next page;
// callers dedupe by id. If some folders fail, the rest are still returned
// together with the error; only all failing yields no messages.
func (c *Client) RecentMerged(ctx context.Context, folderIDs []string, before time.Time, unread bool) ([]Message, time.Time, bool, error) {
	if len(folderIDs) == 1 {
		return c.Recent(ctx, folderIDs[0], before, unread)
	}
	type res struct {
		msgs []Message
		next time.Time
		more bool
		err  error
	}
	res_ := make([]res, len(folderIDs))
	var wg sync.WaitGroup
	for i, id := range folderIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &res_[i]
			r.msgs, r.next, r.more, r.err = c.Recent(ctx, id, before, unread)
		}()
	}
	wg.Wait()
	var horizon time.Time
	more := false
	var errs []error
	for i, r := range res_ {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", folderIDs[i], r.err))
			continue
		}
		if r.more && r.next.After(horizon) {
			horizon, more = r.next, true
		}
	}
	var all []Message
	for _, r := range res_ {
		for _, m := range r.msgs {
			if !m.Date.Before(horizon) {
				all = append(all, m)
			}
		}
	}
	sortNewestFirst(all)
	if len(errs) == len(folderIDs) && len(errs) > 0 {
		return nil, time.Time{}, false, errors.Join(errs...)
	}
	return all, horizon, more, errors.Join(errs...)
}

func sortNewestFirst(ms []Message) {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Date.After(ms[j].Date) })
}

func (c *Client) Read(ctx context.Context, id int) (m FullMessage, err error) {
	return m, c.call(ctx, "read", map[string]any{"id": id}, &m)
}

// Headers returns a message's RFC 822 headers (e.g. List-Unsubscribe), which
// the JSON reads don't expose.
func (c *Client) Headers(ctx context.Context, id int) (mail.Header, error) {
	var r struct {
		Raw string `json:"raw"`
	}
	if err := c.call(ctx, "headers", map[string]any{"id": id}, &r); err != nil {
		return nil, err
	}
	head, _, _ := strings.Cut(strings.ReplaceAll(r.Raw, "\r\n", "\n"), "\n\n")
	msg, err := mail.ReadMessage(strings.NewReader(head + "\n\n"))
	if err != nil {
		return nil, fmt.Errorf("message headers: %w", err)
	}
	return msg.Header, nil
}

func (c *Client) Thread(ctx context.Context, id int) ([]Message, error) {
	var t struct {
		Thread []Message `json:"thread"`
	}
	err := c.call(ctx, "thread", map[string]any{"id": id}, &t)
	return t.Thread, err
}

// Mark sets a flag: one of read, unread, flagged, unflagged, junk, not-junk.
func (c *Client) Mark(ctx context.Context, ids []int, flag string) error {
	return c.call(ctx, "mark", map[string]any{"ids": ids, "flag": flag}, nil)
}

// MarkFolderRead marks every message in a folder as read.
func (c *Client) MarkFolderRead(ctx context.Context, folderID string) error {
	return c.call(ctx, "markFolderRead", map[string]any{"folder": folderID}, nil)
}

func (c *Client) Move(ctx context.Context, ids []int, folderID string) error {
	return c.call(ctx, "move", map[string]any{"ids": ids, "folder": folderID}, nil)
}

func (c *Client) Archive(ctx context.Context, ids []int) error {
	return c.call(ctx, "archive", map[string]any{"ids": ids}, nil)
}

// Delete moves messages to trash.
func (c *Client) Delete(ctx context.Context, ids []int) error {
	return c.call(ctx, "delete", map[string]any{"ids": ids}, nil)
}

type DraftKind int

const (
	NewMail DraftKind = iota
	Reply
	ReplyAll
	Forward
)

// Draft is an outgoing message. For Reply/ReplyAll only Body is used;
// Thunderbird fills recipients and subject from ReplyTo.
type Draft struct {
	Kind                  DraftKind
	ReplyTo               int // source message for Reply/ReplyAll/Forward
	From, To, Cc, Subject string
	Body                  string
}

// Submit saves the draft to Drafts, or sends it when send is true.
func (c *Client) Submit(ctx context.Context, d Draft, send bool) error {
	kind := [...]string{NewMail: "new", Reply: "reply", ReplyAll: "replyAll", Forward: "forward"}[d.Kind]
	params := map[string]any{"kind": kind, "body": d.Body, "send": send}
	switch d.Kind {
	case NewMail:
		params["to"], params["subject"] = d.To, d.Subject
		if d.Cc != "" {
			params["cc"] = d.Cc
		}
		if d.From != "" {
			params["from"] = d.From
		}
	case Reply, ReplyAll:
		params["replyTo"] = d.ReplyTo
	case Forward:
		params["replyTo"], params["to"] = d.ReplyTo, d.To
	}
	return c.call(ctx, "submit", params, nil)
}

// Ensure makes the add-on reachable, starting Thunderbird headless when it
// isn't running. progress gets human-readable status lines.
func (c *Client) Ensure(ctx context.Context, progress func(string)) error {
	err := c.Health(ctx)
	if err == nil {
		return nil
	}
	if exec.Command("pgrep", "-x", "thunderbird").Run() != nil && !handingOff() {
		progress("starting thunderbird --headless")
		if err := spawn("thunderbird", "--headless"); err != nil {
			return fmt.Errorf("start thunderbird: %w", err)
		}
	}
	progress("waiting for the Thunderbird add-on to connect (up to 60s)")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if err = c.Health(ctx); err == nil {
			return nil
		}
	}
	return fmt.Errorf("Thunderbird not reachable: %w (is the add-on installed? run `lazythunder install`)", err)
}

// spawn starts a long-lived daemon detached from our session, logging to the state dir.
func spawn(name string, args ...string) error {
	dir := filepath.Join(stateDir(), "lazythunder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// handoffFile marks a switch from the headless Thunderbird to a windowed one,
// so no lazythunder process starts a headless one in the gap between them.
func handoffFile() string { return filepath.Join(stateDir(), "lazythunder", "handoff") }

func handingOff() bool {
	fi, err := os.Stat(handoffFile())
	return err == nil && time.Since(fi.ModTime()) < 2*time.Minute
}

// thunderbirdPIDs finds our user's Thunderbird main processes (not content
// or helper processes), split by whether they were started with --headless.
func thunderbirdPIDs() (headless, windowed []int) {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if fi, err := os.Stat("/proc/" + e.Name()); err != nil || fi.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if filepath.Base(args[0]) != "thunderbird" || slices.Contains(args[1:], "-contentproc") {
			continue
		}
		if slices.Contains(args[1:], "--headless") {
			headless = append(headless, pid)
		} else {
			windowed = append(windowed, pid)
		}
	}
	return
}

// HeadlessPIDs lists our user's Thunderbird main processes started with --headless.
func HeadlessPIDs() []int { h, _ := thunderbirdPIDs(); return h }

// WindowedPIDs lists our user's Thunderbird main processes started without --headless.
func WindowedPIDs() []int { _, w := thunderbirdPIDs(); return w }

// StopHeadless stops every headless Thunderbird (SIGTERM, SIGKILL after 20s)
// and waits until they are gone.
func StopHeadless(ctx context.Context, progress func(string)) error {
	pids := HeadlessPIDs()
	if len(pids) > 0 {
		progress("stopping the headless Thunderbird")
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	for deadline := time.Now().Add(20 * time.Second); len(pids) > 0; pids = HeadlessPIDs() {
		if time.Now().After(deadline) {
			for _, pid := range pids { // it holds the profile lock
				syscall.Kill(pid, syscall.SIGKILL)
			}
			deadline = time.Now().Add(5 * time.Second)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

// OpenWindow replaces a headless Thunderbird (which holds the profile, so the
// normal launcher shows nothing) with a windowed one, then waits for the
// add-on to reconnect. With no headless instance it just asks Thunderbird for a window.
func (c *Client) OpenWindow(ctx context.Context, progress func(string)) error {
	if err := os.MkdirAll(filepath.Dir(handoffFile()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(handoffFile(), nil, 0o644); err != nil {
		return err
	}
	defer os.Remove(handoffFile())
	if err := StopHeadless(ctx, progress); err != nil { // the window can't open while it holds the profile
		return err
	}
	progress("opening Thunderbird")
	if err := spawn("thunderbird"); err != nil {
		return fmt.Errorf("start thunderbird: %w", err)
	}
	return c.Ensure(ctx, progress) // waits for the add-on in the new window's instance
}
