package ui

import (
	"fmt"
	"strings"

	"github.com/daveHylde/lazythunder/internal/tb"
)

const separator = "--- write below this line ---"

// renderDraft turns a draft into an editable file. ctx is shown as comments
// (e.g. the message being replied to). Thunderbird adds the quote on replies.
func renderDraft(d tb.Draft, ctx string) string {
	var b strings.Builder
	b.WriteString("# lazythunder draft. Lines starting with # above the separator are ignored.\n")
	for _, l := range strings.Split(strings.TrimRight(ctx, "\n"), "\n") {
		if l != "" {
			b.WriteString("# " + l + "\n")
		}
	}
	switch d.Kind {
	case tb.NewMail:
		fmt.Fprintf(&b, "From: %s\nTo: %s\nCc: %s\nSubject: %s\n", d.From, d.To, d.Cc, d.Subject)
	case tb.Forward:
		fmt.Fprintf(&b, "To: %s\n", d.To)
	default:
		b.WriteString("# Recipients and subject are set by Thunderbird; the original is quoted below your text.\n")
	}
	b.WriteString(separator + "\n" + d.Body)
	if d.Body != "" && !strings.HasSuffix(d.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// parseDraft reads an edited file back into d (keeping Kind and ReplyTo).
func parseDraft(d tb.Draft, s string) (tb.Draft, error) {
	head, body, ok := strings.Cut(s, separator+"\n")
	if !ok {
		head, body, ok = strings.Cut(s, separator)
	}
	if !ok {
		return d, fmt.Errorf("separator line %q is missing", separator)
	}
	for _, l := range strings.Split(head, "\n") {
		if strings.HasPrefix(l, "#") {
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "from":
			d.From = v
		case "to":
			d.To = v
		case "cc":
			d.Cc = v
		case "subject":
			d.Subject = v
		}
	}
	d.Body = strings.TrimRight(body, "\n") + "\n"
	return d, validDraft(d)
}

// validDraft is checked after editing and again before saving or sending.
func validDraft(d tb.Draft) error {
	if strings.TrimSpace(d.Body) == "" {
		return fmt.Errorf("body is empty")
	}
	if (d.Kind == tb.NewMail || d.Kind == tb.Forward) && strings.TrimSpace(d.To) == "" {
		return fmt.Errorf("To: is empty")
	}
	return nil
}

func kindName(k tb.DraftKind) string {
	return [...]string{"New message", "Reply", "Reply all", "Forward"}[k]
}
