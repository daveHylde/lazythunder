// Package tb wraps the thunderbird-cli `tb` command (JSON output) and starts
// its bridge plus Thunderbird when they are not running.
package tb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// Run executes `tb args...` and decodes the envelope's data into out (if non-nil).
func (c *Client) Run(ctx context.Context, out any, args ...string) error {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = append(os.Environ(), "TB_BRIDGE_PORT="+c.Port)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var env envelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		if runErr != nil {
			return fmt.Errorf("tb %s: %v: %s", args[0], runErr, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("tb %s: bad JSON: %v", args[0], err)
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
}

type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	PartName    string `json:"partName"`
	Size        int    `json:"size"`
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
	From, Folder, Since, Until string
	Unread, IncludeJunk        bool
	Limit                      int
}

func (c *Client) Search(ctx context.Context, query string, o SearchOpts) (p Page, err error) {
	args := []string{"search", query}
	if o.From != "" {
		args = append(args, "--from", o.From)
	}
	if o.Folder != "" {
		args = append(args, "--folder", o.Folder)
	}
	if o.Since != "" {
		args = append(args, "--since", o.Since)
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

func sortNewestFirst(ms []Message) {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Date.After(ms[j].Date) })
}

func (c *Client) Read(ctx context.Context, id int) (m FullMessage, err error) {
	return m, c.Run(ctx, &m, "read", strconv.Itoa(id), "--full")
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
	if exec.Command("pgrep", "-x", "thunderbird").Run() != nil {
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
