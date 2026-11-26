package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/daveHylde/lazythunder/internal/tb"
)

type hubCall struct {
	Op     string
	Params map[string]any
}

// fakeHub serves the add-on protocol on a temp socket: answers[op] is the
// data for op, or an error message when it is a string prefixed "ERR:".
// It returns a client dialing it and a func listing the calls received.
func fakeHub(t *testing.T, answers map[string]any) (*tb.Client, func() []hubCall) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []hubCall
	var conns []net.Conn
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, nc)
			mu.Unlock()
			go func() {
				r := bufio.NewReader(nc)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var req struct {
						ID     json.RawMessage
						Op     string
						Params map[string]any
					}
					json.Unmarshal(line, &req)
					mu.Lock()
					calls = append(calls, hubCall{req.Op, req.Params})
					mu.Unlock()
					resp := map[string]any{"id": req.ID, "ok": true, "data": answers[req.Op]}
					if s, ok := answers[req.Op].(string); ok && strings.HasPrefix(s, "ERR:") {
						resp = map[string]any{"id": req.ID, "ok": false, "error": s[4:], "code": "E"}
					}
					b, _ := json.Marshal(resp)
					nc.Write(append(b, '\n'))
				}
			}()
		}
	}()
	return &tb.Client{Socket: path}, func() []hubCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]hubCall(nil), calls...)
	}
}

var day = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

func msg(id int, subj string, read, flagged bool) map[string]any {
	return map[string]any{"id": id, "date": day.Add(-time.Duration(id) * time.Hour), "author": "Ann <ann@x.no>",
		"subject": subj, "read": read, "flagged": flagged, "folder": map[string]any{"accountId": "a1", "path": "INBOX"}}
}

// okHooks accepts every confirmation and fails the test on drafts.
func okHooks(t *testing.T, confirms *[]string) Hooks {
	return Hooks{
		Confirm: func(s string) bool { *confirms = append(*confirms, s); return true },
		Propose: func(tb.Draft) { t.Error("unexpected draft") },
		Status:  func(string) {},
	}
}

