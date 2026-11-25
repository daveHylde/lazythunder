// Package unsub unsubscribes from mailing lists without a browser: RFC 8058
// one-click POST, a mailto: request, or an agent working the web page over
// plain HTTP.
package unsub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/render"
)

type Kind int

const (
	OneClick Kind = iota // RFC 8058 POST, no page involved
	Mailto               // send a mail to the list's unsubscribe address
	Web                  // a page the agent works through
)

type Method struct {
	Kind Kind
	URL  string
}

func (m Method) String() string {
	switch m.Kind {
	case OneClick:
		return "one-click"
	case Mailto:
		return "mail to " + strings.TrimPrefix(strings.SplitN(m.URL, "?", 2)[0], "mailto:")
	}
	return "web page " + host(m.URL)
}

var reUnsub = regexp.MustCompile(`(?i)unsub|opt-?out|avmeld|afmeld|avslutt abonnement|abmelden|meld deg av|meld av`)

// Methods lists the ways to unsubscribe, best first: the List-Unsubscribe
// header (one-click, then mailto, then its web link), then unsubscribe links
// in the body (url and label pairs).
func Methods(h mail.Header, bodyLinks [][2]string) []Method {
	var out []Method
	seen := map[string]bool{}
	add := func(k Kind, u string) {
		if !seen[u] {
			seen[u] = true
			out = append(out, Method{k, u})
		}
	}
	var https, mailtos []string
	for _, part := range strings.Split(h.Get("List-Unsubscribe"), ",") {
		u := strings.TrimSpace(part)
		if !strings.HasPrefix(u, "<") || !strings.HasSuffix(u, ">") {
			continue
		}
		u = strings.TrimSpace(u[1 : len(u)-1])
		switch l := strings.ToLower(u); {
		case strings.HasPrefix(l, "https://"):
			https = append(https, u)
		case strings.HasPrefix(l, "mailto:"):
			mailtos = append(mailtos, u)
		}
	}
	if strings.Contains(strings.ToLower(h.Get("List-Unsubscribe-Post")), "list-unsubscribe=one-click") {
		for _, u := range https {
			add(OneClick, u)
		}
	}
	for _, u := range mailtos {
		add(Mailto, u)
	}
	for _, u := range https {
		add(Web, u)
	}
	for _, l := range bodyLinks {
		if strings.HasPrefix(strings.ToLower(l[0]), "https://") && (reUnsub.MatchString(l[1]) || reUnsub.MatchString(l[0])) {
			add(Web, l[0])
		}
	}
	return out
}

type Deps struct {
	HTTP     *http.Client // must refuse private addresses; a cookie jar helps multi-step pages
	Me       string       // the subscribed address
	Send     func(ctx context.Context, to, subject, body string) error
	Step     func(ctx context.Context, me, page string) (agent.WebStep, error)
	Progress func(string)
}

// Do tries the methods in order until one works and reports how it went.
func Do(ctx context.Context, d Deps, ms []Method) (string, error) {
	if len(ms) == 0 {
		return "", errors.New("no unsubscribe option in this message")
	}
	var errs []error
	for _, m := range ms {
		d.Progress("unsubscribing: " + m.String() + "…")
		var err error
		switch m.Kind {
		case OneClick:
			err = oneClick(ctx, d.HTTP, m.URL)
		case Mailto:
			err = sendMailto(ctx, d, m.URL)
		case Web:
			err = web(ctx, d, m.URL)
		}
		if err == nil {
			return "unsubscribed (" + m.String() + ")", nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", m, err))
	}
	return "", errors.Join(errs...)
}

func oneClick(ctx context.Context, c *http.Client, u string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader("List-Unsubscribe=One-Click"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("server said %s", resp.Status)
	}
	return nil
}

// sendMailto sends the request mail. Only subject and body come from the
// link; cc, bcc, attachments and other headers in it are ignored.
func sendMailto(ctx context.Context, d Deps, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	to := u.Opaque
	if to == "" {
		to = u.Path
	}
	if to, err = url.PathUnescape(to); err != nil {
		return err
	}
	if a, err := mail.ParseAddress(to); err != nil || strings.ContainsAny(to, ",;") {
		return fmt.Errorf("bad address %q", to)
	} else {
		to = a.Address
	}
	q := u.Query()
	subject, body := strings.Join(strings.Fields(render.Sanitize(q.Get("subject"))), " "), q.Get("body") // one line: no header injection
	if subject == "" {
		subject = "unsubscribe"
	}
	if body == "" {
		body = "unsubscribe"
	}
	return d.Send(ctx, to, subject, body)
}

const maxSteps = 5

// web lets the agent work through the unsubscribe page: read it, then
// submit a form or follow a link, until the page says it is done.
func web(ctx context.Context, d Deps, start string) error {
	p, err := fetch(ctx, d.HTTP, "GET", start, nil)
	for step := 0; ; step++ {
		if err != nil {
			return err
		}
		if step == maxSteps {
			return errors.New("gave up after several pages")
		}
		d.Progress("unsubscribing: borealis2 is reading " + host(p.url) + "…")
		s, serr := d.Step(ctx, d.Me, p.describe())
		if serr != nil {
			return serr
		}
		switch s.Result {
		case "unsubscribed":
			return nil
		case "needs_browser":
			return errors.New("the page needs a browser: " + s.Explain)
		case "follow":
			if s.Link < 1 || s.Link > len(p.links) {
				return errors.New("agent picked a missing link")
			}
			p, err = fetch(ctx, d.HTTP, "GET", p.links[s.Link-1][0], nil)
		case "submit":
			if s.Form < 1 || s.Form > len(p.forms) {
				return errors.New("agent picked a missing form")
			}
			f := p.forms[s.Form-1]
			vals := f.values()
			for _, fv := range s.Fields {
				vals.Set(fv.Name, fv.Value)
			}
			p, err = fetch(ctx, d.HTTP, f.method, f.action, vals)
		default:
			return errors.New("borealis2: " + s.Explain)
		}
	}
}

type page struct {
	url, status, text string
	forms             []form
	links             [][2]string // url, label
}

type form struct {
	method, action string
	fields         []field
}

type field struct {
	name, typ, value, label string
	on                      bool // checked / selected
}

func (f form) values() url.Values {
	v := url.Values{}
	for _, x := range f.fields {
		switch x.typ {
		case "submit", "button", "image", "reset", "file":
		case "checkbox", "radio", "option":
			if x.on {
				v.Add(x.name, x.value)
			}
		default:
			v.Set(x.name, x.value)
		}
	}
	return v
}

const maxPage = 2 << 20

func fetch(ctx context.Context, c *http.Client, method, u string, vals url.Values) (*page, error) {
	if l := strings.ToLower(u); !strings.HasPrefix(l, "https://") && !strings.HasPrefix(l, "http://") {
		return nil, fmt.Errorf("not a web link: %s", u)
	}
	var body io.Reader
	if method == "POST" {
		body = strings.NewReader(vals.Encode())
	} else if vals != nil {
		pu, err := url.Parse(u)
		if err != nil {
			return nil, err
		}
		pu.RawQuery = vals.Encode()
		u = pu.String()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) lazythunder")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return nil, err
	}
	return parse(resp.Request.URL, resp.Status, string(b)), nil
}

