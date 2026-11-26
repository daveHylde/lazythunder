package agent

import (
	"context"
	"fmt"
	"strings"
)

// Revision is a rewritten draft.
type Revision struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

const revisePrompt = `You rewrite an email draft for the user, following their instruction.
Carry the instruction out fully: if it asks for shorter, cut words; for more polite, soften the wording; for another language, translate; for different content, change what is said.
Keep what the instruction does not touch: the facts, numbers, names, [placeholders], greeting and sign-off style.
The guide's template shows the shape only: never output its <angle-bracket> markers; use the names already in the draft.
Do not invent facts, dates, prices or promises; leave a clear [placeholder] for anything missing.
Return the whole new body as plain text, without quoted text from the original email. Keep the subject unless the instruction is about it.
The user's reply guide, for tone and form:
`

var reviseSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{"subject": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}},
	"required":   []string{"subject", "body"},
}

// ReviseDraft rewrites a draft (subject, body) following instruction.
// original is the email it answers or forwards ("" for new mail): context
// only, never quoted. Stateless: the chat history is not involved.
func (a *Agent) ReviseDraft(ctx context.Context, original, subject, body, instruction string) (Revision, error) {
	if len(original) > 6000 {
		original = strings.ToValidUTF8(original[:6000], "") + "\n…"
	}
	var user strings.Builder
	if original != "" {
		fmt.Fprintf(&user, "The email the draft answers:\n<<<\n%s\n>>>\n\n", original)
	}
	fmt.Fprintf(&user, "Instruction: %s\n\nDraft subject: %s\nDraft body:\n<<<\n%s\n>>>\n\nRewrite the draft so it follows the instruction: %s", instruction, subject, body, instruction)
	var r Revision
	err := a.structured(ctx, "draft", revisePrompt+ReplyGuide(), user.String(), reviseSchema, 300+len(body), &r)
	r.Subject, r.Body = strings.TrimSpace(r.Subject), strings.TrimSpace(r.Body)
	if err == nil && r.Body == "" {
		err = fmt.Errorf("the model returned an empty draft")
	}
	if r.Subject == "" {
		r.Subject = subject
	}
	return r, err
}
