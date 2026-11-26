package ui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// acctScreen is the accounts screen (ctrl+a) in the detail pane: a list of
// accounts with their servers and identities, or a form over it.
type acctScreen struct {
	list []tb.AccountDetails
	rows []acctRow
	cur  int
	form *acctForm
	busy bool // a request is in flight: no second submit
}

// acctRow is a selectable list row: an account (ident -1) or one of its identities.
type acctRow struct{ acct, ident int }

type acctForm struct {
	kind   string // "add" (name, email, password), "servers" (add, step 2), "server" (edit) or "identity"
	title  string
	acct   string          // account id (server, identity)
	ident  tb.IdentityFull // identity being edited; ID "" adds one
	base   tb.NewAccount   // add: what step 1 entered
	hasOut bool            // server: the account has an outgoing server
	in     tb.Server       // server: the settings as loaded, so only edited fields are sent
	out    tb.Server
	fields []formField
	cur    int
}

type formField struct {
	key, label string
	in         textinput.Model
}

type (
	acctListMsg struct {
		list []tb.AccountDetails
		err  error
	}
	acctConfigMsg struct {
		form *acctForm // the form that asked: a later form ignores it
		cfg  tb.Config
		err  error
	}
	acctDoneMsg struct {
		form    *acctForm // the form submitted (nil: a list action); only it is closed
		text    string
		err     error
		folders bool // the set of accounts changed: reload folders and tabs
	}
)

func (m Model) openAccounts() (tea.Model, tea.Cmd) {
	m.accts, m.mode, m.status = &acctScreen{}, mAccounts, "loading accounts…"
	return m, m.loadAccounts()
}

func (m Model) loadAccounts() tea.Cmd {
	if m.accts == nil {
		return nil
	}
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		l, err := c.AccountDetails(ctx)
		return acctListMsg{l, err}
	}
}

// acctUpdate handles the accounts screen's async results; ok is false for other messages.
func (m Model) acctUpdate(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	a := m.accts
	switch msg := msg.(type) {
	case acctListMsg:
		if a == nil {
			return m, nil, true
		}
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil, true
		}
		a.list, a.rows = msg.list, nil
		for i, d := range a.list {
			a.rows = append(a.rows, acctRow{i, -1})
			for j := range d.Identities {
				a.rows = append(a.rows, acctRow{i, j})
			}
		}
		a.cur = max(0, min(a.cur, len(a.rows)-1))
		if !a.busy && a.form == nil {
			m.status = fmt.Sprintf("%d accounts", len(a.list))
		}
		return m, nil, true
	case acctConfigMsg:
		if a == nil {
			return m, nil, true
		}
		a.busy = false
		if a.form != msg.form { // closed or replaced while looking up
			return m, nil, true
		}
		in, out := msg.cfg.Incoming, msg.cfg.Outgoing
		email := a.form.base.Email
		switch {
		case msg.err == nil:
			m.err, m.status = "", "found settings ("+msg.cfg.Source+"): check them, enter on the last field adds the account"
		case tb.Code(msg.err) == "NOT_FOUND":
			m.err, m.status = "", "no settings found for "+email+": enter them"
			in, out = guessServers(email)
		default:
			m.err = "looking up settings: " + msg.err.Error() + " (enter them by hand)"
			in, out = guessServers(email)
		}
		base := a.form.base
		a.form = &acctForm{kind: "servers", title: "New account " + email + " · servers", base: base,
			fields: append(serverFields("in", "incoming", in, true), serverFields("out", "outgoing", out, false)...)}
		return m, a.form.focus(0), true
	case acctDoneMsg:
		if a != nil {
			a.busy = false
		}
		if msg.err != nil {
			m.err = msg.err.Error() // the form stays open so nothing is lost
			return m, nil, true
		}
		m.err, m.status = "", msg.text
		if a != nil && a.form == msg.form {
			a.form = nil
		}
		cmds := []tea.Cmd{m.loadAccounts()}
		if msg.folders {
			cmds = append(cmds, m.loadFolders())
		}
		return m, tea.Batch(cmds...), true
	}
	return m, nil, false
}

