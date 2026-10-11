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
		{"a value three lines after the name", "- name: DB_PASSWORD\n  a: 1\n  b: 2\n  value: hunter2", "- name: DB_PASSWORD\n  a: 1\n  b: 2\n  value: [redacted]", "- name: DB_PASSWORD a: 1 b: 2 value: [redacted]"},
		{"a value past a list entry", "- name: DB_PASSWORD\n  a: 1\n- b: 2\n  value: hunter2", "- name: DB_PASSWORD\n  a: 1\n- b: 2\n  value: hunter2", "- name: DB_PASSWORD a: 1 - b: 2 value: hunter2"},
		{"a value nine lines after the name", "- name: DB_PASSWORD\n  a: 1\n  b: 2\n  c: 3\n  d: 4\n  e: 5\n  f: 6\n  g: 7\n  h: 8\n  i: 9\n  value: hunter2", "- name: DB_PASSWORD\n  a: 1\n  b: 2\n  c: 3\n  d: 4\n  e: 5\n  f: 6\n  g: 7\n  h: 8\n  i: 9\n  value: hunter2", "- name: DB_PASSWORD a: 1 b: 2 c: 3 d: 4 e: 5 f: 6 g: 7 h: 8 i: 9 value: hunter2"},
		{"the first value is the entry's", "- name: DB_PASSWORD\n  value: one\n  value: two", "- name: DB_PASSWORD\n  value: [redacted]\n  value: two", "- name: DB_PASSWORD value: [redacted] value: two"}, {"a name that ends in name", "hostname: DB_PASSWORD\n  value: hunter2", "hostname: DB_PASSWORD\n  value: hunter2", "hostname: DB_PASSWORD value: hunter2"},
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
		// A secret-named key whose value is another key: the nested key is not the value.
		{"a nested secret key", "authorization:\n  credentials: hunter2", "authorization:\n  credentials: [redacted]", "authorization: credentials: [redacted]"},
		{"a nested secret key, after a key that is no secret", "http_config:\n  authorization:\n    type: Bearer\n    credentials: hunter2", "http_config:\n  authorization:\n    type: Bearer\n    credentials: [redacted]", "http_config: authorization: [redacted] Bearer credentials: [redacted]"},
		{"a nested password", "basic_auth:\n  password: hunter2", "basic_auth:\n  password: [redacted]", "basic_auth: password: [redacted]"},
		{"a nested password two levels down", "redis:\n  auth:\n    password: hunter2", "redis:\n  auth:\n    password: [redacted]", "redis: auth: password: [redacted]"},
		{"a nested secret under a token", "token:\n  secret: hunter2", "token:\n  secret: [redacted]", "token: secret: [redacted]"},
		{"a nested value", "apiKey:\n  value: hunter2", "apiKey:\n  value: [redacted]", "apiKey: value: [redacted]"},
		{"a secret key on a line with a secret key", "secret: password: hunter2", "secret: password: [redacted]", "secret: password: [redacted]"},
		{"a nested password in a flow mapping", "auth: {password: hunter2}", "auth: {password: [redacted]", "auth: {password: [redacted]"},
		{"a nested password in JSON", `{"auth": {"password": "hunter2"}}`, `{"auth": {"password": [redacted]}}`, `{"auth": {"password": [redacted]}}`},
		{"a secret key and nothing after the nested key", "token: password:", "token: [redacted]", "token: [redacted]"},
		{"a value with a colon in it is no nested key", "password: abc:hunter2", "password: [redacted]", "password: [redacted]"},
		{"a nested key that is no secret, under a secret", "secret:\n  name: db", "secret:\n  name: db", "secret: [redacted] db"},
		{"a nested key that is no secret, then the secret", "auth:\n  user: bob\n  password: hunter2", "auth:\n  user: bob\n  password: [redacted]", "auth: [redacted] bob password: [redacted]"},
		{"a blank line ends a name's value", "Token:\n\nPlease rotate it", "Token:\n\nPlease rotate it", "Token: [redacted] rotate it"},
		{"an indented value after a blank line", "password:\n\n  hunter2", "password:\n\n  [redacted]", "password: [redacted]"},
		{"a value on the line after a name", "password:\nhunter2", "password:\n[redacted]", "password: [redacted]"},
		{"a label, a blank line and a secret", "Password:\n\nhunter2", "Password:\n\n[redacted]", "Password: [redacted]"},
		{"a blank line of spaces and a secret", "password:\n   \nhunter2x", "password:\n   \n[redacted]", "password: [redacted]"},
		{"a name, a blank line and a list", "The token:\n\n- expired at 10:00", "The token:\n\n- expired at 10:00", "The token: [redacted] expired at 10:00"},
		// A value that looks like a key is the value of the name above it; only a key's shape is a key.
		{"a value that is a secret name, and a line of equals signs", "password: Password123\n=======", "password: [redacted]\n=======", "password: [redacted] ======="},
		{"a value that is a secret name and a hash rocket", "secret: apikey123 => rotated", "secret: [redacted] => rotated", "secret: [redacted] => rotated"},
		{"a value, a spaced equals sign", "password: MySecret2024 = rotated yesterday", "password: [redacted] = rotated yesterday", "password: [redacted] = rotated yesterday"},
		{"a value and a colon on the next line", "password: mytoken\n: x", "password: [redacted]\n: x", "password: [redacted] : x"},
		{"a quoted value that holds a key", `password: "Session: abc123"`, "password: [redacted]", "password: [redacted]"},
		{"a quoted value that holds an assignment", "password: 'key=abc123'", "password: [redacted]", "password: [redacted]"},
		{"a value that is a secret name and a colon", "password: secret:hunter2", "password: [redacted]", "password: [redacted]"},
		{"a quoted value on the next line that holds a colon", "password:\n  'hunter2: x'", "password:\n  [redacted]", "password: [redacted]"},
		{"a secret key and a key, then the value", "password: token: realsecret1", "password: token: [redacted]", "password: token: [redacted]"},
		{"a spaced secret pair two levels down", "auth:\n  credentials:\n    password : SECRETx1", "auth:\n  credentials:\n    password : [redacted]", "auth: credentials: [redacted] : [redacted]"},
		{"a secret pair with an equals sign two levels down", "credentials:\n  secret:\n    token = SECRETx1", "credentials:\n  secret:\n    [redacted]", "credentials: secret: [redacted]"},
		{"a compact JSON object under a secret name", "secret:\n  {\"name\":\"SECRETKEY\",\"value\":\"x\"}", "secret:\n  [redacted]", "secret: [redacted]"},
		{"a secret name as the value, and a pair that reads it as a name", "password:\n  Password123 = rotated", "password:\n  [redacted]", "password: [redacted]"},
		{"a spaced password under basic_auth", "basic_auth:\n  password : S3CRET", "basic_auth:\n  password : [redacted]", "basic_auth: [redacted] : [redacted]"},
		{"a spaced key under a spaced name", "authorization :\n  credentials : S3CRET", "authorization :\n  credentials : [redacted]", "authorization : [redacted] : [redacted]"},
		{"a spaced quoted pair in JSON", `{"credentials" : {"password" : "S3CRET"}}`, `{"credentials" : {"password" : [redacted]}}`, `{"credentials" : {"password" : [redacted]}}`},
		{"a PowerShell assignment under a name", "secret:\n  $env:APIKEY = \"x\"", "secret:\n  [redacted]", "secret: [redacted]"},
		{"a bold name with the colon inside", "**Password:** x", "**Password:** [redacted]", "**Password:** [redacted]"},
		{"an italic name", "*Token:* x", "*Token:* [redacted]", "*Token:* [redacted]"},
		{"a bold name in a list item", "- **API key**: x", "- **API key**: [redacted]", "- **API key**: [redacted]"},
		{"a bold label, a blank line and a quoted secret", "**Password:**\n\n`x`", "**Password:**\n\n[redacted]", "**Password:** [redacted]"},
		{"a value that begins with emphasis marks", "password: ~~x", "password: [redacted]", "password: [redacted]"},
		{"a value that begins with emphasis marks, glued to the colon", "password:~~x", "password:[redacted]", "password:[redacted]"},
		{"a secret and words after a blank line", "password:\n\nhunter2x (rotated)", "password:\n\n[redacted] (rotated)", "password: [redacted] (rotated)"},
		{"a compact JSON object with a comma after it", "Token:\n    {\"db\":\"SECRET\"},", "Token:\n    [redacted]", "Token: [redacted]"},
		{"a compact JSON object that is cut", "password:\n{\"db\":\"SECRET\"", "password:\n[redacted]", "password: [redacted]"},
		{"spaced JSON under a name is read as keys", "password:\n  {\"user\": \"bob\", \"password\": \"SECRET\"}", "password:\n  {\"user\": \"bob\", \"password\": [redacted]}", "password: [redacted] \"bob\", \"password\": [redacted]}"},
		{"a secret pair that is the fifth in the first run", "secret:\n  a=b=c=d=password : SEKRITX", "secret:\n  [redacted]", "secret: [redacted]"},
		{"a PowerShell secret pair after others", "secret:\n  $env:A=1;$env:B=2;$env:APIKEY = SEKRITX", "secret:\n  [redacted]", "secret: [redacted]"},
		{"a secret pair behind many assignments", "token:\n  a0=0;a1=1;a2=2;a3=3;secret = SECRET", "token:\n  [redacted]", "token: [redacted]"},
		{"a name with text, then a secret entry", "name: HOST\nsome text\nname: DB_PASSWORD\nvalue: S3CRET", "name: HOST\nsome text\nname: DB_PASSWORD\nvalue: [redacted]", "name: HOST some text name: DB_PASSWORD value: [redacted]"},
		// Bearer before a secret word and its colon.
		{"a Bearer and a secret word with a colon", "Use the bearer token: abc123xyz789", "Use the bearer token: [redacted]", "Use the bearer token: [redacted]"},
		{"an Authorization, a Bearer and a secret word with a colon", "Authorization: Bearer credentials: abc123xyz789", "Authorization: [redacted] credentials: [redacted]", "Authorization: [redacted] credentials: [redacted]"},
		{"a Bearer ending a line and a secret key under it", "Authorization: Bearer\n  token: realtoken123", "Authorization: [redacted]\n  token: [redacted]", "Authorization: [redacted] token: [redacted]"},
		// Env entries, the JSON inside a string, comments and blank lines, and another entry's value.
		{"an env entry in JSON inside a string", `{\"name\":\"DB_PASSWORD\",\"value\":\"hunter2\"}`, `{\"name\":\"DB_PASSWORD\",\"value\":[redacted]}`, `{\"name\":\"DB_PASSWORD\",\"value\":[redacted]}`},
		{"an env entry with two comment lines", "- name: API_TOKEN\n  # a\n  # b\n  value: hunter2", "- name: API_TOKEN\n  # a\n  # b\n  value: [redacted]", "- name: API_TOKEN # a # b value: [redacted]"},
		{"an env entry with a blank line", "- name: API_TOKEN\n\n  value: hunter2", "- name: API_TOKEN\n\n  value: [redacted]", "- name: API_TOKEN value: [redacted]"},
		{"an env entry and another entry's value", "- name: DB_PASSWORD\n- name: HOST\n  value: public", "- name: DB_PASSWORD\n- name: HOST\n  value: public", "- name: DB_PASSWORD - name: HOST value: public"},
		// A tag or an anchor before a block header.
		{"a block scalar with a tag", "password: !!binary |\n  aHVudGVyMg==\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a block scalar with an anchor", "password: &pw |\n  hunter2\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a block scalar with a tag and an anchor", "password: !!str &pw >-\n  hunter2\nnext: x", "password: [redacted]\nnext: x", "password: [redacted] next: x"},
		{"a tag before a block scalar under a name that is no secret", "note: !!binary |\n  hunter2\nnext: x", "note: !!binary |\n  hunter2\nnext: x", "note: !!binary | hunter2 next: x"},
		{"a tag that is no tag and the word after it", "password: !hunter2 more", "password: [redacted]", "password: [redacted]"},
		{"an anchor and a plain value", "password: &pgpw supersecret1", "password: [redacted]", "password: [redacted]"},
		{"an anchor, a value and an alias", "POSTGRES_PASSWORD: &pw s3cr3t\nother: *pw", "POSTGRES_PASSWORD: [redacted]\nother: *pw", "POSTGRES_PASSWORD: [redacted] other: *pw"},
		{"a tag and a quoted value with a space", `password: !!str "hunter 2"`, "password: [redacted]", "password: [redacted]"},
		{"a tag and a plain value", "password: !!str hunter2", "password: [redacted]", "password: [redacted]"},
		// The continuation marks of PowerShell and cmd.
		{"a flag after a PowerShell backtick", "mysql --password `\n  hunter2 tail", "mysql --password [redacted] tail", "mysql --password [redacted] tail"},
		{"a name after a cmd caret", "set PASSWORD=^\n hunter2 tail", "set PASSWORD=[redacted] tail", "set PASSWORD=[redacted] tail"},
		{"a flag after a backtick, then a flag", "mysql --password `\n  --verbose", "mysql --password [redacted]\n  --verbose", "mysql --password [redacted] --verbose"},
		{"a user flag after a backtick", "curl -u `\n  admin:hunter2 tail", "curl -u `\n  admin:[redacted] tail", "curl -u ` admin:[redacted] tail"},
		// URL userinfo ends at a space of any width.
		{"a no-break space after a host, then an address", "see https://example.test\u00a0bob@example.test tail", "see https://example.test\u00a0bob@example.test tail", "see https://example.test bob@example.test tail"},
		{"a no-break space after userinfo", "https://u:p@h\u00a0next@x", "https://[redacted]@h\u00a0next@x", "https://[redacted]@h next@x"},
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

