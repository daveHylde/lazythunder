package bg

import (
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/daveHylde/lazythunder/internal/render"
)

var (
	shorteners = map[string]bool{"bit.ly": true, "tinyurl.com": true, "t.co": true, "goo.gl": true, "ow.ly": true,
		"is.gd": true, "buff.ly": true, "rebrand.ly": true, "cutt.ly": true, "shorturl.at": true, "tiny.cc": true,
		"rb.gy": true, "t.ly": true, "s.id": true, "lnkd.in": true}
	reUnsub = regexp.MustCompile(`(?i)unsub|opt-?out|avmeld|afmeld|avslutt abonnement|abmelden|manage.?preferences|email.?preferences`)
	// a label that names a site: "www.x.no", "x.no/…", "https://…"
	reLabelHost = regexp.MustCompile(`(?i)^(?:https?://)?((?:[a-z0-9-]+\.)+[a-z]{2,})(?:[/:?#]\S*)?$`)
)

// RiskyExts maps dangerous file extensions (".exe") to why they are risky.
var RiskyExts = map[string]string{}

func init() {
	for reason, exts := range map[string]string{
		"an executable":                        "exe msi com scr pif cpl dll so dylib bin run appimage apk app deb rpm jar msix appx",
		"a script":                             "bat cmd vbs vbe js jse mjs wsf wsh ps1 psm1 sh bash zsh fish py pyw pl rb php hta reg scpt applescript command",
		"a shortcut":                           "lnk url desktop webloc inf settingcontent-ms",
		"an archive":                           "zip rar 7z tar gz tgz bz2 xz zst cab arj lzh ace iso img dmg vhd vhdx",
		"a macro Office file":                  "docm dotm xlsm xltm xlam xlsb pptm potm ppsm ppam sldm one",
		"a web page (scripts run when opened)": "html htm xhtml svg mht mhtml",
	} {
		for _, e := range strings.Fields(exts) {
			RiskyExts["."+e] = reason
		}
	}
}

// SafeLink is the deterministic part of the link filter (the model may veto
// more): https only, a plain public host name, no shorteners, no executable
// or archive downloads, no label that names a different site than the target,
// and no unsubscribe links (U handles those).
func SafeLink(raw, label string) bool {
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
	if r, ok := RiskyExts[strings.ToLower(path.Ext(u.Path))]; ok && !strings.HasPrefix(r, "a web page") {
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

// OneLine makes untrusted text safe for a single UI row: no escapes, no newlines.
func OneLine(s string) string { return strings.Join(strings.Fields(render.Sanitize(s)), " ") }
