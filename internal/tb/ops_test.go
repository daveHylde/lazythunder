package tb

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordHub answers each op with replies[op] (nil if absent) and records the
// last request's params per op ("" when the request carried none).
func recordHub(t *testing.T, replies map[string]any) (*Client, func(op string) string) {
	t.Helper()
	var mu sync.Mutex
	got := map[string]string{}
	c := fakeHub(t, func(op string, params json.RawMessage) (any, *Error) {
		mu.Lock()
		got[op] = string(params)
		mu.Unlock()
		if e, ok := replies[op].(*Error); ok {
			return nil, e
		}
		return replies[op], nil
	})
	return c, func(op string) string {
		mu.Lock()
		defer mu.Unlock()
		p, ok := got[op]
		if !ok {
			t.Fatalf("op %q never sent", op)
		}
		return p
	}
}

func TestAccountsAndFoldersDecode(t *testing.T) {
	c, params := recordHub(t, map[string]any{
		"accounts": json.RawMessage(`[{"id":"account1","name":"Work","type":"imap","identities":[{"id":"id1","email":"me@x.no","name":"Me"}]}]`),
		"folders":  json.RawMessage(`[{"id":"account1://INBOX","accountId":"account1","name":"Inbox","path":"/INBOX","type":"inbox","unreadMessageCount":3,"totalMessageCount":40,"depth":0}]`),
	})
	ctx := context.Background()
	as, err := c.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Account{ID: "account1", Name: "Work", Type: "imap", Identities: []Identity{{ID: "id1", Email: "me@x.no", Name: "Me"}}}
	if len(as) != 1 || fmt.Sprint(as[0]) != fmt.Sprint(want) {
		t.Fatalf("accounts %+v", as)
	}
	fs, err := c.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0] != (Folder{ID: "account1://INBOX", AccountID: "account1", Name: "Inbox", Path: "/INBOX", Type: "inbox", Unread: 3, Total: 40}) {
		t.Fatalf("folders %+v", fs)
	}
	for _, op := range []string{"accounts", "folders"} {
		if p := params(op); p != "" && p != "null" {
			t.Errorf("%s sent params %s, want none", op, p)
		}
	}
}

func TestReadThreadAttachmentsDecode(t *testing.T) {
	c, params := recordHub(t, map[string]any{
		"read": json.RawMessage(`{"id":7,"subject":"Hi","author":"Kari <k@x.no>","date":"2026-11-01T10:00:00Z","read":true,"flagged":true,"tags":["$label1"],
			"folder":{"accountId":"account1","path":"/INBOX"},"recipients":["me@x.no"],"ccList":["b@x.no"],"headerMessageId":"abc@x",
			"parts":{"text":"plain","html":"<p>html</p>","attachments":[{"name":"a.pdf","contentType":"application/pdf","partName":"1.2","size":10}]}}`),
		"thread":      json.RawMessage(`{"thread":[{"id":1},{"id":2}]}`),
		"attachments": json.RawMessage(`[{"name":"logo.png","contentType":"image/png","partName":"1.3","size":5,"contentId":"logo@x"}]`),
	})
	ctx := context.Background()
	m, err := c.Read(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != 7 || m.Subject != "Hi" || !m.Read || !m.Flagged || m.HeaderMessageID != "abc@x" || m.Folder.ID() != "account1://INBOX" ||
		!m.Date.Equal(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)) || len(m.CcList) != 1 || len(m.Tags) != 1 {
		t.Fatalf("read message %+v", m.Message)
	}
	if m.Parts.Text != "plain" || m.Parts.HTML != "<p>html</p>" || len(m.Parts.Attachments) != 1 || m.Parts.Attachments[0].PartName != "1.2" {
		t.Fatalf("read parts %+v", m.Parts)
	}
	th, err := c.Thread(ctx, 7)
	if err != nil || len(th) != 2 || th[0].ID != 1 || th[1].ID != 2 {
		t.Fatalf("thread %+v %v", th, err)
	}
	as, err := c.Attachments(ctx, 7)
	if err != nil || len(as) != 1 || as[0] != (Attachment{Name: "logo.png", ContentType: "image/png", PartName: "1.3", Size: 5, ContentID: "logo@x"}) {
		t.Fatalf("attachments %+v %v", as, err)
	}
	for _, op := range []string{"read", "thread", "attachments"} {
		if p := params(op); p != `{"id":7}` {
			t.Errorf("%s params %s", op, p)
		}
	}
}

