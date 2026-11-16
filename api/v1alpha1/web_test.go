package v1alpha1

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// web makes the fixture's connection a web connection of the profile, with url.
func web(o *ChatConnection, profile WebProfile, url string) {
	o.Spec.Platform = PlatformWeb
	o.Spec.Web = &WebConnectionSpec{Profile: profile, URL: url}
}

func TestWebConnectionValidate(t *testing.T) {
	const reply = "https://receiver.example.com/replies"
	runMutations[ChatConnection](t, "chatconnection.yaml", []mutation[ChatConnection]{
		{"kourier-v1 ok", func(o *ChatConnection) { web(o, WebProfileKourierV1, reply) }, ""},
		{"github ok", func(o *ChatConnection) { web(o, WebProfileGitHub, "") }, ""},
		{"web with no web block", func(o *ChatConnection) { o.Spec.Platform = PlatformWeb },
			"spec.web: is required on a web connection: it names the profile"},
		{"no profile", func(o *ChatConnection) { web(o, "", reply) }, "spec.web.profile: required"},
		{"a profile that is not supported", func(o *ChatConnection) { web(o, "gitlab", "") },
			`spec.web.profile: unsupported value "gitlab", must be one of kourier-v1, github`},
		{"a profile in the wrong case", func(o *ChatConnection) { web(o, "GitHub", "") },
			`spec.web.profile: unsupported value "GitHub", must be one of kourier-v1, github`},
		{"kourier-v1 with no url", func(o *ChatConnection) { web(o, WebProfileKourierV1, "") },
			"spec.web.url: is required by the kourier-v1 profile, which posts the replies to it"},
		{"github with a url", func(o *ChatConnection) { web(o, WebProfileGitHub, reply) },
			"spec.web.url: is set, but the github profile sends no replies"},
		{"a web block on a slack connection", func(o *ChatConnection) { o.Spec.Web = &WebConnectionSpec{Profile: WebProfileGitHub} },
			"spec.web: is set, but only a web connection has one"},
		{"a slack block on a web connection", func(o *ChatConnection) {
			web(o, WebProfileGitHub, "")
			o.Spec.Slack = &SlackConnectionSpec{Mode: SlackModeHTTP}
		}, "spec.slack: is set, but only a slack connection has one"},
	})
}

