package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// textShapes has one case per shape of secret in free text. Each says what must not survive FreeText,
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
	// A space of any width around the separator, after a flag, or ending a value.
	{"a no-break space around an equals sign", "password\u00a0=\u00a0SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a narrow no-break space around a colon", "password\u202f:\u202fSECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a no-break space around the equals sign of an environment name", "api_key\u00a0=\u00a0SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a no-break space before the colon of JSON", "{\"password\"\u00a0: \"SECRETVAL\", \"user\": \"VISIBLE\"}", "SECRETVAL", "VISIBLE"},
	{"a thin space before a colon", "password\u2009: SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"an ideographic space around an equals sign", "password\u3000=\u3000SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a no-break space after a flag", "tool --password\u00a0SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a no-break space before a flag", "tool\u00a0--password SECRETVAL VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a no-break space after curl -u", "curl -u\u00a0admin:SECRETVAL https://VISIBLE", "SECRETVAL", "admin"},
	{"a no-break space after curl --user", "curl --user\u00a0admin:SECRETVAL https://VISIBLE", "SECRETVAL", "admin"},
	{"a no-break space that ends a value", "password=SECRETVAL\u00a0VISIBLE", "SECRETVAL", "VISIBLE"},
	{"an ideographic space that ends a cookie pair", "Cookie: sid=SECRETVAL;\u3000csrf=SECRETTWO", "SECRETTWO", "Cookie"},
	// Kubernetes and shell forms, with a value on a line of its own.
	{"an env entry", "- name: DB_PASSWORD\n  value: SECRETVAL\n- name: VISIBLE", "SECRETVAL", "VISIBLE"},
	{"an env entry, quoted", "- name: \"API_TOKEN\"\n  value: \"SECRETVAL\"\n- name: VISIBLE", "SECRETVAL", "VISIBLE"},
	{"a block scalar", "password: |\n  SECRETVAL\n  SECRETTWO\nnext: VISIBLE", "SECRETTWO", "VISIBLE"},
	{"a flag whose value follows a trailing backslash", "mysql --password \\\n  SECRETVAL \\\n  --VISIBLE", "SECRETVAL", "VISIBLE"},
}

func TestFreeTextRedactsEveryShape(t *testing.T) {
	for _, tc := range textShapes {
		got := FreeText(tc.in)
		if tc.secret != "" && strings.Contains(got, tc.secret) {
			t.Errorf("%s: %q shows %q", tc.name, got, tc.secret)
		}
		if !strings.Contains(got, tc.visible) {
			t.Errorf("%s: %q lost %q, which is no secret", tc.name, got, tc.visible)
		}
	}
}

