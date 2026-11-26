package bg

import (
	"regexp"
	"strings"
	"time"

	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// RePrefix matches reply/forward prefixes ("Re: SV: Fwd[2]:").
var RePrefix = regexp.MustCompile(`(?i)^\s*((re|sv|aw|antw|fw|fwd|vs|wg|tr|rv)(\[\d+\])?\s*:\s*)+`)

// SplitSubject strips reply/forward prefixes. kind is 'r', 'f' or 0 by the first prefix.
func SplitSubject(s string) (kind byte, norm string) {
	p := RePrefix.FindString(s)
	norm = strings.ToLower(strings.TrimSpace(s[len(p):]))
	switch first := strings.ToLower(strings.TrimSpace(p)); {
	case first == "":
	case strings.HasPrefix(first, "re"), strings.HasPrefix(first, "sv"), strings.HasPrefix(first, "aw"), strings.HasPrefix(first, "antw"):
		kind = 'r'
	default:
		kind = 'f'
	}
	return kind, norm
}

// SearchSubject is the subject as tb's case-sensitive subject filter should
// see it: prefixes stripped, original spelling kept. "" when too short to search
// (tiny subjects would match everything).
func SearchSubject(s string) string {
	if raw := strings.TrimSpace(s[len(RePrefix.FindString(s)):]); len(raw) >= 3 {
		return raw
	}
	return ""
}

// addrOf extracts the lower-cased email address from "Name <a@b>".
func addrOf(a string) string {
	if i, j := strings.LastIndex(a, "<"), strings.LastIndex(a, ">"); i >= 0 && j > i {
		a = a[i+1 : j]
	}
	return strings.ToLower(strings.TrimSpace(a))
}

// MatchReplies marks msgs answered by a sent message: a later message with the
// same subject (Re: or no prefix: some clients drop it) sent to the message's
// author is a reply (wins); a later Fwd: of it is a forward. shortcut: subject matching, so generic subjects can misfire;
// exact matching would need each message's References via `tb thread`.
func MatchReplies(msgs, sent []tb.Message) map[int]store.Reply {
	type cand struct {
		kind byte
		m    tb.Message
	}
	bySubj := map[string][]cand{}
	for _, s := range sent {
		k, n := SplitSubject(s.Subject)
		if k == 0 {
			k = 'r'
		}
		if n != "" {
			bySubj[n] = append(bySubj[n], cand{k, s})
		}
	}
	out := map[int]store.Reply{}
	for _, m := range msgs {
		_, n := SplitSubject(m.Subject)
		from := addrOf(m.Author)
		for _, c := range bySubj[n] {
			if !c.m.Date.After(m.Date) || c.m.ID == m.ID {
				continue
			}
			// Thunderbird drops a leading "Re:" from subjects, so a reply to
			// "VS: X" reads "VS: X" too: the same subject as the message is a
			// reply, only an added prefix makes a forward.
			if sameSubject(c.m.Subject, m.Subject) {
				c.kind = 'r'
			}
			to := ""
			if len(c.m.Recipients) > 0 {
				to = c.m.Recipients[0]
			}
			if c.kind == 'r' {
				hit := false
				for _, r := range append(c.m.Recipients, c.m.CcList...) {
					hit = hit || addrOf(r) == from
				}
				if !hit {
					continue
				}
			}
			cur, seen := out[m.ID]
			// the first answer after the message is the one that replied to it
			if !seen || better(c.kind, c.m.Date, cur) {
				out[m.ID] = store.Reply{Kind: c.kind, SentID: c.m.ID, Date: c.m.Date, To: to}
			}
		}
	}
	return out
}

func sameSubject(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// better: a reply beats a forward; between equals the earlier one wins.
func better(kind byte, date time.Time, cur store.Reply) bool {
	return kind == 'r' && cur.Kind == 'f' || kind == cur.Kind && date.Before(cur.Date)
}
