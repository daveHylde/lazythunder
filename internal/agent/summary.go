package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Summary is the sidebar's one-line gist of an email, what to do about it,
// a short title for each numbered link, and how the list should tag it.
type Summary struct {
	Version  int        `json:"v,omitempty"` // SummaryVersion that wrote it; older ones are redone when viewed
	Summary  string     `json:"summary"`
	Actions  []Action   `json:"actions"`
	Links    []LinkInfo `json:"links"`
	Category string     `json:"category"` // one of Categories
	Priority string     `json:"priority"` // high, normal or low
}

// Categories are the kinds of mail Summarize sorts into.
var Categories = []string{"reply-needed", "action", "fyi", "newsletter", "receipt", "notification", "spam"}

// Action is a to-do, optionally done through link number Link (0 = none)
// or by pasting Copy, a code from the email ("" = none).
type Action struct {
	Text string `json:"text"`
	Link int    `json:"link"`
	Copy string `json:"copy,omitempty"`
}

// LinkInfo titles link number N; Suspicious hides it from the UI.
type LinkInfo struct {
	N          int    `json:"n"`
	Title      string `json:"title"`
	Suspicious bool   `json:"suspicious"`
}

const summaryPrompt = `Summarize the email for a busy reader in at most 12 words, in the email's language.
Then list up to 3 concrete action items for the recipient, each under 10 words; an empty list if nothing needs doing.
Newsletters, receipts and notifications usually need no action.
Links in the email are marked [n] and listed under "Links". If an action is done by opening one of them, set its "link" to that n, else 0.
If the email has a code the reader will need to type or paste somewhere (a one-time or verification code, a login or confirmation code, a booking or order reference, a discount or voucher code, a tracking number, a PIN), add an action for using it (e.g. "Enter the code to log in") with "copy" set to the code exactly as written in the email; else "copy" is "".
For every listed link, give a title of at most 6 words that says what opening it does for the reader: start with a verb and name the site or thing it leads to (e.g. "Go to your Instagram account", "Open Jollyroom's website", "Answer the survey", "Pay the invoice", "Track your order"), in the email's language. Never just repeat the site's name.
Set "suspicious" when a link looks like phishing: it asks for a login or payment on a site that does not fit the sender, or the text and target disagree.
Set "category" to one of: "reply-needed" (the sender expects an answer from the recipient), "action" (something to do, but no reply), "fyi" (worth reading, nothing to do), "newsletter", "receipt" (orders, invoices, bookings, payments), "notification" (automated alerts and updates), "spam".
Set "priority" to "high" when it is urgent or has a near deadline, "low" for newsletters, notifications and spam, otherwise "normal".`

var summarySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"summary": map[string]any{"type": "string"},
		"actions": map[string]any{"type": "array", "maxItems": 3, "items": map[string]any{
			"type": "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}, "link": map[string]any{"type": "integer"},
				"copy": map[string]any{"type": "string"}},
			"required": []string{"text", "link", "copy"},
		}},
		"links": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{"n": map[string]any{"type": "integer"}, "title": map[string]any{"type": "string"},
				"suspicious": map[string]any{"type": "boolean"}},
			"required": []string{"n", "title", "suspicious"},
		}},
		"category": map[string]any{"type": "string", "enum": Categories},
		"priority": map[string]any{"type": "string", "enum": []string{"high", "normal", "low"}},
	},
	"required": []string{"summary", "actions", "links", "category", "priority"},
}

// SummaryVersion is bumped when the prompt changes enough that stored notes
// should be redone (2: verb-first, descriptive link titles; 3: copyable codes).
const SummaryVersion = 3

// MaxLinks is how many numbered links Summarize titles.
const MaxLinks = 25

// Summarize asks the model for a Summary of one email (stateless: no chat
// history). links are the body's numbered links, links[i] being [i+1].
func (a *Agent) Summarize(ctx context.Context, from, subject, body string, links []string) (Summary, error) {
	full := body
	if len(body) > 6000 { // ~1500 tokens: enough for the gist, keeps it to seconds
		body = strings.ToValidUTF8(body[:6000], "") + "\n…"
	}
	if len(links) > 0 {
		body += "\n\nLinks:\n"
		for i, l := range links[:min(len(links), MaxLinks)] {
			if len(l) > 200 {
				l = strings.ToValidUTF8(l[:200], "") + "…"
			}
			body += fmt.Sprintf("[%d] %s\n", i+1, l)
		}
	}
	var sum Summary
	err := a.structured(ctx, "summary", summaryPrompt, fmt.Sprintf("From: %s\nSubject: %s\n\n%s", from, subject, body),
		summarySchema, 320+25*min(len(links), MaxLinks), &sum)
	sum.Summary = strings.TrimSpace(sum.Summary)
	sum.Version = SummaryVersion
	for i := range sum.Actions {
		sum.Actions[i].Copy = verbatim(sum.Actions[i].Copy, subject+"\n"+full)
	}
	return sum, err
}

// verbatim returns code when the email really contains it (spacing aside),
// else "": a model can invent or misread a code, and a wrong one is worse than none.
func verbatim(code, text string) string {
	code = strings.TrimSpace(code)
	squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
	if len([]rune(code)) < 3 || len(code) > 64 || !strings.Contains(squash(text), squash(code)) {
		return ""
	}
	return code
}

// structured runs one stateless completion whose reply must match schema,
// and decodes it into out.
func (a *Agent) structured(ctx context.Context, name, system, user string, schema map[string]any, maxTokens int, out any) error {
	m, err := a.post(ctx, map[string]any{
		"messages":        []Msg{{Role: "system", Content: system}, {Role: "user", Content: user}},
		"temperature":     0.2,
		"max_tokens":      maxTokens,
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": name, "schema": schema}},
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(m.Content), out); err != nil {
		return fmt.Errorf("%s is not JSON: %w", name, err)
	}
	return nil
}
