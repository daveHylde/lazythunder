package ui

import "testing"

func TestSafeLink(t *testing.T) {
	for _, c := range []struct {
		url, label string
		want       bool
	}{
		{"https://shop.no/order/9", "Se ordren", true},
		{"https://click.mailchimp.com/x?y=1", "Les mer", true},
		{"https://www.shop.no/a.html", "shop.no", true},
		{"https://news.bbc.co.uk/a", "bbc.co.uk/news", true},
		{"http://shop.no/", "Se", false},
		{"https://1.2.3.4/login", "Logg inn", false},
		{"https://xn--dnb-4oa.no/", "DNB", false},
		{"https://bit.ly/abc", "Se", false},
		{"https://cdn.x.no/setup.exe", "Last ned", false},
		{"https://x.no/files/a.zip", "Last ned", false},
		{"https://evil.com/dnb", "https://www.dnb.no/login", false},
		{"https://evil.com/dnb", "dnb.no", false},
		{"https://x.no/unsubscribe?u=1", "here", false},
		{"https://x.no/p", "Avmeld nyhetsbrev", false},
		{"https://user@x.no/", "x", false},
		{"https://x.no:8443/", "x", false},
	} {
		if got := safeLink(c.url, c.label); got != c.want {
			t.Errorf("safeLink(%q, %q) = %v", c.url, c.label, got)
		}
	}
}
