package tb

import "context"

// Server is a mail server's settings (see "Account management" in
// addon/PROTOCOL.md). Empty fields are left out, so it doubles as the
// partial Server that updateServer takes.
type Server struct {
	Type       string `json:"type,omitempty"` // imap, pop3 or smtp
	Hostname   string `json:"hostname,omitempty"`
	Port       int    `json:"port,omitempty"`
	SocketType string `json:"socketType,omitempty"` // plain, starttls or tls
	Auth       string `json:"auth,omitempty"`       // password-cleartext, oauth2, …
	Username   string `json:"username,omitempty"`
}

// IdentityFull is an identity with every field the add-on can edit.
type IdentityFull struct {
	ID              string `json:"id,omitempty"`
	AccountID       string `json:"accountId,omitempty"`
	Email           string `json:"email"`
	Name            string `json:"name"`
	ReplyTo         string `json:"replyTo"`
	Organization    string `json:"organization"`
	Signature       string `json:"signature"`
	SignatureIsHTML bool   `json:"signatureIsHTML"`
	Default         bool   `json:"default,omitempty"`
}

type AccountDetails struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Incoming   Server         `json:"incoming"`
	Outgoing   *Server        `json:"outgoing"` // the default identity's SMTP server; nil when none
	Identities []IdentityFull `json:"identities"`
}

// Config is what findConfig found for an address.
type Config struct {
	Incoming Server `json:"incoming"`
	Outgoing Server `json:"outgoing"`
	Source   string `json:"source"` // isp, autoconfig, exchange or guess
}

// NewAccount is a createAccount request. Password is empty for oauth2.
type NewAccount struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Incoming Server `json:"incoming"`
	Outgoing Server `json:"outgoing"`
}

// AccountDetails lists every mail account (not Local Folders) with its servers and identities.
func (c *Client) AccountDetails(ctx context.Context) (a []AccountDetails, err error) {
	return a, c.call(ctx, "accountDetails", nil, &a)
}

// FindConfig looks up server settings for email the way Thunderbird's account
// setup does (ISP database, autoconfig, guessing). NOT_FOUND when nothing was found.
func (c *Client) FindConfig(ctx context.Context, email string) (cfg Config, err error) {
	return cfg, c.call(ctx, "findConfig", map[string]any{"email": email}, &cfg)
}

// CreateAccount verifies the login and creates the account. An OAuth sign-in
// needs a Thunderbird window: on NEEDS_WINDOW it opens one (replacing the
// headless instance) and retries once. progress gets status lines.
func (c *Client) CreateAccount(ctx context.Context, a NewAccount, progress func(string)) (d AccountDetails, err error) {
	err = c.call(ctx, "createAccount", a, &d)
	if Code(err) != "NEEDS_WINDOW" {
		return d, err
	}
	progress("the sign-in needs a Thunderbird window: opening it")
	open := c.openWindow
	if open == nil {
		open = c.OpenWindow
	}
	if err := open(ctx, progress); err != nil {
		return d, err
	}
	progress("sign in to " + a.Email + " in the Thunderbird window")
	return d, c.call(ctx, "createAccount", a, &d)
}

// DeleteAccount removes an account from Thunderbird; removeFiles also deletes
// its local mail store. Mail on the server is untouched.
func (c *Client) DeleteAccount(ctx context.Context, id string, removeFiles bool) error {
	return c.call(ctx, "deleteAccount", map[string]any{"id": id, "removeFiles": removeFiles}, nil)
}

// UpdateServer changes an account's servers (nil: unchanged; empty fields:
// unchanged) and, when password is set, its stored password.
func (c *Client) UpdateServer(ctx context.Context, account string, in, out *Server, password string) (d AccountDetails, err error) {
	p := map[string]any{"account": account}
	if in != nil {
		p["incoming"] = in
	}
	if out != nil {
		p["outgoing"] = out
	}
	if password != "" {
		p["password"] = password
	}
	return d, c.call(ctx, "updateServer", p, &d)
}

func (c *Client) CreateIdentity(ctx context.Context, account string, id IdentityFull) (out IdentityFull, err error) {
	id.ID, id.AccountID, id.Default = "", account, false
	p := identityParams(id)
	p["account"] = account
	return out, c.call(ctx, "createIdentity", p, &out)
}

// UpdateIdentity writes id's editable fields to the identity id.ID.
func (c *Client) UpdateIdentity(ctx context.Context, id IdentityFull) (out IdentityFull, err error) {
	p := identityParams(id)
	p["id"] = id.ID
	return out, c.call(ctx, "updateIdentity", p, &out)
}

func identityParams(id IdentityFull) map[string]any {
	return map[string]any{"email": id.Email, "name": id.Name, "replyTo": id.ReplyTo,
		"organization": id.Organization, "signature": id.Signature, "signatureIsHTML": id.SignatureIsHTML}
}

// DeleteIdentity removes an identity (not the account's last one).
func (c *Client) DeleteIdentity(ctx context.Context, id string) error {
	return c.call(ctx, "deleteIdentity", map[string]any{"id": id}, nil)
}

func (c *Client) SetDefaultIdentity(ctx context.Context, account, identity string) error {
	return c.call(ctx, "setDefaultIdentity", map[string]any{"account": account, "identity": identity}, nil)
}
