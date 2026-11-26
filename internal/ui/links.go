package ui

import (
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

var (
	shorteners = map[string]bool{"bit.ly": true, "tinyurl.com": true, "t.co": true, "goo.gl": true, "ow.ly": true,
		"is.gd": true, "buff.ly": true, "rebrand.ly": true, "cutt.ly": true, "shorturl.at": true, "tiny.cc": true,
		"rb.gy": true, "t.ly": true, "s.id": true, "lnkd.in": true}
	reUnsub = regexp.MustCompile(`(?i)unsub|opt-?out|avmeld|afmeld|avslutt abonnement|abmelden|manage.?preferences|email.?preferences`)
	// a label that names a site: "www.x.no", "x.no/…", "https://…"
	reLabelHost = regexp.MustCompile(`(?i)^(?:https?://)?((?:[a-z0-9-]+\.)+[a-z]{2,})(?:[/:?#]\S*)?$`)
)

// safeLink is the deterministic part of the link filter (the model may veto
// more): https only, a plain public host name, no shorteners, no executable
// or archive downloads, no label that names a different site than the target,
// and no unsubscribe links (U handles those).
func safeLink(raw, label string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || net.ParseIP(host) != nil || strings.Contains(host, "xn--") || !strings.Contains(host, ".") {
		return false
	}
	for _, r := range host {
		if r > 127 {
			return false
		}
	}
	site := registrable(host)
	if shorteners[site] || shorteners[host] {
		return false
	}
	if r, ok := riskyExts[strings.ToLower(path.Ext(u.Path))]; ok && !strings.HasPrefix(r, "a web page") {
		return false
	}
	if reUnsub.MatchString(raw) || reUnsub.MatchString(label) {
		return false
	}
	if m := reLabelHost.FindStringSubmatch(strings.TrimSpace(label)); m != nil && registrable(strings.ToLower(m[1])) != site {
		return false
	}
	return true
}

func registrable(host string) string {
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

// shortURL is a link's host and path without the scheme, for grey subtitles.
func shortURL(raw string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	return strings.TrimPrefix(s, "www.")
}
