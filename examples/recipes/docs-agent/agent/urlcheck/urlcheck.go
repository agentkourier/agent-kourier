// Package urlcheck is the one rule set for the URLs that the docs agent and its MCP server are given: an absolute
// http or https URL with a host, and with no credentials, query or fragment. A URL that carries a secret in any of
// those places would be copied into an agent card, a log line or a citation, so none is accepted.
package urlcheck

import (
	"errors"
	"net/url"
	"strings"
)

// HTTP returns raw parsed, if it is an absolute http or https URL that meets the rules in the package comment.
// The error names the rule, never the URL: a rejected URL may hold a password.
func HTTP(raw string) (*url.URL, error) { return check(raw, false) }

// HTTPS is HTTP, and the scheme must be https.
func HTTPS(raw string) (*url.URL, error) { return check(raw, true) }

func check(raw string, httpsOnly bool) (*url.URL, error) {
	want := "an absolute http or https URL"
	if httpsOnly {
		want = "an absolute https URL"
	}
	rule := errors.New("must be " + want + " with a host, and without credentials, a query or a fragment")
	// A '?' or '#' with nothing after it leaves no query or fragment in the parsed URL, so look at the text.
	if strings.ContainsAny(raw, "?#") {
		return nil, rule
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil {
		return nil, rule
	}
	if u.Scheme != "https" && (httpsOnly || u.Scheme != "http") {
		return nil, rule
	}
	return u, nil
}
