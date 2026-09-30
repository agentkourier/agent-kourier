package store

import "testing"

func TestRedactSecrets(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"slack bot token", "auth failed for xoxb-FAKE-1234-abcd today", "auth failed for [redacted] today"},
		{"slack user token", "xoxp-FAKE-token", "[redacted]"},
		{"slack app token", "socket xapp-1-FAKE-abcd failed", "socket [redacted] failed"},
		{"bearer header", "sent Authorization: Bearer FAKE.token.value", "sent Authorization: [redacted]"},
		{"lowercase bearer", "bearer FAKE", "[redacted]"},
		{"jwt", "token eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0. rejected", "token [redacted] rejected"},
		{"several", "xoxb-A and xapp-B", "[redacted] and [redacted]"},
		{"clean", "approved by U123 for interaction i-42", "approved by U123 for interaction i-42"},
		{"empty", "", ""},
	} {
		if got := RedactSecrets(c.in); got != c.want {
			t.Errorf("%s: RedactSecrets(%q) = %q; want %q", c.name, c.in, got, c.want)
		}
	}
}
