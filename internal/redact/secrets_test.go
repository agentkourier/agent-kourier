package redact

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
)

func TestSecrets(t *testing.T) {
	const b64 = "dXNlcjpwYXNzd29yZA=="
	for _, c := range []struct{ name, in, want string }{
		{"slack bot token", "auth failed for xoxb-FAKE-1234-abcd today", "auth failed for [redacted] today"},
		{"slack user token", "xoxp-FAKE-token", "[redacted]"},
		{"slack app token", "socket xapp-1-FAKE-abcd failed", "socket [redacted] failed"},
		{"slack refresh and admin tokens", "xoxr-FAKE xoxa-FAKE xoxs-FAKE", "[redacted] [redacted] [redacted]"},
		{"slack token with a format char in the prefix", "x\u200boxb-FAKE and xoxb\u200b-FAKE and xapp\u2060-1-FAKE", "[redacted] and [redacted] and [redacted]"},
		{"slack token stops at unicode space", "xoxb-FAKE\u00a0next", "[redacted]\u00a0next"},
		{"bearer header", "sent Authorization: Bearer FAKE.token.value", "sent Authorization: [redacted]"},
		{"lowercase bearer", "bearer FAKE", "[redacted]"},
		{"bearer across a no-break space", "Bearer\u00a0FAKE", "[redacted]"},
		{"bearer across an em space", "Bearer\u2003FAKE", "[redacted]"},
		{"bearer across a zero-width space", "Bearer\u200bFAKE", "[redacted]"},
		{"bearer across a tab and newline", "Bearer\t\nFAKE", "[redacted]"},
		{"bearer across mixed gap", "Bearer \u200b\u00a0FAKE rest", "[redacted] rest"},
		{"bearer with a format char in the scheme", "Bea\u200brer FAKE", "[redacted]"},
		{"bearer with a format char inside the token", "Bearer FA\u200bKE tail", "[redacted] tail"},
		{"glued bearer", "sent bearerFAKE-TOKEN1 ok", "sent [redacted] ok"},
		{"glued bearer, upper case", "BEARERabc123.def456", "[redacted]"},
		{"basic header", "Authorization: Basic " + b64, "Authorization: [redacted]"},
		{"lowercase basic", "basic " + b64, "[redacted]"},
		{"basic across a no-break space", "Basic\u00a0" + b64, "[redacted]"},
		{"basic across a zero-width space", "Basic\u200b" + b64, "[redacted]"},
		{"basic across an em space and tab", "Basic\u2003\t" + b64 + " rest", "[redacted] rest"},
		{"basic without padding", "Basic YWJjZGVmZ2g", "[redacted]"},
		{"jwt", "token eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0. rejected", "token [redacted] rejected"},
		{"jwt with signature", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln_nature-x", "[redacted]"},
		{"several", "xoxb-A and xapp-B", "[redacted] and [redacted]"},
		{"clean", "approved by U123 for interaction i-42", "approved by U123 for interaction i-42"},
		{"empty", "", ""},
		{"bearer across control characters", "Bearer\x00FAKE Bearer\x1bFAKE Bearer\u009bFAKE Bearer\u007fFAKE", "[redacted] [redacted] [redacted] [redacted]"},
		{"bearer with a colon", "Bearer: FAKE and bearer:FAKE and Bearer :\u200bFAKE", "[redacted] and [redacted] and [redacted]"},
		{"bearer with a percent-encoded space", "Authorization=Bearer%20FAKE&x=1", "Authorization=[redacted]"},
		{"basic with a colon, control character or %20", "Basic: " + b64 + " Basic\x00" + b64 + " Basic%20" + b64, "[redacted] [redacted] [redacted]"},
		{"short glued bearer token", "BearerTOKEN12", "[redacted]"},
		{"glued bearer with a format char in the token", "bearerTOK\u200bEN12345 tail", "[redacted] tail"},
		{"access_token in a query", "GET /cb?access_token=FAKE123&state=ok", "GET /cb?access_token=[redacted]&state=ok"},
		{"oauth keys", "refresh_token=R1 id_token=I1 client_secret=S1 ACCESS_TOKEN=A1", "refresh_token=[redacted] id_token=[redacted] client_secret=[redacted] ACCESS_TOKEN=[redacted]"},
		{"oauth json", `{"token_type":"bearer","access_token":"FAKE","expires_in":3600}`, `{"token_type":"bearer","access_token":"[redacted]","expires_in":3600}`},
		{"oauth json with spacing", `{"client_secret": "FAKE", "refresh_token" :"FAKE2"}`, `{"client_secret": "[redacted]", "refresh_token" :"[redacted]"}`},
		{"oauth key with a format char", "access\u200b_token=FAKE", "access\u200b_token=[redacted]"},
		{"oauth key without a value", "the access_token field, id_token", "the access_token field, id_token"},
		// Escaped-quote JSON is what %q prints for a token-endpoint body.
		{"escaped-quote json", `{\"access_token\":\"FAKE\",\"expires_in\":3600}`, `{\"access_token\":\"[redacted]\",\"expires_in\":3600}`},
		{"escaped-quote json, every key", `{\"refresh_token\":\"R1\",\"id_token\": \"I1\",\"client_secret\" : \"S1\"}`, `{\"refresh_token\":\"[redacted]\",\"id_token\": \"[redacted]\",\"client_secret\" : \"[redacted]\"}`},
		{"double-escaped json", `{\\\"access_token\\\":\\\"FAKE\\\"}`, `{\\\"access_token\\\":\\\"[redacted]\\\"}`},
		{"single-quoted key and value", `{'access_token': 'FAKE', 'scope': 'x'}`, `{'access_token': '[redacted]', 'scope': 'x'}`},
		// camelCase and hyphen spellings, in any case.
		{"camelCase keys", "accessToken=A1 refreshToken=R1 idToken=I1 clientSecret=S1", "accessToken=[redacted] refreshToken=[redacted] idToken=[redacted] clientSecret=[redacted]"},
		{"hyphen keys", "access-token=A1 refresh-token: R1 id-token=I1 client-secret=S1", "access-token=[redacted] refresh-token: [redacted] id-token=[redacted] client-secret=[redacted]"},
		{"key case", "ACCESSTOKEN=A1 Client_Secret=S1 Access-Token=A2", "ACCESSTOKEN=[redacted] Client_Secret=[redacted] Access-Token=[redacted]"},
		{"camelCase json", `{"accessToken":"FAKE","expiresIn":3600,"clientSecret":"S1"}`, `{"accessToken":"[redacted]","expiresIn":3600,"clientSecret":"[redacted]"}`},
		{"camelCase in escaped-quote json", `{\"idToken\":\"FAKE\",\"refresh-token\":\"R1\"}`, `{\"idToken\":\"[redacted]\",\"refresh-token\":\"[redacted]\"}`},
		{"key words split by a format char", "access\u200b-\u200btoken=FAKE", "access\u200b-\u200btoken=[redacted]"},
		// A Bearer or Slack credential ends at a quote, comma, semicolon, brace
		// or backslash, so compact JSON keeps what follows.
		{"compact json keeps the next field", `{"auth":"Bearer FAKE","user":"U1"}`, `{"auth":"[redacted]","user":"U1"}`},
		{"bearer ends at a delimiter", `Bearer A1,x Bearer A2;x {Bearer A3}x 'Bearer A4'x "Bearer A5"x`, `[redacted],x [redacted];x {[redacted]}x '[redacted]'x "[redacted]"x`},
		{"bearer in escaped-quote json", `{\"auth\":\"Bearer FAKE\",\"user\":\"U1\"}`, `{\"auth\":\"[redacted]\",\"user\":\"U1\"}`},
		{"bearer with a colon in compact json", `{"auth":"Bearer: FAKE","user":"U1"}`, `{"auth":"[redacted]","user":"U1"}`},
		{"quoted bearer credential", `Authorization: Bearer "FAKE" ok`, `Authorization: [redacted]" ok`},
		{"bearer with nothing but a quote after it", `{"auth":"Bearer ","user":"U1"}`, `{"auth":"Bearer ","user":"U1"}`},
		{"slack token ends at a quote", `{"t":"xoxb-FAKE-1","user":"U1"}`, `{"t":"[redacted]","user":"U1"}`},
		{"slack token ends at a delimiter", `xoxp-A1,x xapp-1-A2;x {xoxb-A3}x`, `[redacted],x [redacted];x {[redacted]}x`},
		{"slack token in escaped-quote json", `{\"t\":\"xapp-1-FAKE\",\"user\":\"U1\"}`, `{\"t\":\"[redacted]\",\"user\":\"U1\"}`},
		// An empty value does not take the next word.
		{"empty value before a word", "client_secret= next and access_token= more", "client_secret= next and access_token= more"},
		{"empty value before an ampersand", "client_secret=&grant_type=x", "client_secret=&grant_type=x"},
		{"empty json values", `{"access_token":"","user":"U1"} {\"id_token\":\"\",\"user\":\"U1\"}`, `{"access_token":"","user":"U1"} {\"id_token\":\"\",\"user\":\"U1\"}`},
		{"spaced assignment", "client_secret = FAKE and refresh_token =FAKE2", "client_secret = [redacted] and refresh_token =[redacted]"},
		{"space after a colon", "id_token: FAKE", "id_token: [redacted]"},
		// Prose and identifiers that merely mention the keys survive.
		{"keys in prose", "the access token is valid; rotate clientSecret, then accessToken. See getAccessToken() and idTokenHint", "the access token is valid; rotate clientSecret, then accessToken. See getAccessToken() and idTokenHint"},
		{"identifiers that extend a key", "accessTokenTTL=30 access_tokens=3 client-secret-ref: name refresh-tokens=2 tokenSecretRef: s", "accessTokenTTL=30 access_tokens=3 client-secret-ref: name refresh-tokens=2 tokenSecretRef: s"},
		// Go's %q, %+q and %#v print invisible characters as literal escapes
		// (a backslash and t, x00, u00a0), which are gaps too.
		{"bearer across literal escapes", `Bearer\tFAKE Bearer\nFAKE Bearer\x00FAKE Bearer\u00a0FAKE Bearer\u200bFAKE Bearer\U000e0001FAKE Bearer\0FAKE`, "[redacted] [redacted] [redacted] [redacted] [redacted] [redacted] [redacted]"},
		{"bearer with literal escapes in the scheme", `Bea\u200brer FAKE and B\x00earer FAKE`, "[redacted] and [redacted]"},
		{"basic across literal escapes", `Basic\u00a0` + b64 + ` Basic\t` + b64, "[redacted] [redacted]"},
		{"slack token with a literal escape in the prefix", `xo\u200bxb-FAKE xapp\u2060-1-FAKE xox\u200bb-FAKE xoxb\u200b-FAKE`, "[redacted] [redacted] [redacted] [redacted]"},
		{"jwt with literal escapes", `eyJhbGci\u200bOiJub25lIn0.eyJzdWIiOiJ4In0\u2060.sig\u00ad`, "[redacted]"},
		{"oauth key and padding with literal escapes", `access\u200b_token\t:\tFAKE id\x00Token=FAKE2 client_secret\t= FAKE3`, `access\u200b_token\t:\t[redacted] id\x00Token=[redacted] client_secret\t= [redacted]`},
		{"literal escapes that are not gaps", `Bearer\d FAKE bearer\u12 and x\q`, `Bearer\d FAKE bearer\u12 and x\q`},
		// Identifiers that merely contain "bearer" keep their names.
		{"bearer inside identifiers", "task=abc-bearer-xyz12345 payments/bearer-gateway bearer_gateway.svc bearer.example.com", "task=abc-bearer-xyz12345 payments/bearer-gateway bearer_gateway.svc bearer.example.com"},
		// Prose stays readable: only the credential goes.
		{"bearer alone", "the bearer", "the bearer"},
		{"bearer plural and possessive", "bearers of the bearer's duty", "bearers of the bearer's duty"},
		{"bearer with punctuation", "bearer, basic; bearer.", "bearer, basic; bearer."},
		{"short glued word", "bearerless bearerbonds", "bearerless bearerbonds"},
		{"basic prose", "basic setup, Basic Authentication is on, a basic HTTP check", "basic setup, Basic Authentication is on, a basic HTTP check"},
		{"basic at the end", "the basic", "the basic"},
		{"basic with a short token", "Basic dXNlcjp", "Basic dXNlcjp"},
		{"prose around a credential", "Basic setup: Authorization: Basic " + b64 + " is banned", "Basic setup: Authorization: [redacted] is banned"},
	} {
		got := secrets(c.in)
		if got != c.want {
			t.Errorf("%s: secrets(%q) = %q; want %q", c.name, c.in, got, c.want)
		}
		if again := secrets(got); again != got {
			t.Errorf("%s: not idempotent: %q then %q", c.name, got, again)
		}
	}
}

// TestSecretsAnyGapRune sweeps every rune that unicode.IsSpace or the Cf
// or Cc category reports and checks each one is a legal gap between a scheme and its
// credential, so a new Unicode version cannot open a hole.
func TestSecretsAnyGapRune(t *testing.T) {
	credentials := map[string]string{"Bearer": "FAKETOKEN", "Basic": "dXNlcjpwYXNzd29yZA=="}
	n := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Cc, r) {
			continue
		}
		n++
		for scheme, token := range credentials {
			in := "x " + scheme + string(r) + token + " y"
			if got := secrets(in); got != "x [redacted] y" {
				t.Errorf("secrets(%q) = %q; want the credential redacted (U+%04X)", in, got, r)
			}
			// Go's quoting turns an invisible rune into a literal escape.
			for _, verb := range []string{"%q", "%+q"} {
				if got, want := secrets(fmt.Sprintf(verb, in)), `"x [redacted] y"`; got != want {
					t.Errorf("secrets(%s of %q) = %s; want %s (U+%04X)", verb, in, got, want, r)
				}
			}
		}
		// The same runes may pad an OAuth key from its colon and its value.
		g := string(r)
		in := "x access_token" + g + ":" + g + "FAKETOKEN y"
		if got, want := secrets(in), "x access_token"+g+":"+g+"[redacted] y"; got != want {
			t.Errorf("secrets(%q) = %q; want %q (U+%04X)", in, got, want, r)
		}
	}
	if n < 100 {
		t.Fatalf("swept only %d gap runes; the sweep is broken", n)
	}
}