func (m Model) keyAccounts(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	a, s := m.accts, k.String()
	if a.form != nil {
		return m.keyAcctForm(k)
	}
	switch s {
	case "esc", "q", "ctrl+a":
		m.accts, m.mode, m.status = nil, mNormal, ""
		return m, nil
	case "a":
		a.form = &acctForm{kind: "add", title: "New account", fields: []formField{
			field("name", "your name", "", "shown as the sender"),
			field("email", "email", "", "you@example.org"),
			secretField("password", "password", "empty for OAuth sign-in (Gmail, Outlook…)")}}
		m.err = ""
		return m, a.form.focus(0)
	case "r", "f5":
		m.status = "loading accounts…"
		return m, m.loadAccounts()
	case "j", "down", "k", "up", "g", "G", "home", "end", "ctrl+d", "ctrl+u", "pgdown", "pgup":
		a.cur = move(a.cur, len(a.rows), s, m.preview.Height/2)
		return m, nil
	}
	if a.cur >= len(a.rows) || a.busy {
		if a.busy {
			m.status = "still working…"
		}
		return m, nil
	}
	row := a.rows[a.cur]
	d := a.list[row.acct]
	c, ctx := m.c, m.ctx
	switch s {
	case "D":
		email := acctEmail(d)
		// POP3 mail often lives only on this disk: keep its files.
		removeFiles := d.Type != "pop3"
		text := fmt.Sprintf("Remove account %s? Its local copy of the mail is deleted; the mail on the server stays.", email)
		if !removeFiles {
			text = fmt.Sprintf("Remove account %s? Its downloaded (POP3) mail files stay on disk in your Thunderbird profile.", email)
		}
		m.confirm = &confirmation{text: text,
			onYes: tea.Sequence(func() tea.Msg { return statusMsg("removing " + email + "…") }, func() tea.Msg {
				return acctDoneMsg{text: "removed " + email, err: c.DeleteAccount(ctx, d.ID, removeFiles), folders: true}
			})}
		m.mode = mConfirm
		return m, nil
	case "i":
		id := tb.IdentityFull{}
		if def := defaultIdent(d); def != nil {
			id.Name = def.Name
		}
		a.form = identityForm(d, id)
		m.err = ""
		return m, a.form.focus(0)
	case "e", "enter":
		m.err = ""
		if row.ident >= 0 {
			a.form = identityForm(d, d.Identities[row.ident])
			return m, a.form.focus(0)
		}
		fs := serverFields("in", "incoming", d.Incoming, false)
		if d.Outgoing != nil {
			fs = append(fs, serverFields("out", "outgoing", *d.Outgoing, false)...)
		}
		a.form = &acctForm{kind: "server", title: "Servers of " + acctEmail(d), acct: d.ID, hasOut: d.Outgoing != nil, in: d.Incoming,
			fields: append(fs, secretField("password", "new password", "empty keeps the current one"))}
		if d.Outgoing != nil {
			a.form.out = *d.Outgoing
		}
		return m, a.form.focus(0)
	}
	if row.ident < 0 {
		return m, nil
	}
	id := d.Identities[row.ident]
	switch s {
	case "x":
		if len(d.Identities) == 1 {
			m.status = "an account needs at least one identity"
			return m, nil
		}
		m.confirm = &confirmation{text: fmt.Sprintf("Delete the identity %s?", identLabel(id)), onYes: func() tea.Msg {
			return acctDoneMsg{text: "deleted " + id.Email, err: c.DeleteIdentity(ctx, id.ID)}
		}}
		m.mode = mConfirm
		return m, nil
	case "*":
		if id.Default {
			m.status = id.Email + " is already the default"
			return m, nil
		}
		a.busy = true
		return m, func() tea.Msg {
			return acctDoneMsg{text: id.Email + " is now the default", err: c.SetDefaultIdentity(ctx, d.ID, id.ID), folders: true}
		}
	}
	return m, nil
}

func (m Model) keyAcctForm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	a, f := m.accts, m.accts.form
	switch k.String() {
	case "esc":
		a.form, m.err, m.status = nil, "", ""
		if a.busy {
			m.status = "still working: the result shows here"
		}
		return m, nil
	case "tab", "down":
		return m, f.focus(f.cur + 1)
	case "shift+tab", "up":
		return m, f.focus(f.cur - 1)
	case "enter":
		if f.cur < len(f.fields)-1 {
			return m, f.focus(f.cur + 1)
		}
		if a.busy {
			m.status = "still working…"
			return m, nil
		}
		return m.submitAcctForm()
	}
	var cmd tea.Cmd
	f.fields[f.cur].in, cmd = f.fields[f.cur].in.Update(k)
	return m, cmd
}

