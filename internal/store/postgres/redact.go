package postgres

import (
	"cmp"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// redacted replaces a secret in an error's text.
const redacted = "[redacted]"

// redactor removes a DSN's secrets from error text: the DSN itself and the passwords in it, in the
// forms a driver or the standard library may print them (a parse error of net/url quotes the whole
// URL, pgx prints what it was given). A secret is replaced wherever it appears, so a password that is
// a very short string garbles the text around it; that is the price of never printing it.
type redactor struct{ secrets []string }

// newRedactor collects the secrets in dsn, which may be a URL or a key/value string.
func newRedactor(dsn string) redactor {
	return redactor{}.with(dsn).with(dsnPasswords(dsn)...)
}

// with returns r that also removes secrets. It adds the escaped forms of each.
func (r redactor) with(secrets ...string) redactor {
	out := redactor{secrets: append([]string(nil), r.secrets...)}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		out.secrets = append(out.secrets, s, url.QueryEscape(s), url.PathEscape(s))
	}
	return out
}

// scrub returns err unchanged if its text holds no secret, so that errors.Is and errors.As keep working
// on the common error. If it does, the result prints without the secret, and still matches errors.Is
// (context.Canceled, a store error) but not errors.As, and cannot be unwrapped: the unredacted error
// is not reachable from it.
func (r redactor) scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := r.text(msg)
	if clean == msg {
		return err
	}
	return &redactedError{msg: clean, err: err}
}

// text replaces every secret in s, the longest first so that a password inside the DSN does not leave
// the rest of the DSN behind.
func (r redactor) text(s string) string {
	secrets := slices.Clone(r.secrets)
	slices.SortStableFunc(secrets, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

// redactedError is an error whose text has been scrubbed. It keeps the original out of reach (no
// Unwrap), and answers errors.Is for it, so that a caller can still tell a cancelled context.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

// Is reports whether the original error is target.
func (e *redactedError) Is(target error) bool { return errors.Is(e.err, target) }

// configSecrets returns the password of a parsed config, in the form the driver holds it: decoded, and
// from wherever it came (the DSN, PGPASSWORD or a password file).
func configSecrets(cfg *pgxpool.Config) []string {
	return []string{cfg.ConnConfig.Password}
}

// kvPassword matches password= and sslpassword= in a key/value DSN, with a quoted or a bare value.
var kvPassword = regexp.MustCompile(`(?i)\b(?:ssl)?password\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// dsnPasswords returns the passwords in a DSN of either form, without parsing it the way the driver
// does: the DSN may be one the driver rejects, which is when its error text matters most.
func dsnPasswords(dsn string) []string {
	var out []string
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		if rest, ok := strings.CutPrefix(dsn, scheme); ok {
			out = append(out, urlPasswords(rest)...)
		}
	}
	for _, m := range kvPassword.FindAllStringSubmatch(dsn, -1) {
		v := m[1]
		if strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") && len(v) >= 2 {
			v = strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(v[1 : len(v)-1])
		}
		out = append(out, v)
	}
	return out
}

// urlPasswords returns the passwords of the userinfo and the password-bearing query parameters of the
// URL that follows the scheme. The userinfo is read two ways: as net/url does, up to the first slash,
// and up to the last "@" before the query, which is where a password with an unescaped "@" or "/" ends.
// A guess that is too long only redacts more.
func urlPasswords(rest string) []string {
	var out []string
	addPassword := func(userinfo string) {
		if _, pw, ok := strings.Cut(userinfo, ":"); ok {
			out = append(out, pw)
			if dec, err := url.PathUnescape(pw); err == nil {
				out = append(out, dec)
			}
		}
	}
	beforeQuery, query, _ := strings.Cut(rest, "?")
	authority, _, _ := strings.Cut(beforeQuery, "/") // the path is the database name
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		addPassword(authority[:at])
	}
	if at := strings.LastIndex(beforeQuery, "@"); at >= 0 {
		addPassword(beforeQuery[:at])
	}
	for _, pair := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(pair, "=")
		if k = strings.ToLower(k); k == "password" || k == "sslpassword" {
			out = append(out, v)
			if dec, err := url.QueryUnescape(v); err == nil {
				out = append(out, dec)
			}
		}
	}
	return out
}

// isURL reports whether dsn is in the URL form.
func isURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}
