package slack_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// TestTokenRejected pins the set of codes with which Slack rejects a token for good. The match is exact:
// a caller that reads a code with more text after it (a first word) cuts it first.
func TestTokenRejected(t *testing.T) {
	for code, want := range map[string]bool{
		"invalid_auth": true, "token_revoked": true, "token_expired": true, "account_inactive": true, "not_authed": true,
		"": false, "invalid_auth extra": false, "INVALID_AUTH": false,
		"ratelimited": false, "internal_error": false, "service_unavailable": false, // a try later may work
		"missing_scope": false, "channel_not_found": false, "msg_too_long": false, // not about the token's validity
	} {
		if got := slack.TokenRejected(code); got != want {
			t.Errorf("TokenRejected(%q) = %v, want %v", code, got, want)
		}
	}
}
