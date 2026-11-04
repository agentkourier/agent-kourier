package v1alpha1

import (
	"strings"
	"testing"
)

// The message about a URL that is refused leaves out what a token rides in: the query and the fragment, as it leaves out a
// password. The verdict is the same with or without them.
func TestInvalidURLMessageLeavesOutTheQueryAndFragment(t *testing.T) {
	for _, tc := range []struct{ name, url, want string }{
		{"a query", "ftp://internal.example/path?token=s3cret", `spec.url: invalid URL "ftp://internal.example/path": must be an http or https URL with a host`},
		{"a fragment", "ftp://internal.example/path#access_token=s3cret", `spec.url: invalid URL "ftp://internal.example/path": must be an http or https URL with a host`},
		{"a bare question mark", "ftp://internal.example/?", `spec.url: invalid URL "ftp://internal.example/": must be an http or https URL with a host`},
		{"no host", "http:///path?token=s3cret", `spec.url: invalid URL "http:///path": must be an http or https URL with a host`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := load[AgentBackend](t, "agentbackend.yaml")
			a.Spec.URL = tc.url
			err := a.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v; want it to say %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("Validate = %v; the token is in the message", err)
			}
		})
	}
	a := load[AgentBackend](t, "agentbackend.yaml")
	a.Spec.URL = "https://kagent.example/?x=1"
	if err := a.Validate(); err != nil {
		t.Errorf("a valid URL with a query is refused: %v", err)
	}
}