// TestSecretsQuotedBody feeds secrets what %q prints for a
// token-endpoint body, which is the form an audit field gets when a caller
// logs one: no token survives and the neighbouring fields do.
func TestSecretsQuotedBody(t *testing.T) {
	for _, c := range []struct{ name, body, keep string }{
		{"snake_case", `{"access_token":"FAKEa1","refresh_token":"FAKEr1","id_token":"FAKEi1","client_secret":"FAKEc1","expires_in":3600}`, `\"expires_in\":3600`},
		{"camelCase", `{"accessToken":"FAKEa1","refreshToken":"FAKEr1","idToken":"FAKEi1","clientSecret":"FAKEc1","expiresIn":3600}`, `\"expiresIn\":3600`},
		{"bearer", `{"auth":"Bearer FAKEa1","user":"U1"}`, `\"user\":\"U1\"`},
		{"slack", `{"auth":"xoxb-FAKE-a1","user":"U1"}`, `\"user\":\"U1\"`},
	} {
		got := secrets(fmt.Sprintf("denied %q", c.body))
		if strings.Contains(got, "FAKE") || !strings.Contains(got, c.keep) {
			t.Errorf("%s: secrets(%%q of body) = %s; want every FAKE token gone and %s kept", c.name, got, c.keep)
		}
	}
}