func TestHeadersParsing(t *testing.T) {
	for _, tc := range []struct {
		name, raw, key, want string
	}{
		{"CRLF", "Subject: hi\r\nList-Unsubscribe: <mailto:u@x>\r\n\r\nbody\r\nList-Unsubscribe: nope\r\n", "List-Unsubscribe", "<mailto:u@x>"},
		{"LF", "Subject: hi\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\n\nbody", "List-Unsubscribe-Post", "List-Unsubscribe=One-Click"},
		{"folded", "List-Unsubscribe: <https://x/u>,\r\n <mailto:u@x>\r\nSubject: s\r\n\r\n", "List-Unsubscribe", "<https://x/u>, <mailto:u@x>"},
		{"no body", "Subject: only headers", "Subject", "only headers"},
		{"body header ignored", "Subject: s\n\nX-Fake: 1\n", "X-Fake", ""},
		{"case-insensitive key", "list-unsubscribe: <https://y>\n\n", "List-Unsubscribe", "<https://y>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeHub(t, func(string, json.RawMessage) (any, *Error) { return map[string]string{"raw": tc.raw}, nil })
			h, err := c.Headers(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if got := h.Get(tc.key); got != tc.want {
				t.Fatalf("%s = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
	c := fakeHub(t, func(string, json.RawMessage) (any, *Error) {
		return map[string]string{"raw": "not a header line\n\n"}, nil
	})
	if _, err := c.Headers(context.Background(), 1); err == nil {
		t.Fatal("malformed headers accepted")
	}
}

func TestDownload(t *testing.T) {
	payload := []byte{0, 1, 2, 0xff, '\n', 'P', 'D', 'F'}
	c, params := recordHub(t, map[string]any{"download": map[string]string{"data": base64.StdEncoding.EncodeToString(payload)}})
	path := filepath.Join(t.TempDir(), "a.pdf")
	if err := c.Download(context.Background(), 9, "1.2", path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(payload) {
		t.Fatalf("wrote %v", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	if p := params("download"); p != `{"id":9,"part":"1.2"}` {
		t.Fatalf("params %s", p)
	}

	for name, reply := range map[string]any{
		"bad base64": map[string]string{"data": "!!!not base64"},
		"hub error":  &Error{Code: "NOT_FOUND", Msg: "no such part"},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := recordHub(t, map[string]any{"download": reply})
			path := filepath.Join(t.TempDir(), "x")
			if err := c.Download(context.Background(), 1, "1", path); err == nil {
				t.Fatal("want error")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("file written despite error")
			}
		})
	}
}

func TestMutatingOpsParams(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		op, want string
		do       func(*Client) error
	}{
		{"mark", `{"flag":"read","ids":[1]}`, func(c *Client) error { return c.Mark(ctx, []int{1}, "read") }},
		{"mark", `{"flag":"unread","ids":[1,2]}`, func(c *Client) error { return c.Mark(ctx, []int{1, 2}, "unread") }},
		{"mark", `{"flag":"flagged","ids":[3]}`, func(c *Client) error { return c.Mark(ctx, []int{3}, "flagged") }},
		{"mark", `{"flag":"unflagged","ids":[3]}`, func(c *Client) error { return c.Mark(ctx, []int{3}, "unflagged") }},
		{"mark", `{"flag":"junk","ids":[4]}`, func(c *Client) error { return c.Mark(ctx, []int{4}, "junk") }},
		{"mark", `{"flag":"not-junk","ids":[4]}`, func(c *Client) error { return c.Mark(ctx, []int{4}, "not-junk") }},
		{"markFolderRead", `{"folder":"account1://INBOX"}`, func(c *Client) error { return c.MarkFolderRead(ctx, "account1://INBOX") }},
		{"move", `{"folder":"account1://Archive/2026","ids":[5,6]}`, func(c *Client) error { return c.Move(ctx, []int{5, 6}, "account1://Archive/2026") }},
		{"archive", `{"ids":[7]}`, func(c *Client) error { return c.Archive(ctx, []int{7}) }},
		{"delete", `{"ids":[8,9]}`, func(c *Client) error { return c.Delete(ctx, []int{8, 9}) }},
		{"byMessageId", `{"lookups":[{"id":"abc@x"}]}`, func(c *Client) error { _, err := c.ByMessageID(ctx, "abc@x"); return err }},
		{"health", "", func(c *Client) error { return c.Health(ctx) }},
	} {
		t.Run(tc.op+tc.want, func(t *testing.T) {
			c, params := recordHub(t, nil)
			if err := tc.do(c); err != nil {
				t.Fatal(err)
			}
			if p := params(tc.op); p != tc.want && !(tc.want == "" && p == "null") {
				t.Fatalf("params %s, want %s", p, tc.want)
			}
		})
	}
}

func TestSubmitParams(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Draft
		send bool
		want string
	}{
		{"new draft", Draft{Kind: NewMail, To: "a@x", Subject: "S", Body: "B"}, false,
			`{"body":"B","kind":"new","send":false,"subject":"S","to":"a@x"}`},
		{"new send with from and cc", Draft{Kind: NewMail, From: "me@x", To: "a@x", Cc: "c@x", Subject: "S", Body: "B"}, true,
			`{"body":"B","cc":"c@x","from":"me@x","kind":"new","send":true,"subject":"S","to":"a@x"}`},
		{"reply ignores recipients", Draft{Kind: Reply, ReplyTo: 3, To: "x", Cc: "y", From: "z", Subject: "s", Body: "ok"}, false,
			`{"body":"ok","kind":"reply","replyTo":3,"send":false}`},
		{"reply all send", Draft{Kind: ReplyAll, ReplyTo: 4, Body: "ok"}, true,
			`{"body":"ok","kind":"replyAll","replyTo":4,"send":true}`},
		{"forward", Draft{Kind: Forward, ReplyTo: 5, To: "f@x", Subject: "ignored", Body: "fyi"}, true,
			`{"body":"fyi","kind":"forward","replyTo":5,"send":true,"to":"f@x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, params := recordHub(t, nil)
			if err := c.Submit(context.Background(), tc.d, tc.send); err != nil {
				t.Fatal(err)
			}
			if p := params("submit"); p != tc.want {
				t.Fatalf("params %s\nwant   %s", p, tc.want)
			}
		})
	}
}

func TestSearchHeaderParams(t *testing.T) {
	c, params := recordHub(t, map[string]any{"search": Page{Messages: []Message{{ID: 1}}, Total: 1, HasMore: true}})
	p, err := c.Search(context.Background(), "   ", SearchOpts{From: "kari", Subject: "faktura", Folder: "f",
		Since: "2026-01-02T03:04:05+02:00", Until: "2026-02-01T00:00:00Z", IncludeJunk: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 1 || p.Total != 1 || !p.HasMore {
		t.Fatalf("page %+v", p)
	}
	want := `{"folder":"f","from":"kari","includeJunk":true,"since":"2026-01-02T01:04:05Z","subject":"faktura","until":"2026-02-01T00:00:00Z"}`
	if got := params("search"); got != want {
		t.Fatalf("params %s\nwant   %s", got, want)
	}
}

func TestSearchBadDateSendsNothing(t *testing.T) {
	c := fakeHub(t, func(op string, _ json.RawMessage) (any, *Error) {
		t.Errorf("unexpected %s request", op)
		return nil, nil
	})
	for _, o := range []SearchOpts{{Since: "soon"}, {Until: "later"}} {
		if _, err := c.Search(context.Background(), "", o); err == nil || !strings.Contains(err.Error(), "bad date") {
			t.Errorf("%+v: got %v", o, err)
		}
	}
}

func TestErrorCodes(t *testing.T) {
	for _, code := range []string{"NOT_FOUND", "EXTENSION_DISCONNECTED", "INVALID_PARAMS"} {
		c, _ := recordHub(t, map[string]any{"archive": &Error{Code: code, Msg: "boom"}})
		err := c.Archive(context.Background(), []int{1})
		if Code(err) != code || err.Error() != "boom ("+code+")" {
			t.Errorf("got %v (code %q), want %s", err, Code(err), code)
		}
	}
	for _, err := range []error{nil, errors.New("plain"), context.Canceled} {
		if Code(err) != "" {
			t.Errorf("Code(%v) = %q, want empty", err, Code(err))
		}
	}
}

func TestBadResponseIsError(t *testing.T) {
	c, _ := recordHub(t, map[string]any{"folders": "not a list"})
	if _, err := c.Folders(context.Background()); err == nil || !strings.Contains(err.Error(), "folders: bad response") {
		t.Fatalf("got %v", err)
	}
}

// A hub that drops the connection mid-request yields EXTENSION_DISCONNECTED
// instead of hanging the caller.
func TestHubDropsConnectionMidCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(nc).ReadBytes('\n')
			nc.Close()
		}
	}()
	c := &Client{Socket: path}
	if err := c.Health(context.Background()); Code(err) != "EXTENSION_DISCONNECTED" {
		t.Fatalf("got %v", err)
	}
}

func TestCallTimeouts(t *testing.T) {
	// Marking a big folder read takes one update per message, so it gets far longer.
	if callTimeout("markFolderRead") <= callTimeout("read") || callTimeout("read") <= 0 {
		t.Fatalf("markFolderRead %v, read %v", callTimeout("markFolderRead"), callTimeout("read"))
	}
}

func TestEnsureHealthy(t *testing.T) {
	c, _ := recordHub(t, nil)
	err := c.Ensure(context.Background(), func(s string) { t.Errorf("unexpected progress %q", s) })
	if err != nil {
		t.Fatal(err)
	}
}
