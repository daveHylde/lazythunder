package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/daveHylde/lazythunder/internal/tb"
)

// Revision is a new version of a draft, with a short note on what changed.
type Revision struct {
	Subject string `json:"subject"`
	To      string `json:"to"`
	Cc      string `json:"cc"`
	Body    string `json:"body"`
	Note    string `json:"note"`
}

const revisePrompt = `You write and rewrite an email draft for the user, following their instruction.
If the draft is empty, write the email from the instruction: it is the gist of what the user wants to say.
Otherwise carry the instruction out fully: if it asks for shorter, cut words; for more polite, soften the wording; for another language, translate; for different content, change what is said.
Keep what the instruction does not touch: the facts, numbers, names, [placeholders], greeting and sign-off style.
The guide's template shows the shape only: never output its <angle-bracket> markers; use the names already in the draft.
Do not invent facts, dates, prices or promises; leave a clear [placeholder] for anything missing.
Return the whole new body as plain text, without quoted text from the original email. Keep the subject unless the instruction is about it.
Keep to and cc unless the instruction changes the recipients.
note: what you did in at most 10 words, e.g. "draft written" or "translated to English, cut 2 sentences".
The user's reply guide, for tone and form:
`

var reviseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{"subject": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"},
		"cc": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}, "note": map[string]any{"type": "string"}},
	"required": []string{"subject", "to", "cc", "body", "note"},
}

// ReviseDraft writes (empty body) or rewrites draft d following instruction.
// original is the email it replies to or forwards ("" for new mail): context
// only, never quoted. history holds this draft's earlier instructions, oldest
// first. Stateless: everything the model needs is in this one request.
// Replies keep their recipients and subject (Thunderbird sets them); forwards
// may change only To; new mail may change To, Cc and Subject.
func (a *Agent) ReviseDraft(ctx context.Context, d tb.Draft, original string, history []string, instruction string) (Revision, error) {
	if len(original) > 6000 {
		original = strings.ToValidUTF8(original[:6000], "") + "\n…"
	}
	if len(history) > 10 {
		history = history[len(history)-10:]
	}
	var user strings.Builder
	if original != "" {
		fmt.Fprintf(&user, "The email the draft %s:\n<<<\n%s\n>>>\n\n", map[bool]string{true: "forwards", false: "replies to"}[d.Kind == tb.Forward], original)
	}
	if len(history) > 0 {
		user.WriteString("Earlier instructions for this draft, already carried out in the draft below (oldest first):\n")
		for _, h := range history {
			fmt.Fprintf(&user, "- %s\n", clip(h, 200))
		}
		user.WriteString("\n")
	}
	switch d.Kind {
	case tb.Reply, tb.ReplyAll:
		user.WriteString("Recipients and subject: set by the reply, return them unchanged (empty).\n")
	case tb.Forward:
		fmt.Fprintf(&user, "Forward to: %s\n", d.To)
	default:
		fmt.Fprintf(&user, "To: %s\nCc: %s\nSubject: %s\n", d.To, d.Cc, d.Subject)
	}
	body := strings.TrimSpace(d.Body)
	if body == "" {
		fmt.Fprintf(&user, "Draft body: (empty, nothing written yet)\n\nWrite the email. What the user wants to say: %s", instruction)
	} else {
		fmt.Fprintf(&user, "Instruction: %s\n\nDraft body:\n<<<\n%s\n>>>\n\nRewrite the draft so it follows the instruction: %s", instruction, body, instruction)
	}
	var r Revision
	err := a.structured(ctx, "draft", revisePrompt+ReplyGuide(), user.String(), reviseSchema, 500+len(body), &r)
	r.Subject, r.To, r.Cc = noThoughts(r.Subject), noThoughts(r.To), noThoughts(r.Cc)
	// Seen live: the model writes "\n\fredag" for a new paragraph, and JSON reads "\f" as a
	// form feed. Form feeds and backspaces never belong in a mail: give the letter back,
	// capitalized as it starts a paragraph.
	r.Body = strings.NewReplacer("\f", "\nF", "\b", "\nB").Replace(r.Body)
	r.Body, r.Note = noThoughts(r.Body), noThoughts(r.Note)
	if err == nil && r.Body == "" {
		err = fmt.Errorf("the model returned an empty draft")
	}
	switch d.Kind {
	case tb.Reply, tb.ReplyAll:
		r.To, r.Cc, r.Subject = d.To, d.Cc, d.Subject
	case tb.Forward:
		r.Cc, r.Subject = d.Cc, d.Subject // Submit sends only To for forwards
	}
	if r.To == "" {
		r.To = d.To
	}
	if r.Subject == "" {
		r.Subject = d.Subject
	}
	if r.Note == "" {
		r.Note = "revised"
	}
	if len(r.Note) > 120 {
		r.Note = strings.ToValidUTF8(r.Note[:120], "") + "…"
	}
	return r, err
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
