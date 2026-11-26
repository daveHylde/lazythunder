package tb

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func TestAccountOpsParams(t *testing.T) {
	got := map[string]string{}
	var mu sync.Mutex
	c := fakeHub(t, func(op string, params json.RawMessage) (any, *Error) {
		mu.Lock()
		got[op] = string(params)
		mu.Unlock()
		switch op {
		case "findConfig":
			return map[string]any{"incoming": map[string]any{"type": "imap", "hostname": "imap.x.org", "port": 993},
				"outgoing": map[string]any{"type": "smtp", "port": 465}, "source": "isp"}, nil
		case "accountDetails":
			return []map[string]any{{"id": "account1", "outgoing": nil, "identities": []map[string]any{{"id": "id1", "default": true}}}}, nil
		}
		return nil, nil
	})
	ctx := context.Background()
	cfg, err := c.FindConfig(ctx, "a@x.org")
	if err != nil || cfg.Incoming.Hostname != "imap.x.org" || cfg.Incoming.Port != 993 || cfg.Source != "isp" {
		t.Fatalf("findConfig %+v %v", cfg, err)
	}
	ds, err := c.AccountDetails(ctx)
	if err != nil || len(ds) != 1 || ds[0].Outgoing != nil || !ds[0].Identities[0].Default {
		t.Fatalf("accountDetails %+v %v", ds, err)
	}
	c.DeleteAccount(ctx, "account2", true)
	c.UpdateServer(ctx, "account1", &Server{Port: 143, SocketType: "starttls"}, nil, "pw")
	c.CreateIdentity(ctx, "account1", IdentityFull{ID: "x", Email: "b@x.org", Name: "B"})
	c.UpdateIdentity(ctx, IdentityFull{ID: "id3", Email: "c@x.org"})
	c.DeleteIdentity(ctx, "id3")
	c.SetDefaultIdentity(ctx, "account1", "id2")
	for op, want := range map[string]string{
		"findConfig":         `{"email":"a@x.org"}`,
		"deleteAccount":      `{"id":"account2","removeFiles":true}`,
		"updateServer":       `{"account":"account1","incoming":{"port":143,"socketType":"starttls"},"password":"pw"}`,
		"createIdentity":     `{"account":"account1","email":"b@x.org","name":"B","organization":"","replyTo":"","signature":"","signatureIsHTML":false}`,
		"updateIdentity":     `{"email":"c@x.org","id":"id3","name":"","organization":"","replyTo":"","signature":"","signatureIsHTML":false}`,
		"deleteIdentity":     `{"id":"id3"}`,
		"setDefaultIdentity": `{"account":"account1","identity":"id2"}`,
	} {
		if got[op] != want {
			t.Errorf("%s params %s, want %s", op, got[op], want)
		}
	}
}

func TestCreateAccountOpensWindow(t *testing.T) {
	var mu sync.Mutex
	window, calls := false, 0
	c := fakeHub(t, func(op string, params json.RawMessage) (any, *Error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if !window {
			return nil, &Error{Code: "NEEDS_WINDOW", Msg: "needs a window"}
		}
		var a NewAccount
		json.Unmarshal(params, &a)
		return AccountDetails{ID: "account3", Name: a.Email, Incoming: a.Incoming}, nil
	})
	c.openWindow = func(context.Context, func(string)) error {
		mu.Lock()
		window = true
		mu.Unlock()
		return nil
	}
	var lines []string
	a := NewAccount{Name: "A", Email: "a@x.org", Incoming: Server{Type: "imap", Auth: "oauth2"}, Outgoing: Server{Type: "smtp"}}
	d, err := c.CreateAccount(context.Background(), a, func(s string) { lines = append(lines, s) })
	if err != nil || d.ID != "account3" || d.Incoming.Auth != "oauth2" || calls != 2 || len(lines) == 0 {
		t.Fatalf("got %+v %v, %d calls, progress %q", d, err, calls, lines)
	}

	// a failing window, and other errors, are returned without a retry
	window, calls = false, 0
	c.openWindow = func(context.Context, func(string)) error { return errors.New("no display") }
	if _, err := c.CreateAccount(context.Background(), a, func(string) {}); err == nil || err.Error() != "no display" || calls != 1 {
		t.Fatalf("window failure: %v, %d calls", err, calls)
	}
}