// submitAcctForm validates the form and starts its request.
func (m Model) submitAcctForm() (tea.Model, tea.Cmd) {
	a, f := m.accts, m.accts.form
	v := f.values()
	c, ctx := m.c, m.ctx
	var cmd tea.Cmd
	switch f.kind {
	case "add":
		na, err := parseNewAccount(v)
		if err != nil {
			m.err = err.Error()
			return m, nil
		}
		f.base = na
		m.status = "looking up settings for " + na.Email + "…"
		cmd = func() tea.Msg {
			cfg, err := c.FindConfig(ctx, na.Email)
			return acctConfigMsg{f, cfg, err}
		}
	case "servers":
		na := f.base
		var err error
		if na.Incoming, na.Outgoing, err = parseServers(v, true); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.status = "adding " + na.Email + " (checking the login)…"
		if na.Incoming.Auth == "oauth2" || na.Outgoing.Auth == "oauth2" {
			m.status = "adding " + na.Email + ": the Thunderbird window opens for the sign-in…"
		}
		cmd = func() tea.Msg {
			_, err := c.CreateAccount(ctx, na, func(s string) { go prog.Send(statusMsg(s)) })
			return acctDoneMsg{form: f, text: "added " + na.Email, err: err, folders: true}
		}
	case "server":
		in, out, err := parseServers(v, f.hasOut)
		if err != nil {
			m.err = err.Error()
			return m, nil
		}
		// Send only what was edited: an untouched field keeps Thunderbird's exact value.
		in = changedFields(f.in, in)
		var outp *tb.Server
		if f.hasOut {
			out = changedFields(f.out, out)
			outp = &out
		}
		pw, acct := v["password"], f.acct
		m.status = "saving server settings…"
		cmd = func() tea.Msg {
			_, err := c.UpdateServer(ctx, acct, &in, outp, pw)
			return acctDoneMsg{form: f, text: "saved server settings", err: err}
		}
	case "identity":
		id, err := parseIdentity(v, f.ident)
		if err != nil {
			m.err = err.Error()
			return m, nil
		}
		acct := f.acct
		m.status = "saving " + id.Email + "…"
		cmd = func() tea.Msg {
			if id.ID == "" {
				_, err := c.CreateIdentity(ctx, acct, id)
				return acctDoneMsg{form: f, text: "added " + id.Email, err: err, folders: true}
			}
			_, err := c.UpdateIdentity(ctx, id)
			return acctDoneMsg{form: f, text: "saved " + id.Email, err: err, folders: true}
		}
	}
	m.err, a.busy = "", true
	return m, cmd
}

// ---- forms ----

func field(key, label, val, placeholder string) formField {
	in := textinput.New()
	in.Prompt, in.Placeholder, in.CharLimit = "", placeholder, 1000
	in.SetValue(val)
	return formField{key: key, label: label, in: in}
}

func secretField(key, label, placeholder string) formField {
	f := field(key, label, "", placeholder)
	f.in.EchoMode, f.in.EchoCharacter = textinput.EchoPassword, '•'
	return f
}

// focus moves the cursor to field i (clamped).
func (f *acctForm) focus(i int) tea.Cmd {
	f.fields[f.cur].in.Blur()
	f.cur = max(0, min(i, len(f.fields)-1))
	return f.fields[f.cur].in.Focus()
}

func (f *acctForm) values() map[string]string {
	v := map[string]string{}
	for _, x := range f.fields {
		v[x.key] = x.in.Value()
		if x.in.EchoMode != textinput.EchoPassword {
			v[x.key] = strings.TrimSpace(v[x.key])
		}
	}
	return v
}

var (
	socketTypes = []string{"plain", "starttls", "tls", "starttls-optional"}
	authTypes   = []string{"password-cleartext", "password-encrypted", "oauth2", "ntlm", "gssapi", "external", "none", "secure", "any", "old", "default"}
)