// TestFreeTextKnownLimits pins what the free-text rules document they do not catch, and what they take that is no secret.
// Each is deliberate: when one is fixed, flip its case and update the limits listed on FreeText and in docs/reference/stored-data.md.
func TestFreeTextKnownLimits(t *testing.T) {
	for _, c := range []struct {
		name, in, leak string
		line           bool // the leak is in FreeTextLine's result too
	}{
		// FreeTextLine reads the lines first, and the pair on the second line takes its "@" away before the joined read, which
		// would see the URL's password cut by it.
		{"a URL password, and a pair with an @ on the next line", "https://admin:hunter2\npassword=x@host", "hunter2", true},
		{"a list under a secret name", "password:\n- hunter2", "hunter2", true},
		{"a mapping of keys that are no secret names under a secret name", "secret:\n  db: hunter2", "hunter2", true},
		{"a plain scalar that goes on", "password: hunter\n  2secret", "2secret", true},
		{"a fenced block after a name", "New password:\n\n```\nhunter2x\n```", "hunter2x", true},
		{"terraform's name and value", "set {\n  name  = \"auth.password\"\n  value = \"hunter2x\"\n}", "hunter2x", true},
		{"a tag with its value on the next line", "password: !!str\n  hunter2x", "hunter2x", true},
		{"a secret written where a nested key would be", "token:\n  SECRET : x", "SECRET", false},
		{"a quoted secret written where a nested key would be", "Password:\n    'SECRET': rotated", "SECRET", false},
		{"a sentence that begins with a plain word after a blank line", "password:\n\nletmein now", "letmein", false},
		{"a short flag", "mysql -p hunter2", "hunter2", true},
		{"a name and a value with no separator", "passwd  hunter2", "hunter2", true},
	} {
		if got := FreeText(c.in); !strings.Contains(got, c.leak) {
			t.Errorf("%s: FreeText(%q) = %q, the documented limit no longer leaks %q", c.name, c.in, got, c.leak)
		}
		if got := FreeTextLine(c.in); c.line && !strings.Contains(got, c.leak) {
			t.Errorf("%s: FreeTextLine(%q) = %q, the documented limit no longer leaks %q", c.name, c.in, got, c.leak)
		}
	}
	// Read joined, a line break is a space: a value that began a paragraph, or a word after a flag on the line above, is
	// taken for the value.
	for _, c := range []struct{ name, in, want string }{
		{"a name and a new paragraph", "Token:\n\nPlease rotate it", "Token: [redacted] rotate it"},
		{"a flag and the next sentence", "Retry with --token\nThe deploy failed", "Retry with --token [redacted] deploy failed"},
		{"a secret object and one of its keys", "secret:\n  name: db", "secret: [redacted] db"},
	} {
		if got := FreeTextLine(c.in); got != c.want {
			t.Errorf("%s: FreeTextLine(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if got := FreeText(c.in); got != c.in {
			t.Errorf("%s: FreeText(%q) = %q, want it kept (its lines tell the name from the text)", c.name, c.in, got)
		}
	}
}

// A Markdown legend of a secret word is taken: the name rules cannot tell "**Key**: blue means prod" from a key and its value.
func TestAMarkdownLegendOfASecretWordIsTaken(t *testing.T) {
	for in, want := range map[string]string{
		"**Key**: blue means prod":   "**Key**: [redacted] means prod",
		"**Session**: 42 minutes":    "**Session**: [redacted] minutes",
		"**Note**: blue means prod":  "**Note**: blue means prod",
		"**Token**: rotate it today": "**Token**: [redacted] it today",
	} {
		if got := FreeText(in); got != want {
			t.Errorf("FreeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// A one-word paragraph after a name and a blank line is taken for the value, in every entry: a secret may be a plain word.
func TestAOneWordParagraphAfterABlankLineIsTaken(t *testing.T) {
	if got := FreeText("Token:\n\nExpired"); got != "Token:\n\n[redacted]" {
		t.Errorf("FreeText = %q", got)
	}
	if got := FreeTextLine("Token:\n\nExpired"); got != "Token: [redacted]" {
		t.Errorf("FreeTextLine = %q", got)
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

// FreeTextWhole says whether any of its input was cut, and is FreeText's own answer: it is not read from the length of the
// input, which the token layer changes before the bound is applied, nor from the mark at the end of the result, which a rule
// that takes a value takes with it.
func TestFreeTextWholeSaysWhetherTheTextWasCut(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
		mark bool // the result ends in the mark of a cut
	}{
		{"short", "short value", true, false},
		{"empty", "", true, false},
		{"exactly the bound", strings.Repeat("a ", MaxBytes/2), true, false},
		{"one byte over", strings.Repeat("a ", MaxBytes/2) + "a", false, true},
		{"an unbroken run over", strings.Repeat("y", MaxBytes+1), false, true},
		{"over what the token layer reads", strings.Repeat("a ", 4*MaxBytes), false, true},
		// Under the bound as it came, over it once the token layer has written "[redacted]" for each value: cut.
		{"grows past the bound", strings.Repeat("字 ", 900) + strings.Repeat("access_token=x ", 30) + "; rm -rf /x", false, false},
		{"grows past the bound, many pairs", strings.Repeat("access_token=x ", 250), false, false},
		{"grows past the bound, another name", strings.Repeat("client_secret=a ", 250), false, false},
		// Over the bound as it came, under it once the invisible characters are deleted: whole.
		{"shrinks under the bound", strings.Repeat("\xe2\x80\x8b", MaxBytes/3) + strings.Repeat("a ", MaxBytes/2-10) + "ok", true, false},
		{"shrinks by its line breaks", strings.Repeat("a\r\n", MaxBytes/3), true, false},
		// Over what the token layer reads as it came, and under the bound once the invisible characters are gone: it was cut all the
		// same, and the rest of the text was never read, so it is not whole. 18 KiB of zero-width spaces after a word is 6144
		// characters of 3 bytes, a word more than rawCap, and nothing at all once the spaces are deleted.
		{"over what the token layer reads, under the bound once normalized", "word " + strings.Repeat("\xe2\x80\x8b", 6144), false, true},
	} {
		text, whole := FreeTextWhole(tc.in)
		if whole != tc.want || text != FreeText(tc.in) || strings.HasSuffix(text, Truncated) != tc.mark {
			t.Errorf("%s: FreeTextWhole = %v, mark at the end %v; want %v and %v (and FreeText's text)", tc.name, whole, strings.HasSuffix(text, Truncated), tc.want, tc.mark)
		}
	}
}
