package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/davidhyldgaard/lazythunder/internal/render"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
	"github.com/davidhyldgaard/lazythunder/internal/unsub"
)

type unsubOptsMsg struct {
	msg     tb.Message
	me      string
	methods []unsub.Method
	err     error
}

// startUnsubscribe looks up how the open message can be unsubscribed from
// (List-Unsubscribe header, then unsubscribe links in the body).
func (m Model) startUnsubscribe(x tb.Message) (tea.Model, tea.Cmd) {
	if m.full == nil || m.full.ID != x.ID {
		m.status = "open the message first"
		return m, nil
	}
	full, c, ctx, me := *m.full, m.c, m.ctx, m.recipientIdentity(x)
	m.status = "looking for an unsubscribe option…"
	return m, func() tea.Msg {
		h, err := c.Headers(ctx, x.ID)
		if err != nil {
			return unsubOptsMsg{msg: x, err: err}
		}
		var links [][2]string
		render.Body(full.Parts.Text, full.Parts.HTML, render.Opts{PreferHTML: true,
			OnLink: func(u, label string) { links = append(links, [2]string{u, label}) }})
		return unsubOptsMsg{msg: x, me: me, methods: unsub.Methods(h, links)}
	}
}

func (m Model) confirmUnsubscribe(msg unsubOptsMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = "unsubscribe: " + msg.err.Error()
		return m, nil
	}
	if len(msg.methods) == 0 {
		m.status = "no unsubscribe option in this message"
		return m, nil
	}
	if msg.me == "" { // nothing to send a mailto request from
		kept := msg.methods[:0]
		for _, x := range msg.methods {
			if x.Kind != unsub.Mailto {
				kept = append(kept, x)
			}
		}
		if msg.methods = kept; len(kept) == 0 {
			m.err = "unsubscribe: only by mail, and no identity to send from"
			return m, nil
		}
	}
	c, ctx, x, ag := m.c, m.ctx, msg.msg, m.ag
	jar, _ := cookiejar.New(nil)
	d := unsub.Deps{
		HTTP: &http.Client{Timeout: 30 * time.Second, Transport: httpClient.Transport, CheckRedirect: httpClient.CheckRedirect, Jar: jar},
		Me:   msg.me,
		Send: func(ctx context.Context, to, subject, body string) error {
			return c.Submit(ctx, tb.Draft{Kind: tb.NewMail, From: msg.me, To: to, Subject: subject, Body: body}, true)
		},
		Step:     ag.UnsubscribeStep,
		Progress: func(s string) { prog.Send(statusMsg(s)) },
	}
	run := func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		res, err := unsub.Do(ctx, d, msg.methods)
		if err != nil {
			return doneMsg{err: fmt.Errorf("unsubscribe failed: %w", err)}
		}
		ids := []int{x.ID}
		if err := c.Delete(ctx, ids); err != nil {
			return doneMsg{err: fmt.Errorf("%s, but delete failed: %w", res, err)}
		}
		return doneMsg{text: res + " · moved to trash", remove: ids}
	}
	m.confirm = &confirmation{text: fmt.Sprintf("Unsubscribe from %s (%s) and trash this message?", name(x.Author), msg.methods[0]), onYes: run}
	m.mode = mConfirm
	return m, nil
}

// recipientIdentity is the account identity the message was sent to, so a
// mailto unsubscribe comes from the subscribed address.
func (m Model) recipientIdentity(x tb.Message) string {
	a := m.accounts[x.Folder.AccountID]
	to := strings.ToLower(strings.Join(append(append([]string{}, x.Recipients...), x.CcList...), " "))
	for _, id := range a.Identities {
		if id.Email != "" && strings.Contains(to, strings.ToLower(id.Email)) {
			return id.Email
		}
	}
	if len(a.Identities) > 0 {
		return a.Identities[0].Email
	}
	return ""
}