func TestToolsReachHub(t *testing.T) {
	c, calls := fakeHub(t, map[string]any{
		"folders": []map[string]any{
			{"id": "a1", "name": "Account", "depth": 0},
			{"id": "a1:/INBOX", "name": "Inbox", "unreadMessageCount": 2, "totalMessageCount": 10, "depth": 1},
		},
		"search": map[string]any{"messages": []any{msg(1, "one", false, true), msg(2, "two", true, false), msg(3, "three", true, false)}},
		"read": func() map[string]any {
			m := msg(7, "Faktura", true, false)
			m["recipients"] = []string{"me@x.no"}
			m["ccList"] = []string{"boss@x.no"}
			m["parts"] = map[string]any{"text": "Please pay 100 kr.", "attachments": []any{
				map[string]any{"name": "inv.pdf", "contentType": "application/pdf", "size": 1234}}}
			return m
		}(),
		"thread":  map[string]any{"thread": []any{msg(7, "Faktura", true, false), msg(8, "Re: Faktura", false, false)}},
		"mark":    nil,
		"move":    nil,
		"archive": nil,
		"delete":  nil,
	})
	for _, tc := range []struct {
		tool, args string
		op         string
		params     map[string]any // subset checked
		confirm    string
		want       []string // substrings of the tool result
		notWant    []string
	}{
		{tool: "list_folders", args: `{}`, op: "folders",
			want: []string{"a1:/INBOX | Inbox | 2 unread / 10"}, notWant: []string{"Account"}},
		{tool: "search", args: `{"query":"","from":"ann","unread_only":true}`, op: "search",
			params: map[string]any{"from": "ann", "unread": true, "limit": 20.0},
			want:   []string{"id 1 |", "one | a1:/INBOX unread flagged", "id 3 |"}},
		{tool: "search", args: `{"query":"","limit":500}`, op: "search", params: map[string]any{"limit": 20.0}},
		{tool: "search", args: `{"query":"","limit":3}`, op: "search", params: map[string]any{"limit": 3.0}},
		{tool: "list_messages", args: `{"folder_id":"a1:/INBOX","limit":2}`, op: "search",
			params: map[string]any{"folder": "a1:/INBOX", "includeJunk": true},
			want:   []string{"id 1 |", "id 2 |"}, notWant: []string{"id 3 |"}},
		{tool: "read_message", args: `{"id":7}`, op: "read", params: map[string]any{"id": 7.0},
			want: []string{"id 7\nFrom: Ann <ann@x.no>\nTo: me@x.no\nCc: boss@x.no\n", "Subject: Faktura", "Folder: a1:/INBOX",
				"Attachment: inv.pdf (application/pdf, 1234 bytes)", "Please pay 100 kr."}},
		{tool: "get_thread", args: `{"id":7}`, op: "thread", params: map[string]any{"id": 7.0},
			want: []string{"id 7 |", "Re: Faktura | a1:/INBOX unread"}},
		{tool: "mark", args: `{"ids":[1,2],"flag":"read"}`, op: "mark", confirm: "mark [1 2] as read",
			params: map[string]any{"flag": "read", "ids": []any{1.0, 2.0}}, want: []string{"done"}},
		{tool: "move", args: `{"ids":[3],"folder_id":"a1:/Old"}`, op: "move", confirm: "move [3] to a1:/Old",
			params: map[string]any{"folder": "a1:/Old", "ids": []any{3.0}}, want: []string{"done"}},
		{tool: "archive", args: `{"ids":[4]}`, op: "archive", confirm: "archive [4]",
			params: map[string]any{"ids": []any{4.0}}, want: []string{"done"}},
		{tool: "delete", args: `{"ids":[5]}`, op: "delete", confirm: "move [5] to trash",
			params: map[string]any{"ids": []any{5.0}}, want: []string{"done"}},
	} {
		t.Run(tc.tool+tc.args, func(t *testing.T) {
			before := len(calls())
			var confirms []string
			a := &Agent{TB: c}
			out := a.call(context.Background(), tc.tool, tc.args, okHooks(t, &confirms))
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("result missing %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("result has %q:\n%s", w, out)
				}
			}
			got := calls()[before:]
			if len(got) == 0 || got[len(got)-1].Op != tc.op {
				t.Fatalf("hub calls %+v, want op %s", got, tc.op)
			}
			last := got[len(got)-1].Params
			for k, v := range tc.params {
				if gb, _ := json.Marshal(last[k]); string(gb) != mustJSON(v) {
					t.Errorf("param %s = %s, want %s", k, gb, mustJSON(v))
				}
			}
			if tc.confirm == "" && len(confirms) > 0 || tc.confirm != "" && (len(confirms) != 1 || confirms[0] != tc.confirm) {
				t.Errorf("confirms %q, want %q", confirms, tc.confirm)
			}
		})
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestMutatingToolsDeclinedDoNothing(t *testing.T) {
	c, calls := fakeHub(t, map[string]any{})
	for tool, args := range map[string]string{
		"mark":    `{"ids":[1],"flag":"junk"}`,
		"move":    `{"ids":[1],"folder_id":"a1:/X"}`,
		"archive": `{"ids":[1]}`,
		"delete":  `{"ids":[1]}`,
	} {
		asked := 0
		a := &Agent{TB: c}
		out := a.call(context.Background(), tool, args, Hooks{Confirm: func(string) bool { asked++; return false }})
		if asked != 1 || !strings.HasPrefix(out, "error: NOT DONE") {
			t.Errorf("%s: asked %d, result %q", tool, asked, out)
		}
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("declined actions reached Thunderbird: %+v", got)
	}
}

func TestToolArgumentValidation(t *testing.T) {
	c, calls := fakeHub(t, map[string]any{"read": "ERR:no such message"})
	for _, tc := range []struct{ tool, args, want string }{
		{"read_message", `{"id":`, "error: bad arguments"},
		{"read_message", `{"id":"seven"}`, "error: bad arguments"},
		{"mark", `{"ids":[],"flag":"read"}`, "error: ids is empty"},
		{"archive", `{}`, "error: ids is empty"},
		{"delete", `{"ids":null}`, "error: ids is empty"},
		{"mark", `{"ids":[1],"flag":"important"}`, `error: invalid flag "important"`},
		{"propose_draft", `{"kind":"memo","body":"x"}`, `error: unknown kind "memo"`},
		{"propose_draft", `{"kind":"reply","body":"x"}`, "error: message_id is required for reply"},
		{"propose_draft", `{"kind":"forward","body":"x"}`, "error: message_id is required for forward"},
		{"rm_rf", `{}`, `error: unknown tool "rm_rf"`},
		{"search", `{"query":"","since":"someday"}`, "error: "},
		{"read_message", `{"id":9}`, "error: no such message"},
	} {
		a := &Agent{TB: c}
		out := a.call(context.Background(), tc.tool, tc.args, Hooks{
			Confirm: func(string) bool { t.Errorf("%s %s: asked to confirm", tc.tool, tc.args); return true },
			Propose: func(tb.Draft) { t.Errorf("%s %s: proposed", tc.tool, tc.args) },
		})
		if !strings.HasPrefix(out, tc.want) {
			t.Errorf("%s %s = %q, want prefix %q", tc.tool, tc.args, out, tc.want)
		}
	}
	for _, cl := range calls() {
		if cl.Op != "read" {
			t.Errorf("invalid call reached hub: %+v", cl)
		}
	}
}

func TestProposeDraftKinds(t *testing.T) {
	for _, tc := range []struct {
		args string
		want tb.Draft
	}{
		{`{"kind":"new","to":"a@x.no","cc":"b@x.no","subject":"Hi","from":"me@x.no","body":"Hello"}`,
			tb.Draft{Kind: tb.NewMail, From: "me@x.no", To: "a@x.no", Cc: "b@x.no", Subject: "Hi", Body: "Hello"}},
		{`{"kind":"reply_all","message_id":3,"body":"Thanks"}`, tb.Draft{Kind: tb.ReplyAll, ReplyTo: 3, Body: "Thanks"}},
		{`{"kind":"forward","message_id":4,"to":"c@x.no","body":"FYI"}`, tb.Draft{Kind: tb.Forward, ReplyTo: 4, To: "c@x.no", Body: "FYI"}},
	} {
		var got []tb.Draft
		a := &Agent{}
		out := a.call(context.Background(), "propose_draft", tc.args, Hooks{
			Confirm: func(string) bool { t.Error("drafts need no confirmation"); return false },
			Propose: func(d tb.Draft) { got = append(got, d) },
		})
		if len(got) != 1 || got[0] != tc.want || strings.HasPrefix(out, "error") {
			t.Errorf("%s: got %+v, %q", tc.args, got, out)
		}
	}
}

func TestFormatListStates(t *testing.T) {
	if got := FormatList(nil); got != "no messages" {
		t.Fatalf("empty: %q", got)
	}
	ms := []tb.Message{
		{ID: 1, Subject: "a", Read: true},
		{ID: 2, Subject: "b", Read: false, Flagged: true},
		{ID: 3, Subject: "c", Read: true, Flagged: true},
	}
	lines := strings.Split(strings.TrimRight(FormatList(ms), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines %q", lines)
	}
	for i, suffix := range []string{":/", ":/ unread flagged", ":/ flagged"} {
		if !strings.HasSuffix(lines[i], suffix) {
			t.Errorf("line %d %q, want suffix %q", i, lines[i], suffix)
		}
	}
}

func TestNewReadsProviderEnv(t *testing.T) {
	for _, tc := range []struct {
		url, model, key   string
		wantURL, wantName string
		wantEnabled       bool
	}{
		{"", "", "", "http://127.0.0.1:8085/v1", "assistant", true},
		{"https://api.example.com/v1/", "gpt-x", "sk-1", "https://api.example.com/v1", "gpt-x", true},
		{"off", "", "", "off", "assistant", false},
	} {
		t.Setenv("LAZYTHUNDER_LLM_URL", tc.url)
		t.Setenv("LAZYTHUNDER_MODEL", tc.model)
		t.Setenv("LAZYTHUNDER_API_KEY", tc.key)
		a := New(nil)
		if a.URL != tc.wantURL || a.Model != tc.model || a.APIKey != tc.key || a.Name() != tc.wantName || a.Enabled() != tc.wantEnabled {
			t.Errorf("env %q/%q/%q: got %+v name %q enabled %v", tc.url, tc.model, tc.key, a, a.Name(), a.Enabled())
		}
		if a.HTTP == nil || a.HTTP.Timeout == 0 {
			t.Errorf("HTTP client without timeout")
		}
	}
	var nilAgent *Agent
	if nilAgent.Enabled() || nilAgent.Name() != "assistant" {
		t.Error("nil agent must be disabled")
	}
}

// llm serves reply(n) for the nth request (from 0) and records request bodies.
func llm(t *testing.T, reply func(n int) string) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, req)
		mu.Unlock()
		io.WriteString(w, reply(n))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), reqs...)
	}
}

