package unsub

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"

	"github.com/davidhyldgaard/lazythunder/internal/agent"
)

func TestMethodsOrder(t *testing.T) {
	h := mail.Header{
		"List-Unsubscribe":      {"<mailto:u@list.no?subject=stop>, <https://list.no/u/1>"},
		"List-Unsubscribe-Post": {"List-Unsubscribe=One-Click"},
	}
	ms := Methods(h, [][2]string{{"https://list.no/u/1", "Unsubscribe"}, {"https://x.no/avmeld", "her"}, {"https://x.no/a", "Les mer"}, {"http://x.no/unsub", "unsub"}})
	var got []string
	for _, m := range ms {
		got = append(got, m.String()+" "+m.URL)
	}
	// the header's https link is both one-click and web; dedupe keeps the first
	want := "one-click https://list.no/u/1|mail to u@list.no mailto:u@list.no?subject=stop|web page x.no https://x.no/avmeld"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %v", got)
	}
}

func TestOneClickAndMailto(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = r.Method + " " + string(b)
	}))
	defer srv.Close()
	var sent string
	d := Deps{HTTP: srv.Client(), Progress: func(string) {},
		Send: func(_ context.Context, to, subj, b string) error { sent = to + "|" + subj + "|" + b; return nil }}
	if _, err := Do(context.Background(), d, []Method{{OneClick, srv.URL}}); err != nil || body != "POST List-Unsubscribe=One-Click" {
		t.Fatalf("one-click: %v %q", err, body)
	}
	if _, err := Do(context.Background(), d, []Method{{Mailto, "mailto:Stop%40list.no?subject=remove%20me&cc=boss@x.no&attach=/etc/passwd"}}); err != nil || sent != "Stop@list.no|remove me|unsubscribe" {
		t.Fatalf("mailto: %v %q", err, sent)
	}
	if _, err := Do(context.Background(), d, []Method{{Mailto, "mailto:a@x.no,b@y.no"}}); err == nil {
		t.Fatal("several recipients accepted")
	}
}

func TestWebFormLoop(t *testing.T) {
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			r.ParseForm()
			posted = r.PostForm.Encode()
			io.WriteString(w, "<p>You are unsubscribed.</p>")
			return
		}
		io.WriteString(w, `<form method="post" action="/done"><input type="hidden" name="t" value="tok">
			<input name="email"><select name="freq"><option value="weekly">Weekly</option><option value="never">Never</option></select>
			<input type="checkbox" name="all" value="1"><button name="go" value="unsub">Unsubscribe</button></form>`)
	}))
	defer srv.Close()
	var pages []string
	step := func(_ context.Context, me, page string) (agent.WebStep, error) {
		pages = append(pages, page)
		if strings.Contains(page, "unsubscribed") {
			return agent.WebStep{Result: "unsubscribed"}, nil
		}
		return agent.WebStep{Result: "submit", Form: 1, Fields: []agent.Field{{Name: "email", Value: me}, {Name: "freq", Value: "never"}, {Name: "all", Value: "1"}, {Name: "go", Value: "unsub"}}}, nil
	}
	d := Deps{HTTP: srv.Client(), Me: "me@x.no", Step: step, Progress: func(string) {}}
	if _, err := Do(context.Background(), d, []Method{{Web, srv.URL + "/u"}}); err != nil {
		t.Fatal(err)
	}
	if posted != "all=1&email=me%40x.no&freq=never&go=unsub&t=tok" {
		t.Fatalf("posted %q", posted)
	}
	if !strings.Contains(pages[0], "freq type=option value=\"weekly\" [checked]") {
		t.Fatalf("form description:\n%s", pages[0])
	}
}

func TestWebFetchErrorAfterStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<a href="https://127.0.0.1:1/x">unsub</a><form action="javascript:alert(1)"></form>`)
	}))
	defer srv.Close()
	for _, s := range []agent.WebStep{{Result: "follow", Link: 1}, {Result: "submit", Form: 1}} {
		d := Deps{HTTP: srv.Client(), Progress: func(string) {},
			Step: func(context.Context, string, string) (agent.WebStep, error) { return s, nil }}
		if _, err := Do(context.Background(), d, []Method{{Web, srv.URL}}); err == nil {
			t.Fatalf("%s: failed fetch not reported", s.Result)
		}
	}
}

func TestMailtoSubjectIsOneLine(t *testing.T) {
	var subj string
	d := Deps{Progress: func(string) {}, Send: func(_ context.Context, _, s, _ string) error { subj = s; return nil }}
	Do(context.Background(), d, []Method{{Mailto, "mailto:u@x.no?subject=hi%0d%0aBcc:%20v@y.no"}})
	if subj != "hi Bcc: v@y.no" {
		t.Fatalf("subject %q", subj)
	}
}