// The reply URL of a kourier-v1 connection: https, or http to a host that does not leave the pod or the cluster; never credentials,
// a query or a fragment; and at most maxURLLen bytes. A message shows the scheme and the host at most, not a path or a query, where
// a token may ride.
func TestWebReplyURL(t *testing.T) {
	refusedHTTP := func(shown string) string {
		return fmt.Sprintf("spec.web.url: invalid URL %q: http is allowed only to localhost, a loopback address or a Service in the cluster "+
			"(a host that ends in .svc or .svc.cluster.local); use https", shown)
	}
	for _, tc := range []struct {
		url, want string
	}{
		{"https://receiver.example.com/replies", ""},
		{"HTTPS://Receiver.Example.com:8443/a/b", ""},
		{"https://10.0.0.7/replies", ""},
		{"http://localhost/replies", ""},
		{"http://LOCALHOST:8080", ""},
		{"http://127.0.0.1:9000/replies", ""},
		{"http://127.1.2.3", ""},
		{"http://[::1]:8080/r", ""},
		{"http://receiver.team-a.svc/replies", ""},
		{"http://receiver.team-a.svc:8080/replies", ""},
		{"http://receiver.team-a.svc.cluster.local/replies", ""},
		{"http://Receiver.Team-A.SVC.Cluster.Local:80", ""},
		{"http://receiver.example.com/replies", refusedHTTP("http://receiver.example.com")},
		{"http://10.0.0.7/replies", refusedHTTP("http://10.0.0.7")},
		{"http://receiver/replies", refusedHTTP("http://receiver")},
		{"http://svc/replies", refusedHTTP("http://svc")},
		{"http://evil.svc.example.com/replies", refusedHTTP("http://evil.svc.example.com")},
		{"http://receiver.team-a.svc.cluster.example/x", refusedHTTP("http://receiver.team-a.svc.cluster.example")},
		{"http://localhost.example.com/", refusedHTTP("http://localhost.example.com")},
		{"http://[::2]/", refusedHTTP("http://[::2]")},
		{"http://localhost:/", refusedHTTP("http://localhost:")},
		{"ftp://receiver.example.com/", `spec.web.url: invalid URL "ftp://receiver.example.com": must be an http or https URL with a host`},
		{"receiver.example.com/replies", `spec.web.url: invalid URL "": must be an http or https URL with a host`},
		{"https:///replies", `spec.web.url: invalid URL "https:": must be an http or https URL with a host`},
		{"https://user:secret@receiver.example.com/", "spec.web.url: must not contain credentials; the replies are signed with the Secret's signingSecret"},
		{"https://receiver.example.com/replies?token=x", "spec.web.url: must not have a query or fragment"},
		{"https://receiver.example.com/replies#x", "spec.web.url: must not have a query or fragment"},
		{"https://receiver.example.com/%zz", "spec.web.url: must be an http or https URL with a host"},
		{"https://r.example.com/" + strings.Repeat("a", maxURLLen-len("https://r.example.com/")), ""},
		{"https://r.example.com/" + strings.Repeat("a", maxURLLen+1-len("https://r.example.com/")),
			fmt.Sprintf("spec.web.url: is %d bytes, longer than the %d byte limit", maxURLLen+1, maxURLLen)},
	} {
		c := load[ChatConnection](t, "chatconnection.yaml")
		web(c, WebProfileKourierV1, tc.url)
		var want []string
		if tc.want != "" {
			want = []string{tc.want}
		}
		if got := problemsOf(c.Validate()); !slices.Equal(got, want) {
			t.Errorf("url %.80q: Validate() problems = %q, want %q", tc.url, got, want)
		}
	}
}

// Validate refuses a web url wherever the profile is checked too, so that a slack connection that carries a web block by mistake
// is told everything at once.
func TestAWebBlockOnASlackConnectionIsCheckedToo(t *testing.T) {
	c := load[ChatConnection](t, "chatconnection.yaml")
	c.Spec.Web = &WebConnectionSpec{Profile: "gitlab"}
	want := []string{"spec.web: is set, but only a web connection has one", `spec.web.profile: unsupported value "gitlab", must be one of kourier-v1, github`}
	if got := problemsOf(c.Validate()); !slices.Equal(got, want) {
		t.Errorf("Validate() problems = %q, want %q", got, want)
	}
}

// Output takes none, which the platform's check refuses where Agent Kourier posts the replies, and may be left out, which
// resolution fills in with the platform's default: Validate refuses neither, and Default does not fill it.
func TestBindingOutputValues(t *testing.T) {
	runMutations[Binding](t, "binding.yaml", []mutation[Binding]{
		{"output none ok", func(o *Binding) { o.Spec.Chat.Output = OutputNone }, ""},
		{"output left out ok", func(o *Binding) { o.Spec.Chat.Output = "" }, ""},
		{"output in the wrong case", func(o *Binding) { o.Spec.Chat.Output = "None" },
			`spec.chat.output: unsupported value "None", must be one of live, final, none`},
	})
	b := load[Binding](t, "binding-minimal.yaml")
	if b.Spec.Chat.Output != "" {
		t.Errorf("output after Default = %q; want it left for resolution", b.Spec.Chat.Output)
	}
}

// webURLRule is one rule of spec.web.url, a disjunction of `self == ”`, `self.matches(...)` and `!self.matches(...)` terms, as Go
// evaluates it: CEL's matches is Go's regexp, so a term is its regex.
type webURLRule struct {
	terms []func(string) bool
}

func (r webURLRule) passes(u string) bool {
	return slices.ContainsFunc(r.terms, func(term func(string) bool) bool { return term(u) })
}

