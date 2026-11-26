// Package agent is a small tool-calling mail assistant backed by a local
// llama.cpp server (OpenAI-compatible /v1/chat/completions).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/daveHylde/lazythunder/internal/tb"
)

type Msg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Hooks connect the agent to the UI. Confirm and Propose may block.
type Hooks struct {
	Confirm func(action string) bool // ask the user before a mutating action
	Propose func(tb.Draft)           // hand a draft to the user's compose flow
	Status  func(string)             // progress state, e.g. "thinking…"
	Step    func(string)             // optional: a round of tool calls, e.g. "reading 3 messages"
	Found   func([]tb.Message)       // optional: messages a tool returned (search, list, thread, read)
}

// Agent talks to any OpenAI-compatible chat completions endpoint.
type Agent struct {
	URL, Model string // Model "" lets the server pick (llama.cpp serves its loaded model)
	APIKey     string // sent as a Bearer token when set
	TB         *tb.Client
	History    []Msg
	HTTP       *http.Client
	accounts   string // identity list for the system prompt, loaded once
}

func New(c *tb.Client) *Agent {
	url := strings.TrimRight(os.Getenv("LAZYTHUNDER_LLM_URL"), "/")
	if url == "" {
		url = "http://127.0.0.1:8085/v1"
	}
	return &Agent{URL: url, Model: os.Getenv("LAZYTHUNDER_MODEL"), APIKey: os.Getenv("LAZYTHUNDER_API_KEY"),
		TB: c, HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

// Enabled is false when AI is switched off (LAZYTHUNDER_LLM_URL=off).
func (a *Agent) Enabled() bool { return a != nil && a.URL != "off" }

// Name is the assistant's display name.
func (a *Agent) Name() string {
	if a != nil && a.Model != "" {
		return a.Model
	}
	return "assistant"
}

// post sends one chat completion request and returns the reply message.
func (a *Agent) post(ctx context.Context, req map[string]any) (Msg, error) {
	if !a.Enabled() {
		return Msg{}, fmt.Errorf("AI is off (LAZYTHUNDER_LLM_URL=off)")
	}
	if a.Model != "" {
		req["model"] = a.Model
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, "POST", a.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Msg{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if a.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+a.APIKey)
	}
	resp, err := a.HTTP.Do(hr)
	if err != nil {
		return Msg{}, fmt.Errorf("AI at %s: %w", a.URL, err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message Msg `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Msg{}, fmt.Errorf("AI: %s: %w", resp.Status, err)
	}
	if out.Error != nil {
		return Msg{}, fmt.Errorf("AI: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return Msg{}, fmt.Errorf("AI: empty response (%s)", resp.Status)
	}
	return out.Choices[0].Message, nil
}

func (a *Agent) Reset() { a.History = nil }

const systemPrompt = `You are the mail assistant inside lazythunder, a terminal client for the user's Thunderbird.
Today is %s. The user's accounts and identities:
%s
Current view: %s

Use tools to look things up; never invent message contents or ids. Message ids are integers.
Mutating tools (mark, move, archive, delete) ask the user for confirmation; if declined, accept it.
To write an email or reply, call propose_draft: the user reviews and edits it before anything is saved or sent.
Before writing a reply, read the message (and its thread if it is part of one) so the reply fits.
Follow the reply guide below exactly when writing any email.
Answer briefly and plainly; plain text, no markdown tables.

Reply guide:
%s`

// Ask runs one user turn to completion and returns the assistant's final text.
// viewCtx describes what the user is looking at (folder, selected message).
func (a *Agent) Ask(ctx context.Context, user, viewCtx string, h Hooks) (string, error) {
	if a.accounts == "" {
		accts, err := a.TB.Accounts(ctx)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, ac := range accts {
			for _, id := range ac.Identities {
				fmt.Fprintf(&b, "- account %s: %s <%s> (identity %s)\n", ac.ID, id.Name, id.Email, id.ID)
			}
		}
		a.accounts = b.String()
	}
	// The system prompt is rebuilt each turn so the view context stays current.
	sys := Msg{Role: "system", Content: fmt.Sprintf(systemPrompt, time.Now().Format("Monday 2006-01-02 15:04"), a.accounts, viewCtx, ReplyGuide())}
	if len(a.History) == 0 {
		a.History = []Msg{sys}
	}
	a.History[0] = sys
	a.History = append(a.History, Msg{Role: "user", Content: user})
	a.trim()

	for step := 0; step < 10; step++ {
		h.Status("thinking…")
		reply, err := a.complete(ctx)
		if err != nil {
			return "", err
		}
		reply.Content = noThoughts(reply.Content) // leaked reasoning stays out of the answer and the history
		a.History = append(a.History, reply)
		if len(reply.ToolCalls) == 0 {
			return reply.Content, nil
		}
		if h.Step != nil {
			for _, s := range steps(reply.ToolCalls) {
				h.Step(s)
			}
		}
		for _, tc := range reply.ToolCalls {
			out := a.call(ctx, tc.Function.Name, tc.Function.Arguments, h)
			a.History = append(a.History, Msg{Role: "tool", ToolCallID: tc.ID, Content: clip(out, 8000)})
		}
	}
	return "", fmt.Errorf("agent gave up after 10 tool rounds")
}

// steps describes a round of tool calls in a few words each, one line per
// tool, e.g. `searching "invoice"…` or "reading 3 messages".
func steps(calls []ToolCall) []string {
	var order []string
	n, query := map[string]int{}, ""
	for _, tc := range calls {
		name := tc.Function.Name
		if n[name] == 0 {
			order = append(order, name)
		}
		n[name]++
		if name == "search" && query == "" {
			var p args
			json.Unmarshal([]byte(tc.Function.Arguments), &p)
			query = p.Query
		}
	}
	plural := func(k int, one, many string) string {
		if k == 1 {
			return one
		}
		return fmt.Sprintf(many, k)
	}
	var out []string
	for _, name := range order {
		k := n[name]
		switch name {
		case "search":
			one := "searching…"
			if query != "" {
				one = fmt.Sprintf("searching %q…", query)
			}
			out = append(out, plural(k, one, "running %d searches…"))
		case "read_message":
			out = append(out, plural(k, "reading a message", "reading %d messages"))
		case "get_thread":
			out = append(out, plural(k, "reading the thread", "reading %d threads"))
		case "list_messages":
			out = append(out, plural(k, "listing a folder", "listing %d folders"))
		case "list_folders":
			out = append(out, "looking at your folders")
		case "propose_draft":
			out = append(out, "writing a draft")
		default: // mutating tools: the y/n question says what
			out = append(out, name+"…")
		}
	}
	return out
}

// trim drops the oldest turns so the prompt stays well under the 32k context.
// shortcut: char count approximates tokens; use /tokenize if contexts get tight.
func (a *Agent) trim() {
	size := func() (n int) {
		for _, m := range a.History {
			n += len(m.Content)
		}
		return
	}
	for size() > 60000 && len(a.History) > 2 {
		// drop the oldest message after the system prompt, then any orphaned tool results
		a.History = append(a.History[:1], a.History[2:]...)
		for len(a.History) > 1 && a.History[1].Role == "tool" {
			a.History = append(a.History[:1], a.History[2:]...)
		}
	}
}

func (a *Agent) complete(ctx context.Context) (Msg, error) {
	m, err := a.post(ctx, map[string]any{"messages": a.History, "tools": tools, "temperature": 0.4})
	m.Role = "assistant"
	return m, err
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "\n…[truncated]"
}

// noThoughts drops model reasoning that leaked into a field, e.g.
// "<think>…</think>", "<|channel|>analysis…<|message|>" or "<|channel>thought…<channel|>".
func noThoughts(s string) string {
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	if i := strings.LastIndex(s, "|>"); i >= 0 {
		s = s[i+2:]
	} else if t := strings.TrimSpace(s); strings.HasPrefix(t, "<|") || strings.HasPrefix(t, "<think>") {
		return "" // reasoning that never ended: nothing usable
	}
	return strings.TrimSpace(s)
}
