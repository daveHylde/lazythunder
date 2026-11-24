package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/davidhyldgaard/lazythunder/internal/tb"
)

func fn(name, desc string, props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": props, "required": required},
	}}
}

var (
	str   = map[string]any{"type": "string"}
	num   = map[string]any{"type": "integer"}
	boo   = map[string]any{"type": "boolean"}
	idArr = map[string]any{"type": "array", "items": num, "description": "message ids"}
)

var tools = []map[string]any{
	fn("search", "Full-text search across all accounts. Returns message headers.", map[string]any{
		"query":       str,
		"from":        map[string]any{"type": "string", "description": "sender substring"},
		"folder_id":   str,
		"since":       map[string]any{"type": "string", "description": "ISO date or relative: 7d, 2w, today"},
		"unread_only": boo,
		"limit":       num,
	}, "query"),
	fn("list_folders", "List all folders (id, name, unread/total).", map[string]any{}),
	fn("list_messages", "List newest messages in a folder.", map[string]any{
		"folder_id": str, "unread_only": boo, "limit": num,
	}, "folder_id"),
	fn("read_message", "Read one message including its text body.", map[string]any{"id": num}, "id"),
	fn("get_thread", "List headers of all messages in the conversation containing a message.", map[string]any{"id": num}, "id"),
	fn("mark", "Set a flag on messages. Requires user confirmation.", map[string]any{
		"ids":  idArr,
		"flag": map[string]any{"type": "string", "enum": []string{"read", "unread", "flagged", "unflagged", "junk", "not-junk"}},
	}, "ids", "flag"),
	fn("move", "Move messages to a folder. Requires user confirmation.", map[string]any{"ids": idArr, "folder_id": str}, "ids", "folder_id"),
	fn("archive", "Archive messages. Requires user confirmation.", map[string]any{"ids": idArr}, "ids"),
	fn("delete", "Move messages to trash. Requires user confirmation.", map[string]any{"ids": idArr}, "ids"),
	fn("propose_draft", "Hand an email to the user to review, edit, then save or send. kind=reply/reply_all/forward need message_id; recipients and subject of replies are filled in by Thunderbird.", map[string]any{
		"kind":       map[string]any{"type": "string", "enum": []string{"new", "reply", "reply_all", "forward"}},
		"message_id": num,
		"to":         str, "cc": str, "subject": str,
		"from": map[string]any{"type": "string", "description": "identity email to send from (new emails)"},
		"body": str,
	}, "kind", "body"),
}

type args struct {
	Query      string `json:"query"`
	From       string `json:"from"`
	FolderID   string `json:"folder_id"`
	Since      string `json:"since"`
	UnreadOnly bool   `json:"unread_only"`
	Limit      int    `json:"limit"`
	ID         int    `json:"id"`
	IDs        []int  `json:"ids"`
	Flag       string `json:"flag"`
	Kind       string `json:"kind"`
	MessageID  int    `json:"message_id"`
	To         string `json:"to"`
	Cc         string `json:"cc"`
	Subject    string `json:"subject"`
	Body       string `json:"body"`
}

// call executes a tool and returns text for the model; errors become text too.
func (a *Agent) call(ctx context.Context, name, raw string, h Hooks) string {
	var p args
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "error: bad arguments: " + err.Error()
	}
	out, err := a.dispatch(ctx, name, p, h)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