// serverFields are the form fields for one server, keyed prefix+name.
// withType lets the incoming server's protocol be chosen (new accounts).
func serverFields(prefix, label string, s tb.Server, withType bool) []formField {
	var fs []formField
	if withType {
		fs = append(fs, field(prefix+"type", label+" type", s.Type, "imap / pop3"))
	}
	port := ""
	if s.Port > 0 {
		port = strconv.Itoa(s.Port)
	}
	return append(fs,
		field(prefix+"host", label+" server", s.Hostname, "mail.example.org"),
		field(prefix+"port", label+" port", port, "e.g. 993, 143, 465, 587"),
		field(prefix+"security", label+" security", s.SocketType, strings.Join(socketTypes, " / ")),
		field(prefix+"auth", label+" auth", s.Auth, "password-cleartext / oauth2 / …"),
		field(prefix+"user", label+" username", s.Username, "usually your email address"))
}

// parseServer reads one server's fields. typ is the server type; "" takes it
// from the form's type field (imap or pop3), and no type field leaves it unset.
func parseServer(v map[string]string, prefix, label, typ string) (tb.Server, error) {
	s := tb.Server{Type: typ, Hostname: v[prefix+"host"], SocketType: strings.ToLower(v[prefix+"security"]),
		Auth: strings.ToLower(v[prefix+"auth"]), Username: v[prefix+"user"]}
	if t, ok := v[prefix+"type"]; ok && typ == "" {
		s.Type = strings.ToLower(t)
		if s.Type != "imap" && s.Type != "pop3" {
			return s, fmt.Errorf("%s type: imap or pop3", label)
		}
	}
	if s.Hostname == "" || strings.ContainsAny(s.Hostname, " /:@") {
		return s, fmt.Errorf("%s server: a host name like mail.example.org", label)
	}
	port, err := strconv.Atoi(v[prefix+"port"])
	if err != nil || port < 1 || port > 65535 {
		return s, fmt.Errorf("%s port: a number from 1 to 65535", label)
	}
	s.Port = port
	if !slices.Contains(socketTypes, s.SocketType) {
		return s, fmt.Errorf("%s security: one of %s", label, strings.Join(socketTypes, ", "))
	}
	if !slices.Contains(authTypes, s.Auth) {
		return s, fmt.Errorf("%s auth: one of %s", label, strings.Join(authTypes, ", "))
	}
	if s.Username == "" && s.Auth != "none" {
		return s, fmt.Errorf("%s username: required unless auth is none", label)
	}
	return s, nil
}

// changedFields keeps only the fields of edited that differ from orig; the
// rest are zero, which UpdateServer leaves out (omitempty).
func changedFields(orig, edited tb.Server) tb.Server {
	var s tb.Server
	if edited.Hostname != orig.Hostname {
		s.Hostname = edited.Hostname
	}
	if edited.Port != orig.Port {
		s.Port = edited.Port
	}
	if edited.SocketType != orig.SocketType {
		s.SocketType = edited.SocketType
	}
	if edited.Auth != orig.Auth {
		s.Auth = edited.Auth
	}
	if edited.Username != orig.Username {
		s.Username = edited.Username
	}
	return s
}

// parseServers reads the incoming and (if withOut) outgoing server fields.
func parseServers(v map[string]string, withOut bool) (in, out tb.Server, err error) {
	if in, err = parseServer(v, "in", "incoming", ""); err != nil || !withOut {
		return in, out, err
	}
	out, err = parseServer(v, "out", "outgoing", "smtp")
	return in, out, err
}

func parseNewAccount(v map[string]string) (tb.NewAccount, error) {
	a := tb.NewAccount{Name: v["name"], Email: v["email"], Password: v["password"]}
	if a.Name == "" {
		return a, fmt.Errorf("your name: required (it is shown as the sender)")
	}
	if !validEmail(a.Email) {
		return a, fmt.Errorf("email: an address like you@example.org")
	}
	return a, nil
}

// parseIdentity applies the identity form to base (keeping its id and HTML flag).
func parseIdentity(v map[string]string, base tb.IdentityFull) (tb.IdentityFull, error) {
	id := base
	id.Email, id.Name, id.ReplyTo, id.Organization, id.Signature = v["email"], v["name"], v["replyTo"], v["organization"], v["signature"]
	if !validEmail(id.Email) {
		return id, fmt.Errorf("email: an address like you@example.org")
	}
	if id.ReplyTo != "" && !validEmail(id.ReplyTo) {
		return id, fmt.Errorf("reply-to: an address like you@example.org, or empty")
	}
	return id, nil
}