// TestSecretsKnownLimits pins what secrets documents it does not
// catch. Each leak is deliberate: when one gets fixed, flip its case to a
// redaction test and update the doc on secrets.
func TestSecretsKnownLimits(t *testing.T) {
	for _, c := range []struct{ name, in, leak string }{
		{"object value", `{"access_token":{"value":"FAKE"}}`, "FAKE"},
		{"array value", `{"access_token":["FAKE"]}`, "FAKE"},
		{"escaped quote inside a value", `{"access_token":"FA\"KE"}`, `KE`},
		{"url-encoded json", `%7B%22access_token%22%3A%22FAKE%22%7D`, "FAKE"},
		{"html-encoded json", `{&quot;access_token&quot;:&quot;FAKE&quot;}`, "FAKE"},
		{"escape inside a credential", `Bearer FA\u200bKE`, "KE"},
		{"password", `password=FAKE {"password":"FAKE"}`, "FAKE"},
		{"fullwidth scheme", "\uff22earer FAKE", "FAKE"},
		{"spaced equals with a non-empty value after it", "client_secret= \"FAKE\"", "FAKE"},
	} {
		if got := secrets(c.in); !strings.Contains(got, c.leak) {
			t.Errorf("%s: secrets(%q) = %q; the documented limit no longer leaks %q", c.name, c.in, got, c.leak)
		}
	}
}

