package v1alpha1

import (
	"fmt"
	"regexp"
	"slices"
	"testing"
)

// The rules on spec.web that name profiles name those of the platform table (ChatPlatformOf), which Validate reads: the url rules the
// profiles with replies, and the identity rules the profiles that take an identity. A profile added to the table without its name in
// a rule, or a name left in a rule after the table changed, fails here. The enum of spec.web.profile is webProfiles, in its order.
func TestTheWebRulesFollowThePlatformTable(t *testing.T) {
	spec := crdSchema(t, "chatconnections").at(t, "spec.web")
	enum, _ := crdSchema(t, "chatconnections").at(t, "spec.web.profile")["enum"].([]any)
	var names []string
	for _, v := range enum {
		names = append(names, fmt.Sprint(v))
	}
	var profiles, replies, identities []string
	for _, p := range webProfiles() {
		profiles = append(profiles, string(p))
		rules := platformOf(t, ChatConnectionSpec{Platform: PlatformWeb, Web: &WebConnectionSpec{Profile: p}})
		if rules.Replies {
			replies = append(replies, string(p))
		}
		if rules.Identity.Taken() {
			identities = append(identities, string(p))
		}
	}
	if !slices.Equal(names, profiles) {
		t.Errorf("the enum of spec.web.profile is %v, and webProfiles %v", names, profiles)
	}
	for _, tc := range []struct {
		what, fragment string
		want           []string
	}{
		{"the url is required", "(has(self.url)", replies},
		{"the url is refused", "!has(self.url)", replies},
		{"the identity is required", "(has(self.identity)", identities},
		{"the identity is refused", "!has(self.identity) ||", identities},
	} {
		rule := ruleWith(t, spec, "self.profile in [", tc.fragment)
		if got := listed(t, rule); !slices.Equal(got, tc.want) {
			t.Errorf("%s: the rule %q lists %v, and the table %v", tc.what, rule, got, tc.want)
		}
	}
}

// The schema's rule on a github connection's identity is the github profile's shape of one: githubLogin, within maxGitHubLoginLen.
func TestTheIdentityRuleCopiesGitHubsShape(t *testing.T) {
	rule := ruleWith(t, crdSchema(t, "chatconnections").at(t, "spec.web"), "self.profile != 'github'", "self.identity.matches(")
	m := regexp.MustCompile(`self\.identity\.matches\(r'(.*)'\)\)$`).FindStringSubmatch(rule)
	if m == nil || m[1] != githubLogin.String() {
		t.Errorf("the identity rule %q does not match githubLogin %q", rule, githubLogin)
	}
	if got := limit(t, rule); got != maxGitHubLoginLen {
		t.Errorf("the identity rule's cap is %d, maxGitHubLoginLen %d", got, maxGitHubLoginLen)
	}
	if shape := platformOf(t, githubSpec).Identity; shape.pattern != githubLogin || shape.maxLen != maxGitHubLoginLen {
		t.Errorf("the github profile's identity is %+v, not githubLogin within maxGitHubLoginLen", shape)
	}
}
