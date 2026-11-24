package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Summary is the sidebar's one-line gist of an email plus what to do about it.
type Summary struct {
	Summary string   `json:"summary"`
	Actions []string `json:"actions"`
}

const summaryPrompt = `Summarize the email for a busy reader in at most 12 words, in the email's language (Norwegian or English).
Then list up to 3 concrete action items for the recipient, each under 10 words; an empty list if nothing needs doing.
Newsletters, receipts and notifications usually need no action.`

var summarySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"summary": map[string]any{"type": "string"},
		"actions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 3},
	},
	"required": []string{"summary", "actions"},
}

// Summarize asks the model for a Summary of one email (stateless: no chat history).
func (a *Agent) Summarize(ctx context.Context, from, subject, body string) (Summary, error) {
	if len(body) > 6000 { // ~1500 tokens: enough for the gist, keeps it to seconds
		body = strings.ToValidUTF8(body[:6000], "") + "\n…"
	}
	req, _ := json.Marshal(map[string]any{
		"model": a.Model,
		"messages": []Msg{
			{Role: "system", Content: summaryPrompt},
			{Role: "user", Content: fmt.Sprintf("From: %s\nSubject: %s\n\n%s", from, subject, body)},
		},
		"temperature":     0.2,
		"max_tokens":      200,
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "summary", "schema": summarySchema}},
	})
	hr, err := http.NewRequestWithContext(ctx, "POST", a.URL+"/chat/completions", bytes.NewReader(req))
	if err != nil {
		return Summary{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(hr)
	if err != nil {
		return Summary{}, fmt.Errorf("llama.cpp at %s: %w", a.URL, err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message Msg `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
		return Summary{}, fmt.Errorf("llama.cpp: bad response (%s)", resp.Status)
	}
	var s Summary
	if err := json.Unmarshal([]byte(out.Choices[0].Message.Content), &s); err != nil {
		return Summary{}, fmt.Errorf("summary is not JSON: %w", err)
	}
	s.Summary = strings.TrimSpace(s.Summary)
	return s, nil
}