// Tokens that give themselves away by a prefix are taken with no name before them, in the shape of the real token and
// not a character fewer; a word that only begins like one is kept.
func TestSecretsTokensWithAPrefix(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	k := func(n int) string { return strings.Repeat("aB3", n)[:n] } // letters of both cases and digits, as a key has
	for _, c := range []struct{ name, in, want string }{
		{"GitHub personal", "ghp_" + a(36), redacted},
		{"GitHub OAuth", "gho_" + a(36), redacted},
		{"GitHub user", "ghu_" + a(36), redacted},
		{"GitHub server", "ghs_" + a(36), redacted},
		{"GitHub refresh", "ghr_" + a(36), redacted},
		{"GitHub, longer than today's", "ghp_" + a(40), redacted},
		{"GitHub, ended by a full stop", "ghp_" + a(36) + ".", redacted + "."},
		{"GitHub, ended by an underscore", "ghp_" + a(36) + "_x", redacted + "_x"},
		{"GitHub, one short", "ghp_" + a(35), "ghp_" + a(35)},
		{"GitHub, with an underscore in it", "ghs_" + a(20) + "_" + a(20), "ghs_" + a(20) + "_" + a(20)},
		{"GitHub, an unknown letter", "ghx_" + a(36), "ghx_" + a(36)},
		{"GitHub fine-grained", "github_pat_" + a(22) + "_" + a(59), redacted},
		{"GitHub fine-grained, longer", "github_pat_" + a(22) + "_" + a(70), redacted},
		{"GitHub fine-grained, first part one short", "github_pat_" + a(21) + "_" + a(59), "github_pat_" + a(21) + "_" + a(59)},
		{"GitHub fine-grained, second part one short", "github_pat_" + a(22) + "_" + a(58), "github_pat_" + a(22) + "_" + a(58)},
		{"GitHub fine-grained, no second part", "github_pat_" + a(40), "github_pat_" + a(40)},
		{"GitLab", "glpat-" + a(20), redacted},
		{"GitLab, one short", "glpat-" + a(19), "glpat-" + a(19)},
		{"OpenAI legacy", "sk-" + k(48), redacted},
		{"OpenAI legacy, shortest taken", "sk-" + k(32), redacted},
		{"OpenAI legacy, 31 characters", "sk-" + k(31), redacted},
		{"OpenAI legacy, shortest taken, 20", "sk-" + k(20), redacted},
		{"OpenAI legacy, one short", "sk-" + k(19), "sk-" + k(19)},
		{"a LiteLLM virtual key", "sk-6Yq3x5dM0LzTyb1XkQwR2A", redacted},
		{"a LiteLLM virtual key in a sentence", "key sk-6Yq3x5dM0LzTyb1XkQwR2A here", "key " + redacted + " here"},
		{"a short key with a hyphen and an underscore", "sk-6Yq_x5dM-LzTyb1XkQ-R2A", redacted},
		{"a key of capitals, hyphens and underscores", "sk-aB3_dE5-fG7_hI9-jK1_lM2-nO4_pQ6-rS8", redacted},
		{"a lower-case key with a digit and a long run", "sk-3f9a2b7c1d4e8f0a6b5c9d2e7f1a3b8c", redacted},
		{"a lower-case run of 31 with no digit", "sk-" + a(31), "sk-" + a(31)},
		// Recorded over-redaction: a Title-Case slug has capitals, as a key does.
		{"a Title-Case slug", "sk-Learn-Tutorial-For-Beginners-Part-Two", redacted},
		{"OpenAI legacy, no digit or capital in it", "sk-" + a(48), "sk-" + a(48)},
		{"OpenAI legacy, only a digit", "sk-" + a(47) + "7", redacted},
		{"OpenAI legacy, only a capital", "sk-" + a(47) + "Q", redacted},
		{"OpenAI project", "sk-proj-" + k(100), redacted},
		{"OpenAI project, with hyphens and underscores", "sk-proj-Ab_cd-" + k(60), redacted},
		{"OpenAI project, shortest taken", "sk-proj-" + k(20), redacted},
		{"OpenAI project, one short, lower case (the bare rule takes a mixed one)", "sk-proj-" + a(19), "sk-proj-" + a(19)},
		{"OpenAI project, no digit or capital in it", "sk-proj-" + a(60), "sk-proj-" + a(60)},
		{"OpenAI service account", "sk-svcacct-" + k(100), redacted},
		{"OpenAI admin", "sk-admin-" + k(100), redacted},
		{"OpenRouter", "sk-or-v1-" + strings.Repeat("0a1b2c3d", 8), redacted},
		{"Anthropic", "sk-ant-api03-" + k(90), redacted},
		{"sk- and a hyphenated run of words and a year", "sk-learn-tutorial-for-beginners-2024", "sk-learn-tutorial-for-beginners-2024"},
		{"Google", "AIza" + a(35), redacted},
		{"Google, longer", "AIza" + a(36), redacted},
		{"Google, one short", "AIza" + a(34), "AIza" + a(34)},
		{"Stripe live secret", "sk_live_" + a(24), redacted},
		{"Stripe live restricted", "rk_live_" + a(24), redacted},
		{"Stripe test secret", "sk_test_" + a(24), redacted},
		{"Stripe test restricted", "rk_test_" + a(24), redacted},
		{"Stripe, longer", "sk_live_" + a(99), redacted},
		{"Stripe, one short", "sk_live_" + a(23), "sk_live_" + a(23)},
		{"Stripe, publishable", "pk_live_" + a(24), "pk_live_" + a(24)},
		{"in an assignment", "OPENAI_API_KEY=sk-proj-" + k(40) + " ok", "OPENAI_API_KEY=" + redacted + " ok"},
		{"in a sentence, before a full stop", "use ghp_" + a(36) + ". Then", "use " + redacted + ". Then"},
		{"in JSON", `{"k":"AIza` + a(35) + `","u":"U1"}`, `{"k":"` + redacted + `","u":"U1"}`},
		{"two", "sk-" + k(40) + " " + "glpat-" + a(20), redacted + " " + redacted},
		// Words that begin like a token, and the end of a longer word.
		{"sk-learn", "pip install sk-learn", "pip install sk-learn"},
		{"risk-free", "a risk-free trade", "a risk-free trade"},
		{"ask-me", "ask-me anything", "ask-me anything"},
		{"a task ID", "task-0123456789abcdefghijklmnopqrstuvwxyz1", "task-0123456789abcdefghijklmnopqrstuvwxyz1"},
		{"a disk name", "disk-usage-0123456789abcdefghijklmnop", "disk-usage-0123456789abcdefghijklmnop"},
		{"a hyphenated name that begins sk- and is long", "team-sk-payments-alerts-production-east", "team-sk-payments-alerts-production-east"},
		{"a pod name that begins sk-", "sk-payments-api-7c9d8f6b5-x2k4p", "sk-payments-api-7c9d8f6b5-x2k4p"},
		{"a namespace that begins sk-", "sk-platform-observability-prod", "sk-platform-observability-prod"},
		{"AIza with too little", "AIzawhatever", "AIzawhatever"},
		{"prefixes alone", "ghp_ gho_ github_pat_ glpat- sk-proj- sk_live_ rk_test_", "ghp_ gho_ github_pat_ glpat- sk-proj- sk_live_ rk_test_"},
		{"a name that ends like a Stripe key", "network_live_connections_established", "network_live_connections_established"},
		{"snake-case that ends like a GitHub prefix", "highs_and_lows_metric_value_total", "highs_and_lows_metric_value_total"},
		{"snake-case that ends like a Stripe prefix", "network_test_connectivityToService", "network_test_connectivityToService"},
		{"snake-case that ends like a Stripe prefix, with a capital", "risk_test_suiteNumberOneTwoThree", "risk_test_suiteNumberOneTwoThree"},
		// Azure SAS: the value of sig, from 16 characters, and not what only has the name.
		{"SAS signature", "https://acct.blob.core.windows.net/c?sv=2020-08-04&sig=abcdEFGH1234%2Bsecret%2Fvalue%3D&se=2030", "https://acct.blob.core.windows.net/c?sv=2020-08-04&sig=" + redacted + "&se=2030"},
		{"SAS signature with its padding", "sig=" + a(43) + "=", "sig=" + redacted},
		{"SAS signature, shortest taken", "?sig=" + a(16), "?sig=" + redacted},
		{"SAS signature, one short", "?sig=" + a(15), "?sig=" + a(15)},
		{"sig= in prose", "the sig= field is empty", "the sig= field is empty"},
		{"sig= ending a word (no word boundary: the value is taken)", "mysig=" + a(30), "mysig=" + redacted},
	} {
		if got := secrets(c.in); got != c.want {
			t.Errorf("%s: secrets(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if got := IdentifierText(c.in); got != c.want {
			t.Errorf("%s: IdentifierText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
