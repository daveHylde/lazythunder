package agent

import (
	"context"
	"fmt"
)

// WebStep is the model's next move on an unsubscribe web page.
type WebStep struct {
	Result  string  `json:"result"` // unsubscribed, submit, follow, needs_browser, failed
	Form    int     `json:"form"`   // submit: form number
	Fields  []Field `json:"fields"` // submit: values to set (hidden fields keep theirs)
	Link    int     `json:"link"`   // follow: link number
	Explain string  `json:"explain"`
}

type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

const unsubscribePrompt = `You unsubscribe the user from a mailing list, without a browser, by reading web pages and submitting forms for them.
You get the page the unsubscribe link led to: its text, forms and links. Choose one result:
- "unsubscribed": the page confirms the user is unsubscribed (or was already).
- "submit": a form completes the unsubscribe. Give its number and the fields to set: fill an email field with the user's address,
  pick options that unsubscribe from all mail, and include the name/value of the button to press if it has a name. Hidden fields keep their values.
- "follow": a link on the page leads to unsubscribing. Give its number.
- "needs_browser": it needs JavaScript, a CAPTCHA or a login.
- "failed": anything else; say why in explain.
Never submit a form that subscribes, buys, logs in or changes anything other than mail preferences. Keep explain under 15 words.`

var webStepSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"result": map[string]any{"type": "string", "enum": []string{"unsubscribed", "submit", "follow", "needs_browser", "failed"}},
		"form":   map[string]any{"type": "integer"},
		"fields": map[string]any{"type": "array", "items": map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}},
			"required":   []string{"name", "value"},
		}},
		"link":    map[string]any{"type": "integer"},
		"explain": map[string]any{"type": "string"},
	},
	"required": []string{"result", "form", "fields", "link", "explain"},
}

// UnsubscribeStep asks what to do on one page (described by page) to
// unsubscribe me.
func (a *Agent) UnsubscribeStep(ctx context.Context, me, page string) (WebStep, error) {
	var s WebStep
	err := a.structured(ctx, "unsubscribe", unsubscribePrompt, fmt.Sprintf("User's address: %s\n\n%s", me, page), webStepSchema, 400, &s)
	return s, err
}