func parse(base *url.URL, status, src string) *page {
	p := &page{url: base.String(), status: status, text: render.HTML(src, render.Opts{})}
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return p
	}
	abs := func(ref string) string {
		r, err := base.Parse(strings.TrimSpace(ref))
		if err != nil {
			return ""
		}
		return r.String()
	}
	var cur *form
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Form:
				method := strings.ToUpper(attr(n, "method"))
				if method != "POST" {
					method = "GET"
				}
				action := abs(attr(n, "action"))
				if attr(n, "action") == "" {
					action = base.String()
				}
				p.forms = append(p.forms, form{method: method, action: action})
				cur = &p.forms[len(p.forms)-1]
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				cur = nil
				return
			case atom.Input, atom.Button, atom.Textarea:
				if cur != nil && attr(n, "name") != "" {
					typ := strings.ToLower(attr(n, "type"))
					if n.DataAtom == atom.Button && typ == "" {
						typ = "submit"
					}
					if n.DataAtom == atom.Textarea {
						typ = "textarea"
					}
					val := attr(n, "value")
					if n.DataAtom == atom.Textarea {
						val = text(n)
					}
					_, checked := attrOK(n, "checked")
					cur.fields = append(cur.fields, field{name: attr(n, "name"), typ: typ, value: val, on: checked,
						label: strings.TrimSpace(text(n) + " " + attr(n, "placeholder") + " " + attr(n, "aria-label"))})
				}
			case atom.Select:
				if cur != nil && attr(n, "name") != "" {
					var opts []field
					anySel := false
					var walkOpt func(o *html.Node)
					walkOpt = func(o *html.Node) {
						if o.Type == html.ElementNode && o.DataAtom == atom.Option {
							v, ok := attrOK(o, "value")
							if !ok {
								v = strings.TrimSpace(text(o))
							}
							_, sel := attrOK(o, "selected")
							anySel = anySel || sel
							opts = append(opts, field{name: attr(n, "name"), typ: "option", value: v, on: sel, label: strings.TrimSpace(text(o))})
						}
						for c := o.FirstChild; c != nil; c = c.NextSibling {
							walkOpt(c)
						}
					}
					walkOpt(n)
					if _, multi := attrOK(n, "multiple"); !anySel && !multi && len(opts) > 0 {
						opts[0].on = true // a browser submits the first option by default
					}
					cur.fields = append(cur.fields, opts...)
					return
				}
			case atom.A:
				if u := abs(attr(n, "href")); strings.HasPrefix(u, "http") && len(p.links) < 40 {
					p.links = append(p.links, [2]string{u, strings.TrimSpace(text(n))})
				}
			case atom.Script, atom.Style:
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return p
}

// describe is the page as the agent sees it.
func (p *page) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\nStatus: %s\n\nText:\n", p.url, p.status)
	t := p.text
	if len(t) > 3000 {
		t = strings.ToValidUTF8(t[:3000], "") + "…"
	}
	b.WriteString(t + "\n")
	for i, f := range p.forms {
		fmt.Fprintf(&b, "\nForm %d (%s %s):\n", i+1, f.method, f.action)
		for _, x := range f.fields {
			on := ""
			if x.on {
				on = " [checked]"
			}
			fmt.Fprintf(&b, "- %s type=%s value=%q%s %s\n", x.name, x.typ, x.value, on, oneLine(x.label))
		}
	}
	if len(p.links) > 0 {
		b.WriteString("\nLinks:\n")
		for i, l := range p.links {
			fmt.Fprintf(&b, "%d. %s → %s\n", i+1, oneLine(l[1]), l[0])
		}
	}
	return b.String()
}

func attr(n *html.Node, k string) string { v, _ := attrOK(n, k); return v }

func attrOK(n *html.Node, k string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val, true
		}
	}
	return "", false
}

func text(n *html.Node) string {
	var b strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return b.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(render.Sanitize(s)), " ")
	if len(s) > 80 {
		s = strings.ToValidUTF8(s[:80], "") + "…"
	}
	return s
}

func host(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}
