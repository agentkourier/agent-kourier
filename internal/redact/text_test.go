package redact

import (
	"strings"
	"testing"
)

// textShapes has one case per shape of secret in free text. Each says what must not survive Text,
// and a neighbour that is no secret and must.
var textShapes = []struct {
	name    string
	in      string
	secret  string
	visible string
}{
	{"flag with =", "psql --password=SECRETVAL -h VISIBLE", "SECRETVAL", "VISIBLE"},
	{"environment assignment", "PGPASSWORD=SECRETVAL psql -h VISIBLE", "SECRETVAL", "VISIBLE"},
	{"connection string", "host=VISIBLE user=app password=SECRETVAL dbname=x", "SECRETVAL", "VISIBLE"},
	{"JSON", `{"password":"SECRETVAL","user":"VISIBLE"}`, "SECRETVAL", "VISIBLE"},
	{"JSON inside a string", `{\"password\":\"SECRETVAL\",\"user\":\"VISIBLE\"}`, "SECRETVAL", "VISIBLE"},
	{"hash rocket", "{'password' => 'SECRETVAL', 'user' => 'VISIBLE'}", "SECRETVAL", "VISIBLE"},
	{"a secret word as the value of another pair", "x=password=SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"authorization with a scheme", "curl -H 'Authorization: Token SECRETVAL' VISIBLE", "SECRETVAL", "VISIBLE"},
	{"cookie pairs", "Cookie: sid=SECRETVAL; csrf=SECRETTWO", "SECRETTWO", "Cookie"},
	{"PEM private key", "VISIBLE -----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEASECRETVAL\n-----END RSA PRIVATE KEY-----", "SECRETVAL", "VISIBLE"},
	{"PEM key cut before its end", "VISIBLE -----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAASECRETVAL", "SECRETVAL", "VISIBLE"},
	{"URL userinfo", "https://adm:SECRETVAL@host/VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a space in a URL password", "VISIBLE@x https://adm:pa SECRETVAL@host/x", "SECRETVAL", "VISIBLE@x"},
	{"schemeless user and password", "root:SECRETVAL@10.0.0.1:5432/VISIBLE", "SECRETVAL", "VISIBLE"},
	{"schemeless with an @ in the password", "root:SEC@RETVAL@host VISIBLE", "RETVAL", "VISIBLE"},
	{"AWS secret key by shape", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYSECRETVAL1 VISIBLE", "SECRETVAL1", "VISIBLE"},
	{"pair after a newline", "VISIBLE: 1\nkey: SECRETVAL", "SECRETVAL", "VISIBLE"},
	{"pair after a tab", "HOST=VISIBLE\ttoken=SECRETVAL", "SECRETVAL", "VISIBLE"},
	{"zero-width in a pair name", "VISIBLE pass\u200bword=SECRETVAL", "SECRETVAL", "VISIBLE"},
	{"secret flag then its value", "tool --token SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"secret flag with one dash", "tool -password SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a flag then another flag", "tool --token --verbose VISIBLE", "", "--verbose"},
	{"curl -u", "curl -u admin:SECRETVAL https://VISIBLE", "SECRETVAL", "admin"},
	{"curl --user=", "curl --user=admin:SECRETVAL https://VISIBLE", "SECRETVAL", "admin"},
	{"curl -u attached", "curl -uadmin:SECRETVAL https://VISIBLE", "SECRETVAL", "admin"},
	{"a user without a password", "sudo -u postgres psql VISIBLE", "", "postgres"},
	{"a quote that never closes", `VISIBLE password="SECRETVAL and more`, "SECRETVAL", "VISIBLE"},
	{"a planted marker", "VISIBLE password=[redacted]SECRETVAL", "SECRETVAL", "VISIBLE"},
	{"a secret pair inside a secret value", "VISIBLE password=token=SECRETVAL", "SECRETVAL", "VISIBLE"},
	{"a Slack token, by the layer underneath", "VISIBLE xoxb-1234-SECRETVAL", "SECRETVAL", "VISIBLE"},
	{"a bearer token, by the layer underneath", "VISIBLE Bearer SECRETVALUE12", "SECRETVALUE12", "VISIBLE"},
}

func TestTextRedactsEveryShape(t *testing.T) {
	for _, tc := range textShapes {
		got := Text(tc.in)
		if tc.secret != "" && strings.Contains(got, tc.secret) {
			t.Errorf("%s: %q shows %q", tc.name, got, tc.secret)
		}
		if !strings.Contains(got, tc.visible) {
			t.Errorf("%s: %q lost %q, which is no secret", tc.name, got, tc.visible)
		}
	}
}

// What only looks like a secret is kept: a commit hash, an email, a time, plain words.
func TestTextKeepsWhatIsNoSecret(t *testing.T) {
	in := "sha 0123456789abcdef0123456789abcdef01234567 mail ada@example.test at 12:30 on monday, the keyboard is monkey-proof"
	if got := Text(in); got != in {
		t.Errorf("Text(%q) = %q; want it unchanged", in, got)
	}
}

func TestSecretName(t *testing.T) {
	for name, want := range map[string]bool{
		"password": true, "Password": true, "db_password": true, "dbPassword": true, "DB_PASSWORD": true, "PGPASSWORD": true,
		"api-key": true, "apiKey": true, "APIKey": true, "DBAuth": true, "myKey": true, "key2": true, "x-auth-token": true,
		"AWS_SECRET_ACCESS_KEY": true, "sessionid": true, "connectionString": true, "connection_string": true, "dsn": true, "pwd": true,
		"monkey": false, "author": false, "keyboard": false, "max_tokens": false, "passed": false, "bypass": false, "name": false, "": false,
	} {
		if got := SecretName(name); got != want {
			t.Errorf("SecretName(%q) = %t, want %t", name, got, want)
		}
	}
}

// Text deletes format characters itself, so a caller that did not clean first is as safe.
func TestTextDeletesFormatCharacters(t *testing.T) {
	if got := Text("a\u200bb\u202ec password\u200b=\u200bSECRETVAL"); strings.Contains(got, "SECRETVAL") || strings.ContainsAny(got, "\u200b\u202e") {
		t.Errorf("Text = %q; want the format characters gone and the value redacted", got)
	}
}

// Text cuts its input at a word boundary before it redacts, so the work is bounded and no part of a
// secret that straddles the cut survives.
func TestTextIsBounded(t *testing.T) {
	if got := Text(strings.Repeat("a=", 512<<10)); len(got) > MaxBytes+len(Truncated) {
		t.Errorf("a 1 MB value kept %d bytes; want at most %d", len(got), MaxBytes+len(Truncated))
	}
	if got := Text("short value"); got != "short value" {
		t.Errorf("a short value became %q", got)
	}
	straddle := strings.Repeat("x ", (MaxBytes-12)/2) + "password=SECRETVALUE tail"
	if got := Text(straddle); strings.Contains(got, "SEC") || strings.Contains(got, "word=") || !strings.HasSuffix(got, Truncated) {
		t.Errorf("a secret across the limit left %q at the end", got[len(got)-40:])
	}
	if got := Text(strings.Repeat("y", 2*MaxBytes)); strings.Contains(got, "y") {
		t.Errorf("an unbroken run of %d bytes kept some of itself", 2*MaxBytes)
	}
	if got := Bound("short"); got != "short" {
		t.Errorf("Bound(short) = %q", got)
	}
}

// Redacting twice is redacting once, so a field that passes through two layers is not changed by the second.
func TestTextIsIdempotent(t *testing.T) {
	for _, tc := range textShapes {
		once := Text(tc.in)
		if twice := Text(once); twice != once {
			t.Errorf("%s: Text(Text(x)) = %q; Text(x) = %q", tc.name, twice, once)
		}
	}
}

// FuzzTextNeverShowsAValue: whatever follows a secret name, in any of the shapes, is not shown.
func FuzzTextNeverShowsAValue(f *testing.F) {
	for _, s := range []string{"hunter2hunter2", "Zq9\\nxx", "a b", "\"q\"", "x=y", "\u200b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		var b strings.Builder // one word of letters and digits, long enough to be no accident
		for _, r := range v {
			if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		secret := "Zx7" + b.String() + "Qq9"
		for _, in := range []string{
			"password=" + secret, "--token " + secret, `{"api_key":"` + secret + `"}`, "https://u:" + secret + "@host/", "PGPASSWORD=" + secret + " psql",
			"Cookie: sid=" + secret, "curl -u admin:" + secret + " http://h",
		} {
			if got := Text(in); strings.Contains(got, secret) {
				t.Errorf("Text(%q) = %q; want %q gone", in, got, secret)
			}
		}
	})
}
