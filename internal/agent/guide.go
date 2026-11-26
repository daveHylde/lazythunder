package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultReplyGuide is written to ~/.config/lazythunder/reply-guide.md on
// first use; the user edits that file to change how replies are written.
const defaultReplyGuide = `# How the assistant writes replies
# (lines starting with # are ignored; edit freely)

Language: reply in the language of the email.
Tone: polite, friendly and to the point. No fluff, no filler, no repeating the question back.
Length: as short as the content allows; usually 2-5 sentences. One short paragraph per point.
Do not invent facts, dates, prices or promises the user did not give; if something is missing,
leave a clear [placeholder].
Never add a subject line, quoted text or signature blocks beyond the template below.

Template (greeting and sign-off in the reply's language):
Hi <first name>,

<reply>

Best regards,
<my first name, from the identity the reply is sent from>
`

// ReplyGuide returns the user's reply guide, creating the default file if missing.
func ReplyGuide() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return stripComments(defaultReplyGuide)
	}
	path := filepath.Join(dir, "lazythunder", "reply-guide.md")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
			os.WriteFile(path, []byte(defaultReplyGuide), 0o644)
		}
		return stripComments(defaultReplyGuide)
	}
	if err != nil {
		return stripComments(defaultReplyGuide)
	}
	return stripComments(string(b))
}

func stripComments(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
