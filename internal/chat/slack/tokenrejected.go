package slack

// TokenRejected reports whether code is one with which Slack rejects a token for good: invalid_auth, token_revoked,
// token_expired, account_inactive and not_authed. No retry mends it until the token changes. The match is exact; a
// caller whose text carries more than the code cuts the code out first.
func TokenRejected(code string) bool {
	switch code {
	case "invalid_auth", "token_revoked", "token_expired", "account_inactive", "not_authed":
		return true
	}
	return false
}