func (a *Agent) dispatch(ctx context.Context, name string, p args, h Hooks) (string, error) {
	c := a.TB
	confirm := func(desc string) error {
		if !h.Confirm(desc) {
			return fmt.Errorf("NOT DONE. The user declined to %s. Tell the user it was not done", desc)
		}
		return nil
	}
	limit := p.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	switch name {
	case "mark", "move", "archive", "delete":
		if len(p.IDs) == 0 {
			return "", fmt.Errorf("ids is empty")
		}
	}
	switch name {
	case "search":
		pg, err := c.Search(ctx, p.Query, tb.SearchOpts{From: p.From, Folder: p.FolderID, Since: p.Since, Unread: p.UnreadOnly, Limit: limit})
		return FormatList(pg.Messages), err
	case "list_folders":
		fs, err := c.Folders(ctx)
		var b strings.Builder
		for _, f := range fs {
			if f.Depth > 0 {
				fmt.Fprintf(&b, "%s | %s | %d unread / %d\n", f.ID, f.Name, f.Unread, f.Total)
			}
		}
		return b.String(), err
	case "list_messages":
		ms, _, _, err := c.Recent(ctx, p.FolderID, time.Time{}, p.UnreadOnly)
		return FormatList(ms[:min(limit, len(ms))]), err
	case "read_message":
		m, err := c.Read(ctx, p.ID)
		if err != nil {
			return "", err
		}
		return FormatFull(m), nil
	case "get_thread":
		t, err := c.Thread(ctx, p.ID)
		return FormatList(t), err
	case "mark":
		switch p.Flag {
		case "read", "unread", "flagged", "unflagged", "junk", "not-junk":
		default:
			return "", fmt.Errorf("invalid flag %q", p.Flag)
		}
		if err := confirm(fmt.Sprintf("mark %v as %s", p.IDs, p.Flag)); err != nil {
			return "", err
		}
		return "done", c.Mark(ctx, p.IDs, p.Flag)
	case "move":
		if err := confirm(fmt.Sprintf("move %v to %s", p.IDs, p.FolderID)); err != nil {
			return "", err
		}
		return "done", c.Move(ctx, p.IDs, p.FolderID)
	case "archive":
		if err := confirm(fmt.Sprintf("archive %v", p.IDs)); err != nil {
			return "", err
		}
		return "done", c.Archive(ctx, p.IDs)
	case "delete":
		if err := confirm(fmt.Sprintf("move %v to trash", p.IDs)); err != nil {
			return "", err
		}
		return "done", c.Delete(ctx, p.IDs)
	case "propose_draft":
		kinds := map[string]tb.DraftKind{"new": tb.NewMail, "reply": tb.Reply, "reply_all": tb.ReplyAll, "forward": tb.Forward}
		k, ok := kinds[p.Kind]
		if !ok {
			return "", fmt.Errorf("unknown kind %q", p.Kind)
		}
		if k != tb.NewMail && p.MessageID == 0 {
			return "", fmt.Errorf("message_id is required for %s", p.Kind)
		}
		h.Propose(tb.Draft{Kind: k, ReplyTo: p.MessageID, From: p.From, To: p.To, Cc: p.Cc, Subject: p.Subject, Body: p.Body})
		return "draft handed to the user for review; do not propose it again", nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// FormatList renders message headers one per line, compact for the model.
func FormatList(ms []tb.Message) string {
	if len(ms) == 0 {
		return "no messages"
	}
	var b strings.Builder
	for _, m := range ms {
		state := ""
		if !m.Read {
			state += " unread"
		}
		if m.Flagged {
			state += " flagged"
		}
		fmt.Fprintf(&b, "id %d | %s | %s | %s | %s%s\n", m.ID, m.Date.Local().Format("2006-01-02 15:04"), m.Author, m.Subject, m.Folder.ID(), state)
	}
	return b.String()
}

// FormatFull renders a message with headers and plain-text body.
func FormatFull(m tb.FullMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id %d\nFrom: %s\nTo: %s\n", m.ID, m.Author, strings.Join(m.Recipients, ", "))
	if len(m.CcList) > 0 {
		fmt.Fprintf(&b, "Cc: %s\n", strings.Join(m.CcList, ", "))
	}
	fmt.Fprintf(&b, "Date: %s\nSubject: %s\nFolder: %s\n", m.Date.Local().Format("2006-01-02 15:04"), m.Subject, m.Folder.ID())
	for _, at := range m.Parts.Attachments {
		fmt.Fprintf(&b, "Attachment: %s (%s, %d bytes)\n", at.Name, at.ContentType, at.Size)
	}
	b.WriteString("\n" + m.PlainText())
	return b.String()
}