func choice(m string) string { return `{"choices":[{"message":` + m + `}]}` }

func TestAskReadsThroughHubAndKeepsHistory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	big := strings.Repeat("æ", 6000) // 12000 bytes: tool results are clipped to 8000
	c, _ := fakeHub(t, map[string]any{
		"accounts": []map[string]any{{"id": "acc1", "identities": []map[string]any{{"id": "id1", "email": "me@x.no", "name": "Me"}}}},
		"read":     map[string]any{"id": 7, "subject": "Big", "parts": map[string]any{"text": big}},
	})
	srv, reqs := llm(t, func(n int) string {
		if n%2 == 0 {
			return choice(`{"content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"read_message","arguments":"{\"id\":7}"}}]}`)
		}
		return choice(`{"content":"  It is big.  "}`)
	})
	a := &Agent{URL: srv.URL, TB: c, HTTP: srv.Client()}
	var status []string
	h := Hooks{Confirm: func(string) bool { return false }, Propose: func(tb.Draft) {}, Status: func(s string) { status = append(status, s) }}
	text, err := a.Ask(context.Background(), "what is 7?", "Inbox", h)
	if err != nil || text != "It is big." {
		t.Fatalf("got %q, %v", text, err)
	}
	if !strings.Contains(strings.Join(status, "\n"), `read_message {"id":7}`) {
		t.Errorf("status lines %q", status)
	}
	r := reqs()
	if len(r) != 2 || r[0]["tools"] == nil {
		t.Fatalf("requests %d, tools sent: %v", len(r), r[0]["tools"] != nil)
	}
	if _, has := r[0]["model"]; has {
		t.Error("model sent though none configured")
	}
	msgs := r[1]["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "account acc1: Me <me@x.no> (identity id1)") || !strings.Contains(sys, "Current view: Inbox") {
		t.Errorf("system prompt: %s", sys)
	}
	tool := msgs[len(msgs)-1].(map[string]any)
	out := tool["content"].(string)
	if tool["role"] != "tool" || tool["tool_call_id"] != "t1" || !strings.HasSuffix(out, "…[truncated]") || len(out) > 8100 || !utf8.ValidString(out) {
		t.Errorf("tool message role=%v id=%v len=%d valid=%v", tool["role"], tool["tool_call_id"], len(out), utf8.ValidString(out))
	}
	// Second turn reuses the conversation; Reset forgets it.
	if _, err := a.Ask(context.Background(), "again", "Drafts", h); err != nil {
		t.Fatal(err)
	}
	r = reqs()
	msgs = r[len(r)-1]["messages"].([]any)
	if !strings.Contains(msgs[0].(map[string]any)["content"].(string), "Current view: Drafts") || len(msgs) < 6 {
		t.Errorf("second turn lost history or stale view: %d msgs", len(msgs))
	}
	a.Reset()
	if a.History != nil {
		t.Error("Reset kept history")
	}
}

