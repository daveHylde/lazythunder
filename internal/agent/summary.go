package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Summary is the sidebar's one-line gist of an email, what to do about it,
// and a short title for each numbered link.
type Summary struct {
	Summary string     `json:"summary"`
	Actions []Action   `json:"actions"`
	Links   []LinkInfo `json:"links"`
}

// Action is a to-do, optionally done through link number Link (0 = none).
type Action struct {
	Text string `json:"text"`
	Link int    `json:"link"`
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
For every listed link, give a title of at most 5 words saying what it opens (e.g. "Order status", "Pay invoice", "Read full article"), in the email's language.
Set "suspicious" when a link looks like phishing: it asks for a login or payment on a site that does not fit the sender, or the text and target disagree.`

var summarySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"summary": map[string]any{"type": "string"},
		"actions": map[string]any{"type": "array", "maxItems": 3, "items": map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}, "link": map[string]any{"type": "integer"}},
			"required":   []string{"text", "link"},
		}},
		"links": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{"n": map[string]any{"type": "integer"}, "title": map[string]any{"type": "string"},
				"suspicious": map[string]any{"type": "boolean"}},
			"required": []string{"n", "title", "suspicious"},
		}},
	},
	"required": []string{"summary", "actions", "links"},
}

// MaxLinks is how many numbered links Summarize titles.
const MaxLinks = 25

// Summarize asks the model for a Summary of one email (stateless: no chat
// history). links are the body's numbered links, links[i] being [i+1].
func (a *Agent) Summarize(ctx context.Context, from, subject, body string, links []string) (Summary, error) {
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
		summarySchema, 300+25*min(len(links), MaxLinks), &sum)
	sum.Summary = strings.TrimSpace(sum.Summary)
	return sum, err
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
