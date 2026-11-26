package unsub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"testing"

	"github.com/daveHylde/lazythunder/internal/agent"
)

func TestMethodsHeaderVariants(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header mail.Header
		body   [][2]string
		want   string
	}{
		{"nothing", mail.Header{}, nil, ""},
		{"https without one-click post is a web page",
			mail.Header{"List-Unsubscribe": {"<https://l.no/u>"}}, nil, "web page l.no"},
		{"http and malformed entries are ignored",
			mail.Header{"List-Unsubscribe": {"<http://l.no/u>, https://bare.no/u, <ftp://l.no>, < mailto:a@l.no >"}}, nil, "mail to a@l.no"},
		{"one-click needs the post header value",
			mail.Header{"List-Unsubscribe": {"<https://l.no/u>"}, "List-Unsubscribe-Post": {"something-else"}}, nil, "web page l.no"},
		{"body links only when they look like unsubscribe",
			mail.Header{}, [][2]string{{"https://n.no/x", "Meld deg av"}, {"https://n.no/opt-out", "here"}, {"https://n.no/read", "Read more"}},
			"web page n.no|web page n.no"},
	} {
		var got []string
		for _, m := range Methods(tc.header, tc.body) {
			got = append(got, m.String())
		}
		if strings.Join(got, "|") != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDoNoMethods(t *testing.T) {
	if _, err := Do(context.Background(), Deps{}, nil); err == nil || !strings.Contains(err.Error(), "no unsubscribe option") {
		t.Fatalf("err %v", err)
	}
}

func TestDoFallsThroughToNextMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	var progress []string
	sendErr := error(nil)
	d := Deps{HTTP: srv.Client(), Progress: func(s string) { progress = append(progress, s) },
		Send: func(context.Context, string, string, string) error { return sendErr }}
	ms := []Method{{OneClick, srv.URL}, {Mailto, "mailto:u@l.no"}}
	got, err := Do(context.Background(), d, ms)
	if err != nil || got != "unsubscribed (mail to u@l.no)" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(progress) != 2 || !strings.Contains(progress[0], "one-click") {
		t.Errorf("progress %q", progress)
	}
	sendErr = errors.New("smtp down")
	_, err = Do(context.Background(), d, ms)
	if err == nil || !strings.Contains(err.Error(), "one-click: server said 500") || !strings.Contains(err.Error(), "mail to u@l.no: smtp down") {
		t.Fatalf("joined error %v", err)
	}
}

func TestMailtoDefaultsAndBody(t *testing.T) {
	var got string
	d := Deps{Progress: func(string) {}, Send: func(_ context.Context, to, s, b string) error { got = to + "|" + s + "|" + b; return nil }}
	for raw, want := range map[string]string{
		"mailto:u@l.no": "u@l.no|unsubscribe|unsubscribe",
		"mailto:u@l.no?body=remove%20me%0Aplease": "u@l.no|unsubscribe|remove me\nplease",
		"mailto:List%20<u@l.no>?subject=Avmeld":   "u@l.no|Avmeld|unsubscribe",
		"mailto:not-an-address":                   "",
		"mailto:a@l.no;b@l.no":                    "",
		"mailto:%zz":                              "",
	} {
		got = ""
		_, err := Do(context.Background(), d, []Method{{Mailto, raw}})
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%s: sent %q err %v, want %q", raw, got, err, want)
		}
	}
}

