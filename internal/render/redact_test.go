package render

import (
	"strings"
	"testing"
)

// secretShapes has one case per shape of secret the display redaction knows. Each says what must
// not appear in a tool line, and a neighbour that is no secret and must stay.
var secretShapes = []struct {
	name    string
	args    map[string]any
	secret  string // must not appear
	visible string // must still appear
}{
	{"camel-case key", map[string]any{"dbApiKey": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"acronym then word", map[string]any{"APIKey": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"snake-case key", map[string]any{"db_password": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"kebab-case key", map[string]any{"x-api-key": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"one word ending in a secret word", map[string]any{"PGPASSWORD": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"key words are whole words", map[string]any{"monkey": "VISIBLE1", "author": "VISIBLE2", "max_tokens": "VISIBLE3", "keyboard": "VISIBLE4"}, "[redacted]", "VISIBLE4"},
	{"k8s env pair", map[string]any{"env": []any{map[string]any{"name": "DB_PASSWORD", "value": "SECRETVAL"}, map[string]any{"name": "LOG_LEVEL", "value": "VISIBLE"}}}, "SECRETVAL", "VISIBLE"},
	{"k8s pair with a key sibling", map[string]any{"e": map[string]any{"key": "api_key", "value": "SECRETVAL", "other": "VISIBLE"}}, "SECRETVAL", "VISIBLE"},
	{"flag in a command", map[string]any{"cmd": "psql --password=SECRETVAL -h VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"environment assignment", map[string]any{"cmd": "PGPASSWORD=SECRETVAL psql -h VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"connection string", map[string]any{"dsn": "x", "c": "host=VISIBLE user=app password=SECRETVAL dbname=x"}, "SECRETVAL", "VISIBLE"},
	{"JSON inside a string", map[string]any{"body": `{"password":"SECRETVAL","user":"VISIBLE"}`}, "SECRETVAL", "VISIBLE"},
	{"a secret word as the value of another pair", map[string]any{"cmd": "x=password=SECRETVAL VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"authorization with a scheme", map[string]any{"cmd": "curl -H 'Authorization: Token SECRETVAL' VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"cookie pairs", map[string]any{"cmd": "Cookie: sid=SECRETVAL; csrf=SECRETTWO"}, "SECRETTWO", "Calling"},
	{"PEM private key", map[string]any{"in": "VISIBLE -----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEASECRETVAL\n-----END RSA PRIVATE KEY-----"}, "SECRETVAL", "VISIBLE"},
	{"PEM key cut before its end", map[string]any{"in": "VISIBLE -----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAASECRETVAL"}, "SECRETVAL", "VISIBLE"},
	{"schemeless user and password", map[string]any{"target": "root:SECRETVAL@10.0.0.1:5432/VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"schemeless with an @ in the password", map[string]any{"target": "root:SEC@RETVAL@host VISIBLE"}, "RETVAL", "VISIBLE"},
	{"AWS secret key by shape", map[string]any{"x": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYSECRETVAL1 VISIBLE"}, "SECRETVAL1", "VISIBLE"},
}

func TestSecretShapesAreRedactedInToolLines(t *testing.T) {
	for _, tc := range secretShapes {
		got := ToolLine("run", tc.args, FlavourText)
		if strings.Contains(got, tc.secret) {
			t.Errorf("%s: %q shows %q", tc.name, got, tc.secret)
		}
		if !strings.Contains(got, tc.visible) {
			t.Errorf("%s: %q lost %q, which is no secret", tc.name, got, tc.visible)
		}
	}
}

// What only looks like a secret is shown: a 40-hex commit hash, an email, a time and a plain word.
func TestWhatIsNoSecretIsShown(t *testing.T) {
	args := map[string]any{
		"sha": "0123456789abcdef0123456789abcdef01234567", "mail": "ada@example.test", "at": "12:30 on monday", "note": "the keyboard is monkey-proof",
	}
	got := ToolLine("run", args, FlavourText)
	for _, keep := range []string{"0123456789abcdef0123456789abcdef01234567", "ada@example.test", "12:30 on monday", "keyboard is monkey-proof"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q lost %q", got, keep)
		}
	}
}

func TestSecretWords(t *testing.T) {
	for name, want := range map[string]bool{
		"password": true, "Password": true, "db_password": true, "dbPassword": true, "DB_PASSWORD": true, "PGPASSWORD": true,
		"api-key": true, "apiKey": true, "APIKey": true, "x-auth-token": true, "Authorization": true, "AWS_SECRET_ACCESS_KEY": true,
		"session_id": true, "jwt": true, "signature": true, "dsn": true, "bearer": true, "pwd": true, "credentials": true,
		"monkey": false, "author": false, "keyboard": false, "max_tokens": false, "passed": false, "bypass": false, "name": false, "": false,
	} {
		if got := isSecretName(name); got != want {
			t.Errorf("isSecretName(%q) = %t, want %t", name, got, want)
		}
	}
}
