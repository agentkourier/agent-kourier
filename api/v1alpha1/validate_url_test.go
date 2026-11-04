package v1alpha1

import (
	"strings"
	"testing"
)

// The message about a URL that is refused shows the scheme and the host, which is what a person needs to fix it, and nothing
// else: not a password, a query, a fragment, a path or an opaque part, in which a token may ride. The verdict is the same with
// or without them.
func TestInvalidURLMessageShowsOnlyTheSchemeAndHost(t *testing.T) {
	const tail = `: must be an http or https URL with a host`
	for _, tc := range []struct{ name, url, want string }{
		{"a query", "ftp://internal.example/path?token=s3cret", `spec.url: invalid URL "ftp://internal.example"` + tail},
		{"a fragment", "ftp://internal.example/path#access_token=s3cret", `spec.url: invalid URL "ftp://internal.example"` + tail},
		{"a token in the path", "ftp://internal.example/s3cret/x", `spec.url: invalid URL "ftp://internal.example"` + tail},
		{"a bare question mark", "ftp://internal.example/?", `spec.url: invalid URL "ftp://internal.example"` + tail},
		{"an opaque part", "kagent:s3cret", `spec.url: invalid URL "kagent:"` + tail},
		{"no host", "http:///path?token=s3cret", `spec.url: invalid URL "http:"` + tail},
		{"no scheme and no host", "s3cret", `spec.url: invalid URL ""` + tail},
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
