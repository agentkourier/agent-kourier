package store

import "regexp"

// secretPattern matches Slack bot/user/app tokens, bearer credentials, and JWTs.
// It is compiled once and never modified.
var secretPattern = regexp.MustCompile(`xox[a-z]-\S+|xapp-\S+|(?i:bearer)\s+\S+|eyJ[\w-]+\.[\w-]+\.[\w-]*`)

// RedactSecrets replaces anything in s that looks like a Slack token, a bearer
// credential, or a JWT with "[redacted]". It is a backstop for free-text audit
// fields, not a license to put secrets in them.
func RedactSecrets(s string) string {
	return secretPattern.ReplaceAllString(s, "[redacted]")
}