func TestAskGivesUpAfterTenToolRounds(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv, reqs := llm(t, func(int) string {
		return choice(`{"content":"","tool_calls":[{"id":"x","type":"function","function":{"name":"nope","arguments":"{}"}}]}`)
	})
	a := &Agent{URL: srv.URL, HTTP: srv.Client(), accounts: "-\n"}
	_, err := a.Ask(context.Background(), "loop", "", Hooks{Status: func(string) {}})
	if err == nil || !strings.Contains(err.Error(), "gave up") || len(reqs()) != 10 {
		t.Fatalf("err %v after %d requests", err, len(reqs()))
	}
}

func TestAskHubDownFailsBeforeLLM(t *testing.T) {
	srv, reqs := llm(t, func(int) string { return choice(`{"content":"hi"}`) })
	a := &Agent{URL: srv.URL, HTTP: srv.Client(), TB: &tb.Client{Socket: filepath.Join(t.TempDir(), "none.sock")}}
	if _, err := a.Ask(context.Background(), "hi", "", Hooks{Status: func(string) {}}); err == nil || len(reqs()) != 0 {
		t.Fatalf("err %v, requests %d", err, len(reqs()))
	}
}

func TestProviderErrorsSurface(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"message":"model not loaded"}}`: "AI: model not loaded",
		`{"choices":[]}`:           "empty response",
		`<html>bad gateway</html>`: "AI: 200 OK",
	} {
		srv, _ := llm(t, func(int) string { return body })
		a := &Agent{URL: srv.URL, HTTP: srv.Client(), accounts: "-\n"}
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		_, err := a.Ask(context.Background(), "hi", "", Hooks{Status: func(string) {}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", body, err, want)
		}
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	a := &Agent{URL: url, HTTP: http.DefaultClient}
	if _, err := a.Summarize(context.Background(), "", "", "", nil); err == nil || !strings.Contains(err.Error(), "AI at "+url) {
		t.Errorf("unreachable server: %v", err)
	}
}

func TestSummarizeBoundsPromptAndDecodes(t *testing.T) {
	srv, reqs := llm(t, func(int) string {
		return choice(`{"content":"{\"summary\":\"  Pay invoice  \",\"actions\":[{\"text\":\"Pay\",\"link\":1}],\"links\":[{\"n\":1,\"title\":\"Pay the invoice\",\"suspicious\":false}],\"category\":\"receipt\",\"priority\":\"high\"}"}`)
	})
	a := &Agent{URL: srv.URL, HTTP: srv.Client()}
	links := make([]string, 30)
	for i := range links {
		links[i] = "https://x.no/" + strings.Repeat("p", 300)
	}
	s, err := a.Summarize(context.Background(), "Fiken", "Faktura", strings.Repeat("b", 7000), links)
	if err != nil {
		t.Fatal(err)
	}
	if s.Summary != "Pay invoice" || s.Version != SummaryVersion || s.Category != "receipt" || s.Priority != "high" ||
		len(s.Actions) != 1 || s.Actions[0].Link != 1 || len(s.Links) != 1 || s.Links[0].Title != "Pay the invoice" {
		t.Fatalf("summary %+v", s)
	}
	user := reqs()[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.HasPrefix(user, "From: Fiken\nSubject: Faktura\n\n") || strings.Contains(user, strings.Repeat("b", 6001)) {
		t.Error("body not truncated to 6000")
	}
	if !strings.Contains(user, "[25] ") || strings.Contains(user, "[26] ") || strings.Contains(user, strings.Repeat("p", 200)) {
		t.Error("links not capped to MaxLinks / 200 chars")
	}
	if rf := reqs()[0]["response_format"].(map[string]any); rf["type"] != "json_schema" {
		t.Errorf("response_format %v", rf)
	}

	srv2, _ := llm(t, func(int) string { return choice(`{"content":"Sure! Here is the summary"}`) })
	a.URL = srv2.URL
	if _, err := a.Summarize(context.Background(), "", "", "x", nil); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("non-JSON reply: %v", err)
	}
}

func TestUnsubscribeStepDecodes(t *testing.T) {
	srv, reqs := llm(t, func(int) string {
		return choice(`{"content":"{\"result\":\"submit\",\"form\":2,\"fields\":[{\"name\":\"email\",\"value\":\"me@x.no\"}],\"link\":0,\"explain\":\"form 2\"}"}`)
	})
	a := &Agent{URL: srv.URL, HTTP: srv.Client()}
	s, err := a.UnsubscribeStep(context.Background(), "me@x.no", "PAGE")
	if err != nil || s.Result != "submit" || s.Form != 2 || len(s.Fields) != 1 || s.Fields[0] != (Field{"email", "me@x.no"}) {
		t.Fatalf("%+v %v", s, err)
	}
	user := reqs()[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "User's address: me@x.no") || !strings.Contains(user, "PAGE") {
		t.Errorf("prompt %q", user)
	}
}

func TestReplyGuideFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	got := ReplyGuide()
	if strings.Contains(got, "# How") || !strings.Contains(got, "Best regards") {
		t.Fatalf("default guide %q", got)
	}
	path := filepath.Join(dir, "lazythunder", "reply-guide.md")
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "# How the assistant") {
		t.Fatalf("default not written to %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("# my notes\nSign as Dave.\n  # indented comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReplyGuide(); got != "Sign as Dave." {
		t.Fatalf("edited guide %q", got)
	}
}

func TestSummarizeKeepsOnlyCodesInTheEmail(t *testing.T) {
	srv, _ := llm(t, func(int) string {
		return choice(`{"content":"{\"summary\":\"Login code\",\"actions\":[` +
			`{\"text\":\"Enter the code\",\"link\":0,\"copy\":\" 483 920 \"},` +
			`{\"text\":\"Use the voucher\",\"link\":0,\"copy\":\"SAVE-50\"},` +
			`{\"text\":\"Track it\",\"link\":0,\"copy\":\"XX\"}],` +
			`\"links\":[],\"category\":\"notification\",\"priority\":\"high\"}"}`)
	})
	a := &Agent{URL: srv.URL, HTTP: srv.Client()}
	s, err := a.Summarize(context.Background(), "Bank", "Your code", "Your code is 483920. Valid 10 minutes.", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{s.Actions[0].Copy, s.Actions[1].Copy, s.Actions[2].Copy}
	if got[0] != "483 920" || got[1] != "" || got[2] != "" {
		t.Fatalf("copy %q: want the code that is in the email (spacing aside), nothing invented or too short", got)
	}
}