func validEmail(s string) bool {
	user, dom, ok := strings.Cut(s, "@")
	return ok && user != "" && strings.Contains(dom, ".") && !strings.ContainsAny(s, " <>,;\"")
}

func identityForm(d tb.AccountDetails, id tb.IdentityFull) *acctForm {
	title := "New identity in " + acctEmail(d)
	if id.ID != "" {
		title = "Identity " + id.Email
	}
	return &acctForm{kind: "identity", title: title, acct: d.ID, ident: id, fields: []formField{
		field("email", "email", id.Email, "you@example.org"),
		field("name", "name", id.Name, "shown as the sender"),
		field("replyTo", "reply-to", id.ReplyTo, "optional"),
		field("organization", "organization", id.Organization, "optional"),
		field("signature", "signature", id.Signature, "optional")}}
}

// guessServers prefills manual entry when no settings were found.
func guessServers(email string) (in, out tb.Server) {
	_, dom, _ := strings.Cut(email, "@")
	in = tb.Server{Type: "imap", Hostname: "imap." + dom, Port: 993, SocketType: "tls", Auth: "password-cleartext", Username: email}
	out = tb.Server{Type: "smtp", Hostname: "smtp." + dom, Port: 465, SocketType: "tls", Auth: "password-cleartext", Username: email}
	return in, out
}

func defaultIdent(d tb.AccountDetails) *tb.IdentityFull {
	for i := range d.Identities {
		if d.Identities[i].Default {
			return &d.Identities[i]
		}
	}
	if len(d.Identities) > 0 {
		return &d.Identities[0]
	}
	return nil
}

// acctEmail names an account by its default address.
func acctEmail(d tb.AccountDetails) string {
	if id := defaultIdent(d); id != nil && id.Email != "" {
		return id.Email
	}
	return d.Name
}

func identLabel(id tb.IdentityFull) string {
	if id.Name == "" {
		return id.Email
	}
	return id.Name + " <" + id.Email + ">"
}

// ---- view ----

func serverLine(s tb.Server) string {
	return fmt.Sprintf("%s %s:%d %s · %s · %s", s.Type, s.Hostname, s.Port, s.SocketType, s.Auth, s.Username)
}

func (m Model) acctLines(w, h int) []string {
	a := m.accts
	if f := a.form; f != nil {
		lines := []string{bold.Render("tab/↓ next field · enter next, on the last field submits · esc cancel"), ""}
		for i, x := range f.fields {
			label := fmt.Sprintf("%-20s ", x.label)
			if i == f.cur {
				label = bold.Render(label)
			} else {
				label = dim.Render(label)
			}
			lines = append(lines, label+x.in.View())
		}
		return lines
	}
	if len(a.list) == 0 {
		return []string{dim.Render("(no accounts) · a adds one")}
	}
	var lines []string
	curLine := 0
	for i, r := range a.rows {
		d := a.list[r.acct]
		var line string
		if r.ident < 0 {
			if i > 0 {
				lines = append(lines, "")
			}
			line = bold.Render(render.Sanitize(d.Name)) + dim.Render(" ("+d.Type+")")
		} else {
			id := d.Identities[r.ident]
			mark := "  "
			if id.Default {
				mark = flagSt.Render("★ ")
			}
			line = "   " + mark + render.Sanitize(identLabel(id))
		}
		if i == a.cur {
			curLine = len(lines)
			line = highlight(line, w, true)
		}
		lines = append(lines, line)
		if r.ident < 0 {
			lines = append(lines, dim.Render("   in  "+render.Sanitize(serverLine(d.Incoming))))
			if d.Outgoing != nil {
				lines = append(lines, dim.Render("   out "+render.Sanitize(serverLine(*d.Outgoing))))
			}
		}
	}
	from, to := window(len(lines), curLine, h)
	return lines[from:to]
}

func (m Model) acctHelp() string {
	if m.accts.form != nil {
		return "enter next/submit · esc cancel"
	}
	return "a add · e edit · D remove · i add identity · x delete identity · * default · esc close"
}

// viewGone reports whether a folder the current list shows no longer exists.
func (m Model) viewGone() bool {
	have := map[string]bool{}
	for _, f := range m.folders {
		have[f.ID] = true
	}
	for _, id := range m.view.folderIDs {
		if !have[id] {
			return true
		}
	}
	return false
}
