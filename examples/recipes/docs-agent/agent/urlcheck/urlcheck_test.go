package urlcheck_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/urlcheck"
)

// TestRules is the one table of what every URL setting of the recipe accepts.
func TestRules(t *testing.T) {
	tests := []struct {
		raw       string
		httpOK    bool // accepted by HTTP
		httpsOnly bool // accepted by HTTPS
	}{
		{"https://docs.example.com", true, true},
		{"https://docs.example.com/", true, true},
		{"https://docs.example.com:8443/a2a", true, true},
		{"HTTPS://docs.example.com/a2a", true, true},
		{"http://localhost:8080/a2a", true, false},
		{"http://kb-search.kb.svc:8090/", true, false},
		{"https://[::1]:8443/x", true, true},

		{"", false, false},
		{"/a2a", false, false},
		{"kb-search:8090", false, false},
		{"//docs.example.com/a2a", false, false},
		{"ftp://docs.example.com", false, false},
		{"https:/docs.example.com", false, false},
		{"https:docs.example.com", false, false},
		{"https://", false, false},
		{"https://user@docs.example.com", false, false},
		{"https://user:pass@docs.example.com", false, false},
		{"https://@docs.example.com", false, false},
		{"https://docs.example.com/?token=x", false, false},
		{"https://docs.example.com/?", false, false},
		{"https://docs.example.com/#top", false, false},
		{"https://docs.example.com/#", false, false},
		{"https://docs.example.com:port/", false, false},
		{"https://%zz/", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			u, err := urlcheck.HTTP(tt.raw)
			if (err == nil) != tt.httpOK {
				t.Errorf("HTTP(%q) = %v, accepted = %v, want %v", tt.raw, u, err == nil, tt.httpOK)
			}
			if err == nil && u.Host == "" {
				t.Errorf("HTTP(%q) accepted a URL with no host", tt.raw)
			}
			_, err = urlcheck.HTTPS(tt.raw)
			if (err == nil) != tt.httpsOnly {
				t.Errorf("HTTPS(%q) accepted = %v, want %v", tt.raw, err == nil, tt.httpsOnly)
			}
		})
	}
}

// TestErrorNeverEchoesTheURL pins that a URL carrying a password cannot reach a log through its error.
func TestErrorNeverEchoesTheURL(t *testing.T) {
	for _, check := range []func(string) error{
		func(s string) error { _, err := urlcheck.HTTP(s); return err },
		func(s string) error { _, err := urlcheck.HTTPS(s); return err },
	} {
		for _, raw := range []string{"https://user:hunter2@docs.example.com", "ftp://user:hunter2@x", "https://x/?k=hunter2", "https://%zz:hunter2/"} {
			err := check(raw)
			if err == nil {
				t.Fatalf("%q accepted", raw)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q echoes the URL", err)
			}
		}
	}
}
