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
// category reports and checks each one is a legal gap between a scheme and its
// credential, so a new Unicode version cannot open a hole.
func TestRedactSecretsAnyGapRune(t *testing.T) {
	credentials := map[string]string{"Bearer": "FAKETOKEN", "Basic": "dXNlcjpwYXNzd29yZA=="}
	n := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) {
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
