package store

import (
	"testing"
	"unicode"
)

func TestRedactSecrets(t *testing.T) {
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
		got := RedactSecrets(c.in)
		if got != c.want {
			t.Errorf("%s: RedactSecrets(%q) = %q; want %q", c.name, c.in, got, c.want)
		}
		if again := RedactSecrets(got); again != got {
			t.Errorf("%s: not idempotent: %q then %q", c.name, got, again)
		}
	}
}

// TestRedactSecretsAnyGapRune sweeps every rune that unicode.IsSpace or the Cf
// or Cc category reports and checks each one is a legal gap between a scheme and its
// credential, so a new Unicode version cannot open a hole.
func TestRedactSecretsAnyGapRune(t *testing.T) {
	credentials := map[string]string{"Bearer": "FAKETOKEN", "Basic": "dXNlcjpwYXNzd29yZA=="}
	n := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Cc, r) {
			continue
		}
		n++
		for scheme, token := range credentials {
			in := "x " + scheme + string(r) + token + " y"
			if got := RedactSecrets(in); got != "x [redacted] y" {
				t.Errorf("RedactSecrets(%q) = %q; want the credential redacted (U+%04X)", in, got, r)
			}
		}
	}
	if n < 100 {
		t.Fatalf("swept only %d gap runes; the sweep is broken", n)
	}
}