func parseWebURLRule(t *testing.T, rule string) webURLRule {
	t.Helper()
	var out webURLRule
	for term := range strings.SplitSeq(rule, " || ") {
		if term == "self == ''" {
			out.terms = append(out.terms, func(u string) bool { return u == "" })
			continue
		}
		m := regexp.MustCompile(`^(!?)self\.matches\(r?'(.*)'\)$`).FindStringSubmatch(term)
		if m == nil {
			t.Fatalf("url rule %q: the term %q is not self == '' or [!]self.matches('regex')", rule, term)
		}
		re, err := regexp.Compile(m[2])
		if err != nil {
			t.Fatalf("url rule %q: %v", rule, err)
		}
		negated := m[1] == "!"
		out.terms = append(out.terms, func(u string) bool { return re.MatchString(u) != negated })
	}
	return out
}

// The schema's http rule is the regex Validate holds an http URL to, so that the two agree on which host an http URL may name.
func TestTheWebHTTPRuleIsValidatesRegex(t *testing.T) {
	rule := ruleWith(t, crdSchema(t, "chatconnections").at(t, "spec.web.url"), "^http://')")
	m := regexp.MustCompile(`\|\| self\.matches\(r'(.*)'\)$`).FindStringSubmatch(rule)
	if m == nil || m[1] != webHTTPURL.String() {
		t.Errorf("the http rule %q does not match webHTTPURL %q", rule, webHTTPURL)
	}
}

// TestCELWebURLRulesRefuseOnlyWhatValidateRefuses holds the rules of spec.web.url to Validate, on a few hundred thousand seeded
// strings, with no cluster, as TestCELURLRulesRefuseOnlyWhatURLParseRefuses does for an AgentBackend's url.
func TestCELWebURLRulesRefuseOnlyWhatValidateRefuses(t *testing.T) {
	var rules []webURLRule
	for _, r := range crdSchema(t, "chatconnections").at(t, "spec.web.url").rules() {
		rules = append(rules, parseWebURLRule(t, r.Rule))
	}
	if len(rules) != 6 {
		t.Fatalf("want 6 url rules, found %d", len(rules))
	}
	refusedByRules := func(u string) bool {
		return slices.ContainsFunc(rules, func(r webURLRule) bool { return !r.passes(u) })
	}
	broker := func(u string) bool {
		c := &ChatConnection{Spec: ChatConnectionSpec{Platform: PlatformWeb, CredentialsSecretRef: SecretRef{Name: "s"},
			Web: &WebConnectionSpec{Profile: WebProfileKourierV1, URL: u}}}
		c.Name, c.Namespace = "a", "b"
		return c.Validate() == nil
	}

	schemes := []string{"http://", "https://", "HTTP://", "hTTps://", "http:", "http:/", "http:///", "ftp://", "//", ""}
	pieces := []string{"h", "localhost", "LocalHost", "127.0.0.1", "127.1", "[::1]", "[::2]", ".svc", ".svc.cluster.local", "svc", "ns", "a-b", ".",
		":", ":80", "/", "//", "@", "u:p@", "#", "?", "%", "%41", "%zz", "[", "]", " ", "\t", "\n", "\x00", "\x7f",
		"\\", "^", "`", "{", "}", "|", "<", ">", "\"", "'", "é", "!", "$", "&", "(", ")", "*", "+", ",", ";", "=", "~", "_", "-"}
	r := rand.New(rand.NewPCG(3, 4)) // seeded, so that a failure repeats
	var accepted, acceptedHTTP, refused int
	for range 300000 {
		var b strings.Builder
		if r.IntN(10) < 9 {
			b.WriteString(schemes[r.IntN(len(schemes))])
		}
		for range 1 + r.IntN(6) {
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
			if strings.HasPrefix(strings.ToLower(u), "http://") {
				acceptedHTTP++
			}
		case refusedByRules(u):
			refused++
		}
	}
	t.Logf("%d URLs accepted by Validate (%d of them http), %d refused by Validate and the rules", accepted, acceptedHTTP, refused)
	if accepted < 2000 || acceptedHTTP < 200 || refused < 5000 {
		t.Errorf("the generator no longer reaches every side: accepted %d (http %d), refused by the rules %d", accepted, acceptedHTTP, refused)
	}
}