// pages serves a page per path; "/loop" links to itself.
func pages(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loop":
			io.WriteString(w, `<a href="/loop">again</a>`)
		case "/get":
			fmt.Fprintf(w, "<p>got %s</p>", r.URL.RawQuery)
		default:
			io.WriteString(w, `<script>var secret=1</script><style>p{}</style><p>Hello</p>
				<form action="/get"><input type="hidden" name="t" value="tok"><input name="email"><input type="submit" name="s" value="Go"></form>
				<a href="/loop">Loop</a><a href="mailto:x@y.no">mail</a>`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWebAgentOutcomes(t *testing.T) {
	srv := pages(t)
	for _, tc := range []struct {
		name  string
		start string
		step  func(n int, page string) (agent.WebStep, error)
		want  string // "" = success, else error substring
	}{
		{"needs browser", "/", func(int, string) (agent.WebStep, error) {
			return agent.WebStep{Result: "needs_browser", Explain: "captcha"}, nil
		}, "needs a browser: captcha"},
		{"agent failed", "/", func(int, string) (agent.WebStep, error) {
			return agent.WebStep{Result: "failed", Explain: "no form"}, nil
		}, "agent: no form"},
		{"model down", "/", func(int, string) (agent.WebStep, error) {
			return agent.WebStep{}, errors.New("AI off")
		}, "AI off"},
		{"missing link", "/", func(int, string) (agent.WebStep, error) {
			return agent.WebStep{Result: "follow", Link: 9}, nil
		}, "missing link"},
		{"link zero", "/", func(int, string) (agent.WebStep, error) {
			return agent.WebStep{Result: "follow", Link: 0}, nil
		}, "missing link"},
		{"missing form", "/", func(int, string) (agent.WebStep, error) {
			return agent.WebStep{Result: "submit", Form: 2}, nil
		}, "missing form"},
		{"endless pages", "/loop", func(int, string) (agent.WebStep, error) {
			return agent.WebStep{Result: "follow", Link: 1}, nil
		}, "gave up"},
		{"GET form puts values in the query", "/", func(n int, page string) (agent.WebStep, error) {
			if n == 0 {
				return agent.WebStep{Result: "submit", Form: 1, Fields: []agent.Field{{Name: "email", Value: "me@x.no"}}}, nil
			}
			if !strings.Contains(page, "got email=me%40x.no&t=tok") {
				return agent.WebStep{Result: "failed", Explain: "page: " + page}, nil
			}
			return agent.WebStep{Result: "unsubscribed"}, nil
		}, ""},
	} {
		n := 0
		d := Deps{HTTP: srv.Client(), Me: "me@x.no", Progress: func(string) {},
			Step: func(_ context.Context, _ string, page string) (agent.WebStep, error) {
				s, err := tc.step(n, page)
				n++
				return s, err
			}}
		_, err := Do(context.Background(), d, []Method{{Web, srv.URL + tc.start}})
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
		if tc.name == "endless pages" && n != maxSteps {
			t.Errorf("endless pages: %d agent steps, want %d", n, maxSteps)
		}
	}
}

func TestWebPageAsAgentSeesIt(t *testing.T) {
	srv := pages(t)
	var seen string
	d := Deps{HTTP: srv.Client(), Progress: func(string) {},
		Step: func(_ context.Context, _ string, page string) (agent.WebStep, error) {
			seen = page
			return agent.WebStep{Result: "unsubscribed"}, nil
		}}
	if _, err := Do(context.Background(), d, []Method{{Web, srv.URL + "/"}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"URL: " + srv.URL + "/", "Status: 200 OK", "Hello",
		"Form 1 (GET " + srv.URL + "/get):", `- t type=hidden value="tok"`, "1. Loop → " + srv.URL + "/loop"} {
		if !strings.Contains(seen, want) {
			t.Errorf("page missing %q:\n%s", want, seen)
		}
	}
	for _, bad := range []string{"secret", "p{}", "mailto:"} {
		if strings.Contains(seen, bad) {
			t.Errorf("page leaks %q:\n%s", bad, seen)
		}
	}
}

func TestDescribeBounds(t *testing.T) {
	var links strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&links, `<a href="/l%d">%s</a>`, i, strings.Repeat("x", 100))
	}
	base, _ := url.Parse("https://n.no/")
	p := parse(base, "200 OK", "<p>"+strings.Repeat("ø", 2000)+"</p>"+links.String())
	if len(p.links) != 40 {
		t.Errorf("links %d, want cap 40", len(p.links))
	}
	desc := p.describe()
	if !strings.Contains(desc, "…\n") || strings.Contains(desc, strings.Repeat("ø", 1600)) {
		t.Error("page text not truncated")
	}
	if strings.Contains(desc, strings.Repeat("x", 81)) {
		t.Error("link label not shortened to one short line")
	}
}

func TestWebRefusesNonHTTPStart(t *testing.T) {
	d := Deps{Progress: func(string) {}, Step: func(context.Context, string, string) (agent.WebStep, error) {
		t.Fatal("agent consulted for a non-web link")
		return agent.WebStep{}, nil
	}}
	if _, err := Do(context.Background(), d, []Method{{Web, "file:///etc/passwd"}}); err == nil || !strings.Contains(err.Error(), "not a web link") {
		t.Fatalf("err %v", err)
	}
}
