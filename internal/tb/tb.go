// Package tb wraps the thunderbird-cli `tb` command (JSON output) and starts
// its bridge plus Thunderbird when they are not running.
package tb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

type Client struct {
	Bin  string // tb executable
	Port string // bridge HTTP port (7700 is often taken, e.g. by Meilisearch)
}

func New() *Client {
	port := os.Getenv("LAZYTHUNDER_BRIDGE_PORT")
	if port == "" {
		port = "7710"
	}
	return &Client{Bin: "tb", Port: port}
}

// Error is a failed tb envelope ({"ok":false,"error":...,"code":...}).
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Msg + " (" + e.Code + ")" }

func Code(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	Code  string          `json:"code"`
}

// runSlack is how long past tb's own timeout Run waits before killing it.
var runSlack = 30 * time.Second

// Run executes `tb args...` and decodes the envelope's data into out (if non-nil).
func (c *Client) Run(ctx context.Context, out any, args ...string) error {
	// tb has its own timeout (30s, or --timeout), but a wedged bridge or
	// Node process must never leave a caller waiting forever.
	limit := 30*time.Second + runSlack
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--timeout" {
			if ms, err := strconv.Atoi(args[i+1]); err == nil {
				limit = time.Duration(ms)*time.Millisecond + runSlack
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.WaitDelay = 5 * time.Second // don't wait on pipes held open by stray children
	cmd.Env = append(os.Environ(), "TB_BRIDGE_PORT="+c.Port)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("tb %s: no answer from Thunderbird after %s", args[0], limit.Round(time.Second))
	}
	name := args[0]
	for i := 0; i < len(args) && strings.HasPrefix(args[i], "-"); i += 2 { // skip global flags like --timeout N
		if i+2 < len(args) {
			name = args[i+2]
		}
	}
	var env envelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		if runErr != nil {
			return fmt.Errorf("tb %s: %v: %s", name, runErr, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("tb %s: bad JSON: %v", name, err)
	}
	if !env.OK {
		return &Error{Code: env.Code, Msg: env.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
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
	return a, c.Run(ctx, &a, "attachments", strconv.Itoa(id))
}

// Download saves one part of a message to path.
func (c *Client) Download(ctx context.Context, id int, part, path string) error {
	return c.Run(ctx, nil, "attachment-download", strconv.Itoa(id), part, "--output", path)
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
	return a, c.Run(ctx, &a, "accounts")
}

func (c *Client) Folders(ctx context.Context) (f []Folder, err error) {
	return f, c.Run(ctx, &f, "folders", "--all")
}

type SearchOpts struct {
	From, Folder, Since, Until, Subject string
	Unread, IncludeJunk                 bool
	Limit                               int
}

// SearchWindow bounds full-text searches without an explicit Since: an
// unbounded search over a large profile takes minutes in the add-on.
const SearchWindow = "90d"

func (c *Client) Search(ctx context.Context, query string, o SearchOpts) (Page, error) {
	lower := strings.ToLower(query)
	if lower == query {
		return c.search(ctx, query, o)
	}
	// The full-text index is case sensitive in practice ("Domene" finds
	// nothing, "domene" finds it), so also search lowercase and merge.
	var a, b Page
	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); b, errB = c.search(ctx, lower, o) }()
	a, errA = c.search(ctx, query, o)
	wg.Wait()
	if errA != nil && errB != nil {
		return Page{}, errA
	}
	a.Messages = MergeNewest(a.Messages, b.Messages)
	a.HasMore = a.HasMore || b.HasMore
	a.Total = len(a.Messages)
	return a, nil
}

func (c *Client) search(ctx context.Context, query string, o SearchOpts) (p Page, err error) {
	args := []string{"--timeout", "120000", "search", query}
	if query != "" && o.Since == "" {
		o.Since = SearchWindow
	}
	if o.From != "" {
		args = append(args, "--from", o.From)
	}
	if o.Folder != "" {
		args = append(args, "--folder", o.Folder)
	}
	if o.Since != "" {
		args = append(args, "--since", o.Since)
	}
	if o.Subject != "" {
		args = append(args, "--subject="+o.Subject) // one argv: a subject may start with "-"
	}
	if o.Until != "" {
		args = append(args, "--until", o.Until)
	}
	if o.Unread {
		args = append(args, "--unread")
	}
	if o.IncludeJunk {
		args = append(args, "--include-junk")
	}
	if o.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(o.Limit))
	}
	return p, c.Run(ctx, &p, args...)
}

// SearchHeaders matches query against sender and subject over all time,
// newest first. These native header queries take a second or two, unlike
// the full-text index, so search shows them first.
func (c *Client) SearchHeaders(ctx context.Context, query string, unread bool) ([]Message, error) {
	opts := []SearchOpts{{From: query}}     // author matching ignores case
	for _, v := range caseVariants(query) { // subject matching does not
		opts = append(opts, SearchOpts{Subject: v})
	}
	pages := make([]Page, len(opts))
	errs := make([]error, len(opts))
	var wg sync.WaitGroup
	for i, o := range opts {
		o.Unread, o.Limit = unread, 500
		wg.Add(1)
		go func() {
			defer wg.Done()
			pages[i], errs[i] = c.Search(ctx, "", o)
		}()
	}
	wg.Wait()
	var all []Message
	for _, p := range pages {
		all = append(all, p.Messages...)
	}
	all = MergeNewest(nil, all)
	failed := 0
	for _, e := range errs {
		if e != nil {
			failed++
		}
	}
	if failed == len(errs) {
		return nil, errors.Join(errs...)
	}
	return all, errors.Join(errs...)
}

