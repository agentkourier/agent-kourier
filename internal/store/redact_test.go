package store

import (
	"fmt"
	"strings"
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
			// Go's quoting turns an invisible rune into a literal escape.
			for _, verb := range []string{"%q", "%+q"} {
				if got, want := RedactSecrets(fmt.Sprintf(verb, in)), `"x [redacted] y"`; got != want {
					t.Errorf("RedactSecrets(%s of %q) = %s; want %s (U+%04X)", verb, in, got, want, r)
				}
			}
		}
		// The same runes may pad an OAuth key from its colon and its value.
		g := string(r)
		in := "x access_token" + g + ":" + g + "FAKETOKEN y"
		if got, want := RedactSecrets(in), "x access_token"+g+":"+g+"[redacted] y"; got != want {
			t.Errorf("RedactSecrets(%q) = %q; want %q (U+%04X)", in, got, want, r)
		}
	}
	if n < 100 {
		t.Fatalf("swept only %d gap runes; the sweep is broken", n)
	}
}

// TestRedactSecretsQuotedBody feeds RedactSecrets what %q prints for a
// token-endpoint body, which is the form an audit field gets when a caller
// logs one: no token survives and the neighbouring fields do.
func TestRedactSecretsQuotedBody(t *testing.T) {
	for _, c := range []struct{ name, body, keep string }{
		{"snake_case", `{"access_token":"FAKEa1","refresh_token":"FAKEr1","id_token":"FAKEi1","client_secret":"FAKEc1","expires_in":3600}`, `\"expires_in\":3600`},
		{"camelCase", `{"accessToken":"FAKEa1","refreshToken":"FAKEr1","idToken":"FAKEi1","clientSecret":"FAKEc1","expiresIn":3600}`, `\"expiresIn\":3600`},
		{"bearer", `{"auth":"Bearer FAKEa1","user":"U1"}`, `\"user\":\"U1\"`},
		{"slack", `{"auth":"xoxb-FAKE-a1","user":"U1"}`, `\"user\":\"U1\"`},
	} {
		got := RedactSecrets(fmt.Sprintf("denied %q", c.body))
		if strings.Contains(got, "FAKE") || !strings.Contains(got, c.keep) {
			t.Errorf("%s: RedactSecrets(%%q of body) = %s; want every FAKE token gone and %s kept", c.name, got, c.keep)
		}
	}
}

// TestRedactSecretsKnownLimits pins what RedactSecrets documents it does not
// catch. Each leak is deliberate: when one gets fixed, flip its case to a
// redaction test and update the doc on RedactSecrets.
func TestRedactSecretsKnownLimits(t *testing.T) {
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
		if got := RedactSecrets(c.in); !strings.Contains(got, c.leak) {
			t.Errorf("%s: RedactSecrets(%q) = %q; the documented limit no longer leaks %q", c.name, c.in, got, c.leak)
		}
	}
}
