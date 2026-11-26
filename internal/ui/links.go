package ui

import "strings"

// shortURL is a link's host and path without the scheme, for grey subtitles.
func shortURL(raw string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	return strings.TrimPrefix(s, "www.")
}