func TestReviseDraft(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // the default reply guide, not the user's
	srv, reqs := llm(t, func(int) string {
		return choice(`{"content":"{\"subject\":\"\",\"body\":\"  Hei,\\n\\nKort versjon.\\n\\nMvh\\nDavid  \"}"}`)
	})
	a := &Agent{URL: srv.URL, HTTP: srv.Client()}
	r, err := a.ReviseDraft(context.Background(), "From: shop\n\nBlack Friday!", "Re: Black Friday", "Hei,\n\nLang tekst.\n\nMvh\nDavid", "kortere")
	if err != nil {
		t.Fatal(err)
	}
	if r.Body != "Hei,\n\nKort versjon.\n\nMvh\nDavid" || r.Subject != "Re: Black Friday" {
		t.Fatalf("revision %+v (body trimmed, empty subject keeps the old one)", r)
	}
	msgs := reqs()[0]["messages"].([]any)
	system, user := msgs[0].(map[string]any)["content"].(string), msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(system, "Language: reply in the language") {
		t.Error("reply guide not in the prompt")
	}
	for _, want := range []string{"Black Friday!", "Lang tekst.", "Instruction: kortere"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	srv2, _ := llm(t, func(int) string { return choice(`{"content":"{\"subject\":\"x\",\"body\":\" \"}"}`) })
	a.URL = srv2.URL
	if _, err := a.ReviseDraft(context.Background(), "", "x", "y", "z"); err == nil {
		t.Error("an empty revision must be an error, not a blank draft")
	}
}
