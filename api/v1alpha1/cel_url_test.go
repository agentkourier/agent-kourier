package v1alpha1

import (
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// urlRule reads a spec.url rule of the form [!]self.matches([r]'regex') and returns the regex, and whether the rule
// refuses what the regex matches. CEL's matches is Go's regexp, so the regex is the rule.
func urlRule(t *testing.T, rule string) (re *regexp.Regexp, refusesMatch bool) {
	t.Helper()
	m := regexp.MustCompile(`^(!?)self\.matches\(r?'(.*)'\)$`).FindStringSubmatch(rule)
	if m == nil {
		t.Fatalf("url rule %q is not of the form [!]self.matches('regex')", rule)
	}
	re, err := regexp.Compile(m[2])
	if err != nil {
		t.Fatalf("url rule %q: %v", rule, err)
	}
	return re, m[1] == "!"
}

// TestCELURLRulesRefuseOnlyWhatURLParseRefuses holds the four URL rules to url.Parse, which Validate calls, on a few
// hundred thousand seeded strings, with no cluster. An apiserver evaluates the rules as these regexes (the cluster
// test sends objects to one), so this finds an over-refusal in seconds: the control character rule once read past a
// "#", which url.Parse cuts off before it checks, and refused "http://h#\n", which Validate accepts.
func TestCELURLRulesRefuseOnlyWhatURLParseRefuses(t *testing.T) {
	type rule struct {
		re      *regexp.Regexp
		refuses bool // the rule refuses a URL that the regex matches, and otherwise one that it does not
	}
	var rules []rule
	for _, r := range crdSchema(t, "agentbackends").at(t, "spec.url").rules() {
		re, refuses := urlRule(t, r.Rule)
		rules = append(rules, rule{re, refuses})
	}
	if len(rules) != 4 {
		t.Fatalf("want 4 url rules, found %d", len(rules))
	}
	refusedByRules := func(u string) bool {
		return slices.ContainsFunc(rules, func(r rule) bool { return r.re.MatchString(u) == r.refuses })
	}
	broker := func(u string) bool {
		b := &AgentBackend{Spec: AgentBackendSpec{Dialect: DialectA2A, URL: u}}
		b.Name, b.Namespace = "a", "b"
		return b.Validate() == nil
	}

	schemes := []string{"http://", "https://", "HTTP://", "hTTps://", "http:", "http:/", "http:///", "ftp://", "//", ""}
	pieces := []string{"h", "kagent", ".", ":", ":80", "/", "//", "@", "u:p@", "#", "?", "%", "%41", "%zz", "[::1]", "[", "]", " ", "\t", "\n", "\x00", "\x7f",
		"\\", "^", "`", "{", "}", "|", "<", ">", "\"", "'", "é", "!", "$", "&", "(", ")", "*", "+", ",", ";", "=", "~", "_", "-"}
	r := rand.New(rand.NewPCG(1, 2)) // seeded, so that a failure repeats
	var accepted, refused, ctlAfterFragment int
	for range 300000 {
		var b strings.Builder
		if r.IntN(10) < 9 {
			b.WriteString(schemes[r.IntN(len(schemes))])
		}
		for range 1 + r.IntN(7) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		u := b.String()
		if u == "" {
			continue
		}
		ok := broker(u)
		switch {
		case ok && refusedByRules(u):
			t.Errorf("the rules refuse %q, which Validate accepts", u)
		case ok:
			accepted++
			if i := strings.Index(u, "#"); i >= 0 && strings.ContainsAny(u[i:], "\x00\t\n\x7f") {
				ctlAfterFragment++
			}
		case refusedByRules(u):
			refused++
		}
	}
	t.Logf("%d URLs accepted by Validate (%d with a control character after a #), %d refused by Validate and the rules", accepted, ctlAfterFragment, refused)
	if accepted < 2000 || ctlAfterFragment < 20 || refused < 5000 {
		t.Errorf("the generator no longer reaches both sides: accepted %d, with a control character after a #: %d, refused by the rules %d", accepted, ctlAfterFragment, refused)
	}
}