// What only looks like a secret is kept: a commit hash, an email, a time, plain words.
func TestFreeTextKeepsWhatIsNoSecret(t *testing.T) {
	in := "sha 0123456789abcdef0123456789abcdef01234567 mail ada@example.test at 12:30 on monday, the keyboard is monkey-proof"
	if got := FreeText(in); got != in {
		t.Errorf("FreeText(%q) = %q; want it unchanged", in, got)
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

// FreeText deletes the invisible characters itself, so a caller that did not clean first is as safe.
func TestFreeTextDeletesFormatCharacters(t *testing.T) {
	if got := FreeText("a\u200bb\u202ec password\u200b=\u200bSECRETVAL"); strings.Contains(got, "SECRETVAL") || strings.ContainsAny(got, "\u200b\u202e") {
		t.Errorf("Text = %q; want the format characters gone and the value redacted", got)
	}
}

// FreeText cuts its input at a word boundary before it redacts, so the work is bounded and no part of a
// secret that straddles the cut survives.
func TestFreeTextIsBounded(t *testing.T) {
	if got := FreeText(strings.Repeat("a=", 512<<10)); len(got) > MaxBytes+len(Truncated) {
		t.Errorf("a 1 MB value kept %d bytes; want at most %d", len(got), MaxBytes+len(Truncated))
	}
	if got := FreeText("short value"); got != "short value" {
		t.Errorf("a short value became %q", got)
	}
	straddle := strings.Repeat("x ", (MaxBytes-12)/2) + "password=SECRETVALUE tail"
	if got := FreeText(straddle); strings.Contains(got, "SEC") || strings.Contains(got, "word=") || !strings.HasSuffix(got, Truncated) {
		t.Errorf("a secret across the limit left %q at the end", got[len(got)-40:])
	}
	if got := FreeText(strings.Repeat("y", 2*MaxBytes)); strings.Contains(got, "y") {
		t.Errorf("an unbroken run of %d bytes kept some of itself", 2*MaxBytes)
	}
	if got := Bound("short"); got != "short" {
		t.Errorf("Bound(short) = %q", got)
	}
}

// Redacting twice is redacting once, so a field that passes through two layers is not changed by the second.
func TestFreeTextIsIdempotent(t *testing.T) {
	for _, tc := range textShapes {
		once := FreeText(tc.in)
		if twice := FreeText(once); twice != once {
			t.Errorf("%s: FreeText(FreeText(x)) = %q; FreeText(x) = %q", tc.name, twice, once)
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
			if got := FreeText(in); strings.Contains(got, secret) {
				t.Errorf("FreeText(%q) = %q; want %q gone", in, got, secret)
			}
		}
	})
}

// The exact result of the rules for Kubernetes env entries, YAML block scalars, a trailing backslash and the space class, and
// of what they leave alone. FreeText is the first of each pair, FreeTextLine, the text with its lines joined, the second.
func TestFreeTextReadsManifestAndShellForms(t *testing.T) {
	for _, tc := range []struct{ name, in, free, line string }{
		{"an env entry", "- name: DB_PASSWORD\n  value: hunter2", "- name: DB_PASSWORD\n  value: [redacted]", "- name: DB_PASSWORD value: [redacted]"},
		{"an env entry, then another", "- name: PGPASSWORD\n  value: hunter2\n- name: LOG\n  value: debug", "- name: PGPASSWORD\n  value: [redacted]\n- name: LOG\n  value: debug", "- name: PGPASSWORD value: [redacted] - name: LOG value: debug"},
		{"an env entry with a line between", "- name: API_TOKEN\n  # by hand\n  value: hunter2", "- name: API_TOKEN\n  # by hand\n  value: [redacted]", "- name: API_TOKEN # by hand value: [redacted]"},
		{"an env entry in JSON", `{"name":"DB_PASSWORD","value":"hunter2"}`, `{"name":"DB_PASSWORD","value":[redacted]}`, `{"name":"DB_PASSWORD","value":[redacted]}`},
		{"an env entry that is no secret", "- name: LOG_LEVEL\n  value: debug", "- name: LOG_LEVEL\n  value: debug", "- name: LOG_LEVEL value: debug"},
		{"an env entry with no value of its own", "- name: DB_PASSWORD\n  valueFrom:\n    fieldRef:\n      fieldPath: metadata.name", "- name: DB_PASSWORD\n  valueFrom:\n    fieldRef:\n      fieldPath: metadata.name", "- name: DB_PASSWORD valueFrom: fieldRef: fieldPath: metadata.name"},
		{"a value three lines after the name", "- name: DB_PASSWORD\n  a: 1\n  b: 2\n  value: hunter2", "- name: DB_PASSWORD\n  a: 1\n  b: 2\n  value: hunter2", "- name: DB_PASSWORD a: 1 b: 2 value: hunter2"},
		{"a name that ends in name", "hostname: DB_PASSWORD\n  value: hunter2", "hostname: DB_PASSWORD\n  value: hunter2", "hostname: DB_PASSWORD value: hunter2"},
		{"a literal block scalar", "password: |\n  hunter2\n  more\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a literal block scalar that strips", "password: |-\n  hunter2\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a folded block scalar", "password: >\n  hunter2\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a folded block scalar that strips, with a blank line", "password: >-\n  hunter2\n\n  more\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a block scalar with an indentation indicator", "password: |2+\n    hunter2\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a block scalar under a list entry", "- token: |\n    hunter2\n- other: x", "- token: [redacted]\n- other: x", "- token: [redacted] - other: x"},
		{"a block scalar with nothing under it", "password: |\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a block scalar under a name that is no secret", "description: |\n  hunter2\nnext: x", "description: |\n  hunter2\nnext: x", "description: | hunter2 next: x"},
		{"a folded block scalar under a name that is no secret", "args: >-\n  hunter2\nnext: x", "args: >-\n  hunter2\nnext: x", "args: >- hunter2 next: x"},
		{"a block header with text after it", "password: | hunter2\n  more\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a block header with text after it, and no line under it", "password: | hunter2\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a block header with a comment after it", "password: | # note\n  hunter2\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a block scalar under a name, and a line indented as much as the name", "  password: |\n    hunter2\n  next: x", "  password: [redacted]\n  next: x", "password: [redacted] next: x"},
		{"a cookie list for an env entry", "- name: COOKIE\n  value: sid=hunter2; csrf=more tail", "- name: COOKIE\n  value: [redacted] tail", "- name: COOKIE value: [redacted] tail"},
		{"a flag's value is no cookie list", "tool --token abc; note=1", "tool --token [redacted] note=1", "tool --token [redacted] note=1"},
		{"a lone backslash and a space", `mysql --password \ hunter2 tail`, `mysql --password [redacted] tail`, `mysql --password [redacted] tail`},
		{"a lone backslash and a user flag", `curl -u \ admin:hunter2 tail`, `curl -u \ admin:[redacted] tail`, `curl -u \ admin:[redacted] tail`},
		{"a trailing backslash and a blank line", "mysql --password \\\n\nrun now", "mysql --password [redacted]\n\nrun now", "mysql --password [redacted] run now"},
		{"a no-break space in the user of a user flag", "curl -u ad\u00a0min:hunter2 tail", "curl -u ad\u00a0min:[redacted] tail", "curl -u ad min:[redacted] tail"},
		{"a flag and its value after a trailing backslash", "mysql --password \\\n  hunter2", "mysql --password [redacted]", "mysql --password [redacted]"},
		{"a name and its value after a trailing backslash", "PASSWORD=\\\n  hunter2 tail", "PASSWORD=[redacted] tail", "PASSWORD=[redacted] tail"},
		{"a command that goes on", "mysql --password \\\n  hunter2 \\\n  --verbose", "mysql --password [redacted] \\\n  --verbose", "mysql --password [redacted] \\ --verbose"},
		{"a quoted value after a trailing backslash", "curl --token \\\n  'hunter2 two' tail", "curl --token [redacted] tail", "curl --token [redacted] tail"},
		{"a flag that follows a trailing backslash", "mysql --password \\\n  --verbose", "mysql --password [redacted]\n  --verbose", "mysql --password [redacted] --verbose"},
		{"a continued command with a flag that is no secret", "mysql --verbose \\\n  run", "mysql --verbose \\\n  run", "mysql --verbose \\ run"},
		{"a user flag after a trailing backslash", "curl -u \\\n admin:hunter2 tail", "curl -u \\\n admin:[redacted] tail", "curl -u \\ admin:[redacted] tail"},
		{"a flag and its value across a line break", "deploy --password\nhunter2", "deploy --password\nhunter2", "deploy --password [redacted]"},
		{"a no-break space around an equals sign", "password\u00a0=\u00a0hunter2", "password\u00a0=\u00a0[redacted]", "password = [redacted]"},
		{"a no-break space that ends a value", "password=hunter2\u00a0tail", "password=[redacted]\u00a0tail", "password=[redacted] tail"},
		{"a thin space that ends a quoted value", "{\"password\": \"hunter2\"}\u2009tail", "{\"password\": [redacted]}\u2009tail", "{\"password\": [redacted]} tail"},
		{"a marker that a no-break space ends", "password=[redacted]\u00a0hunter2", "password=[redacted]\u00a0hunter2", "password=[redacted] hunter2"},
		{"a URL's password across a line break", "https://user:pa\nss@host/x", "https://user:pa\nss@host/x", "https://[redacted]@host/x"},
		{"a user and its password across a line break", "-u\nadmin:hunter2", "-u\nadmin:hunter2", "-u admin:[redacted]"},
	} {
		if got := FreeText(tc.in); got != tc.free {
			t.Errorf("%s: FreeText(%q) = %q, want %q", tc.name, tc.in, got, tc.free)
		}
		if got := FreeTextLine(tc.in); got != tc.line {
			t.Errorf("%s: FreeTextLine(%q) = %q, want %q", tc.name, tc.in, got, tc.line)
		}
		if got := FreeTextCutLine(tc.in, 4096); got != tc.line {
			t.Errorf("%s: FreeTextCutLine(%q) = %q, want %q", tc.name, tc.in, got, tc.line)
		}
		if got := FreeTextCut(tc.in, 4096); got != tc.free {
			t.Errorf("%s: FreeTextCut(%q) = %q, want %q", tc.name, tc.in, got, tc.free)
		}
	}
}

// capAt cuts at a character: whatever byte the limit falls on, the text it returns is valid and is a prefix.
func TestCapAtCutsAtACharacter(t *testing.T) {
	text := "a\U000065e5b\U0001F600c"
	for n := 0; n <= len(text); n++ {
		got, cut := capAt(text, n)
		if !utf8.ValidString(got) || !strings.HasPrefix(text, got) || len(got) > n || cut != (n < len(text)) {
			t.Errorf("capAt(%q, %d) = %q, %t", text, n, got, cut)
		}
	}
	if got, _ := capAt(text, 3); got != "a" {
		t.Errorf("capAt(%q, 3) = %q, want the character that does not fit left out", text, got)
	}
}
