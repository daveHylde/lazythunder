package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/daveHylde/lazythunder/internal/tb"
)

func TestParseServers(t *testing.T) {
	ok := map[string]string{"intype": "IMAP", "inhost": "imap.x.org", "inport": "993", "insecurity": "tls",
		"inauth": "password-cleartext", "inuser": "a@x.org",
		"outhost": "smtp.x.org", "outport": "587", "outsecurity": "starttls", "outauth": "oauth2", "outuser": "a@x.org"}
	in, out, err := parseServers(ok, true)
	if err != nil {
		t.Fatal(err)
	}
	if in != (tb.Server{Type: "imap", Hostname: "imap.x.org", Port: 993, SocketType: "tls", Auth: "password-cleartext", Username: "a@x.org"}) ||
		out != (tb.Server{Type: "smtp", Hostname: "smtp.x.org", Port: 587, SocketType: "starttls", Auth: "oauth2", Username: "a@x.org"}) {
		t.Fatalf("got %+v %+v", in, out)
	}
	for k, bad := range map[string]string{"intype": "exchange", "inhost": "", "inport": "70000", "outport": "0",
		"insecurity": "ssl", "outauth": "password", "inuser": ""} {
		v := map[string]string{}
		for kk, vv := range ok {
			v[kk] = vv
		}
		v[k] = bad
		if _, _, err := parseServers(v, true); err == nil {
			t.Errorf("%s=%q accepted", k, bad)
		}
	}
	// editing: no type field (unchanged), outgoing skipped when the account has none
	edit := map[string]string{"inhost": "h.x.org", "inport": "143", "insecurity": "starttls", "inauth": "none", "inuser": ""}
	if in, _, err := parseServers(edit, false); err != nil || in.Type != "" || in.Port != 143 {
		t.Fatalf("edit: %+v %v", in, err)
	}
}

func TestParseNewAccountAndIdentity(t *testing.T) {
	if _, err := parseNewAccount(map[string]string{"name": "A", "email": "a@x.org"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []map[string]string{{"email": "a@x.org"}, {"name": "A", "email": "a.x.org"}, {"name": "A", "email": "a@x"}} {
		if _, err := parseNewAccount(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
	base := tb.IdentityFull{ID: "id2", SignatureIsHTML: true}
	id, err := parseIdentity(map[string]string{"email": "b@x.org", "name": "B", "signature": "-- B"}, base)
	if err != nil || id.ID != "id2" || !id.SignatureIsHTML || id.Signature != "-- B" {
		t.Fatalf("got %+v %v", id, err)
	}
	if _, err := parseIdentity(map[string]string{"email": "b@x.org", "replyTo": "nope"}, base); err == nil {
		t.Error("bad reply-to accepted")
	}
}

func TestPasswordNotTrimmedNorShown(t *testing.T) {
	f := &acctForm{fields: []formField{field("email", "email", " a@x.org ", ""), secretField("password", "password", "")}}
	f.fields[1].in.SetValue(" s3cret ")
	v := f.values()
	if v["email"] != "a@x.org" || v["password"] != " s3cret " {
		t.Fatalf("got %q", v)
	}
	f.focus(1)
	if strings.Contains(f.fields[1].in.View(), "s3cret") {
		t.Fatal("password shown")
	}
}

func TestViewGoneAfterAccountRemoval(t *testing.T) {
	m := Model{folders: []tb.Folder{{ID: "account1://INBOX"}}, view: listView{folderIDs: []string{"account1://INBOX"}}}
	if m.viewGone() {
		t.Fatal("existing folder reported gone")
	}
	m.view.folderIDs = append(m.view.folderIDs, "account9://INBOX")
	if !m.viewGone() {
		t.Fatal("removed account's folder not reported gone")
	}
	if (Model{}).viewGone() {
		t.Fatal("search results (no folders) reported gone")
	}
}

func TestChangedFieldsSendsOnlyEdits(t *testing.T) {
	orig := tb.Server{Type: "imap", Hostname: "imap.x", Port: 993, SocketType: "starttls-optional", Auth: "secure", Username: "me"}
	edited := orig
	edited.Port = 143
	if got := changedFields(orig, edited); got != (tb.Server{Port: 143}) {
		t.Fatalf("got %+v, want only the port", got)
	}
	if got := changedFields(orig, orig); got != (tb.Server{}) {
		t.Fatalf("unchanged server sends %+v", got)
	}
}

func TestStaleAccountResultsKeepTheNewForm(t *testing.T) {
	old, cur := &acctForm{kind: "add"}, &acctForm{kind: "identity"}
	m := Model{accts: &acctScreen{form: cur, busy: true}}
	mm, _, _ := m.acctUpdate(acctConfigMsg{form: old, err: errors.New("x")})
	m = mm.(Model)
	if m.accts.busy || m.accts.form != cur {
		t.Fatalf("stale lookup: busy %v, form replaced %v", m.accts.busy, m.accts.form != cur)
	}
	m.accts.busy = true
	mm, _, _ = m.acctUpdate(acctDoneMsg{form: old, text: "saved"})
	m = mm.(Model)
	if m.accts.busy || m.accts.form != cur {
		t.Fatalf("stale done: busy %v, form closed %v", m.accts.busy, m.accts.form != cur)
	}
	mm, _, _ = m.acctUpdate(acctDoneMsg{form: cur, text: "saved"})
	if mm.(Model).accts.form != nil {
		t.Fatal("own result did not close the form")
	}
}