// caseVariants are the spellings a case-sensitive match must try: as typed,
// lower, UPPER, Capitalized and Title Case.
func caseVariants(q string) []string {
	title := strings.Fields(strings.ToLower(q))
	for i, w := range title {
		r := []rune(w)
		title[i] = strings.ToUpper(string(r[:1])) + string(r[1:])
	}
	lower := strings.ToLower(q)
	r := []rune(lower)
	capital := lower
	if len(r) > 0 {
		capital = strings.ToUpper(string(r[:1])) + string(r[1:])
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range []string{q, lower, strings.ToUpper(q), capital, strings.Join(title, " ")} {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
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
	return m, c.Run(ctx, &m, "read", strconv.Itoa(id), "--full")
}

// Headers returns a message's RFC 822 headers (e.g. List-Unsubscribe), which
// the JSON reads don't expose.
func (c *Client) Headers(ctx context.Context, id int) (mail.Header, error) {
	var r struct {
		Raw string `json:"raw"`
	}
	if err := c.Run(ctx, &r, "read", strconv.Itoa(id), "--raw"); err != nil {
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
	err := c.Run(ctx, &t, "thread", strconv.Itoa(id), "--headers")
	return t.Thread, err
}

func joinIDs(ids []int) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.Itoa(id)
	}
	return strings.Join(s, ",")
}

// Mark sets a flag: one of read, unread, flagged, unflagged, junk, not-junk.
func (c *Client) Mark(ctx context.Context, ids []int, flag string) error {
	return c.Run(ctx, nil, "mark", joinIDs(ids), "--"+flag)
}

// MarkFolderRead marks every message in a folder as read. tb's bulk
// mark-read handles one batch of unread messages per call, so repeat until a
// batch comes back short.
func (c *Client) MarkFolderRead(ctx context.Context, folderID string) error {
	const batch = 500
	for range 200 { // cap: never spin if marks don't stick

		var out struct {
			Marked int `json:"marked"`
		}
		if err := c.Run(ctx, &out, "--timeout", "300000", "bulk", "mark-read", folderID, "--limit", strconv.Itoa(batch)); err != nil { // one update per message: slow
			return err
		}
		if out.Marked < batch {
			return nil
		}
	}
	return fmt.Errorf("%s still has unread messages after marking 100000", folderID)
}

func (c *Client) Move(ctx context.Context, ids []int, folderID string) error {
	return c.Run(ctx, nil, "move", joinIDs(ids), folderID)
}

func (c *Client) Archive(ctx context.Context, ids []int) error {
	return c.Run(ctx, nil, "archive", joinIDs(ids))
}

// Delete moves messages to trash.
func (c *Client) Delete(ctx context.Context, ids []int) error {
	return c.Run(ctx, nil, "delete", joinIDs(ids))
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
	f, err := os.CreateTemp("", "lazythunder-body-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(d.Body); err != nil {
		f.Close()
		return err
	}
	f.Close()

	var args []string
	switch d.Kind {
	case NewMail:
		args = []string{"compose", "--to", d.To, "--subject", d.Subject}
		if d.Cc != "" {
			args = append(args, "--cc", d.Cc)
		}
		if d.From != "" {
			args = append(args, "--from", d.From)
		}
	case Reply, ReplyAll:
		args = []string{"reply", strconv.Itoa(d.ReplyTo)}
		if d.Kind == ReplyAll {
			args = append(args, "--all")
		}
	case Forward:
		args = []string{"forward", strconv.Itoa(d.ReplyTo), "--to", d.To}
	}
	mode := "--draft"
	if send {
		mode = "--send"
	}
	if d.Kind == Forward {
		// tb forward has no --body-file
		return c.Run(ctx, nil, append(args, "--body", d.Body, mode)...)
	}
	return c.Run(ctx, nil, append(args, "--body-file", f.Name(), mode)...)
}

// Ensure makes the bridge and Thunderbird available, starting whichever is
// missing (Thunderbird headless). progress gets human-readable status lines.
func (c *Client) Ensure(ctx context.Context, progress func(string)) error {
	err := c.Run(ctx, nil, "health")
	if err == nil {
		return nil
	}
	if Code(err) != "EXTENSION_DISCONNECTED" {
		progress("starting tb-bridge on port " + c.Port)
		if err := spawn("tb-bridge", "--port", c.Port); err != nil {
			return fmt.Errorf("start tb-bridge: %w", err)
		}
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
		if err = c.Run(ctx, nil, "health"); err == nil {
			return nil
		}
	}
	return fmt.Errorf("Thunderbird not reachable: %w (is the thunderbird-cli add-on installed?)", err)
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

// headlessPIDs finds our user's Thunderbird main processes started with --headless.
func headlessPIDs() []int {
	var pids []int
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
		if filepath.Base(args[0]) == "thunderbird" && slices.Contains(args[1:], "--headless") {
			pids = append(pids, pid)
		}
	}
	return pids
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
	pids := headlessPIDs()
	if len(pids) > 0 {
		progress("stopping the headless Thunderbird")
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	for deadline := time.Now().Add(20 * time.Second); len(pids) > 0; pids = headlessPIDs() {
		if time.Now().After(deadline) {
			for _, pid := range pids { // it holds the profile lock: the window can't open until it is gone
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
	progress("opening Thunderbird")
	if err := spawn("thunderbird"); err != nil {
		return fmt.Errorf("start thunderbird: %w", err)
	}
	return c.Ensure(ctx, progress) // waits for the add-on in the new window's instance
}
