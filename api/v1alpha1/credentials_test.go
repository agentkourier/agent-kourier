package v1alpha1

import (
	"slices"
	"testing"
)

// The keys of a Slack connection's credentials Secret are the two of spec section 7, the bot token first, as the broker reads and
// reports them. A platform that is not supported has none.
func TestCredentialKeysPerPlatform(t *testing.T) {
	if got, want := CredentialKeys(PlatformSlack), []string{"botToken", "appToken"}; !slices.Equal(got, want) {
		t.Errorf("CredentialKeys(%q) = %q, want %q", PlatformSlack, got, want)
	}
	for _, p := range []string{"", "teams", "Slack"} {
		if got := CredentialKeys(p); got != nil {
			t.Errorf("CredentialKeys(%q) = %q, want nil: the platform is not supported", p, got)
		}
	}
}

// A caller may sort or change the slice it is given (the broker sorts it) without changing what the next caller gets.
func TestCredentialKeysReturnsACopy(t *testing.T) {
	keys := CredentialKeys(PlatformSlack)
	keys[0] = "changed"
	if got := CredentialKeys(PlatformSlack); got[0] != "botToken" {
		t.Errorf("CredentialKeys(%q) = %q after a caller changed its slice, want the table unchanged", PlatformSlack, got)
	}
}

// Every platform that a ChatConnection may name has keys, and one it may not name has none: a platform added to Validate without keys
// would start with no credentials read.
func TestEveryPlatformValidateAcceptsHasCredentialKeys(t *testing.T) {
	for _, p := range []string{PlatformSlack, "", "teams", "github", "Slack"} {
		c := load[ChatConnection](t, "chatconnection.yaml")
		c.Spec.Platform = p
		if valid, keyed := c.Validate() == nil, CredentialKeys(p) != nil; valid != keyed {
			t.Errorf("platform %q: Validate accepts it %v, CredentialKeys has keys %v: the two must agree", p, valid, keyed)
		}
	}
}
