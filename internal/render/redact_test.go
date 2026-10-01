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
	{"acronym then word", map[string]any{"DBAuth": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"lower then upper", map[string]any{"myKey": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"word then digit", map[string]any{"key2": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
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
	{"a space in a URL password", map[string]any{"u": "https://adm:pa SECRETVAL@host/x", "o": "VISIBLE@x"}, "SECRETVAL", "VISIBLE@x"},
	{"a space in a URL password, among arguments", map[string]any{"a": "https://u:SECRETVAL x@h", "b": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"schemeless user and password", map[string]any{"target": "root:SECRETVAL@10.0.0.1:5432/VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"schemeless with an @ in the password", map[string]any{"target": "root:SEC@RETVAL@host VISIBLE"}, "RETVAL", "VISIBLE"},
	{"AWS secret key by shape", map[string]any{"x": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYSECRETVAL1 VISIBLE"}, "SECRETVAL1", "VISIBLE"},
	// A line break in a string must not join the next name: redaction reads the decoded text.
	{"pair after a newline", map[string]any{"a": "VISIBLE: 1\nkey: SECRETVAL"}, "SECRETVAL", "VISIBLE"},
	{"pwd after a newline", map[string]any{"a": "VISIBLE: 1\npwd: SECRETVAL"}, "SECRETVAL", "VISIBLE"},
	{"auth after a newline", map[string]any{"a": "HOST=VISIBLE\nauth=SECRETVAL"}, "SECRETVAL", "VISIBLE"},
	{"pair after a tab", map[string]any{"a": "HOST=VISIBLE\ttoken=SECRETVAL"}, "SECRETVAL", "VISIBLE"},
	{"pair after a carriage return", map[string]any{"a": "HOST=VISIBLE\rsecret=SECRETVAL"}, "SECRETVAL", "VISIBLE"},
	// Zero-width characters cannot split a name.
	{"zero-width in a key", map[string]any{"pa\u200bssword": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"zero-width in a pair name", map[string]any{"cmd": "VISIBLE pass\u200bword=SECRETVAL"}, "SECRETVAL", "VISIBLE"},
	// Command-line shapes.
	{"secret flag then its value", map[string]any{"cmd": "tool --token SECRETVAL VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"secret flag with one dash", map[string]any{"cmd": "tool -password SECRETVAL VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"a flag then another flag", map[string]any{"cmd": "tool --token --verbose VISIBLE"}, "", "--verbose"},
	{"curl -u", map[string]any{"cmd": "curl -u admin:SECRETVAL https://VISIBLE"}, "SECRETVAL", "admin"},
	{"curl --user", map[string]any{"cmd": "curl --user admin:SECRETVAL https://VISIBLE"}, "SECRETVAL", "admin"},
	{"a user without a password", map[string]any{"cmd": "sudo -u postgres psql VISIBLE"}, "", "postgres"},
	{"hash rocket", map[string]any{"cfg": "{'password' => 'SECRETVAL', 'user' => 'VISIBLE'}"}, "SECRETVAL", "VISIBLE"},
	{"a quote that never closes", map[string]any{"cmd": `VISIBLE password="SECRETVAL and more`}, "SECRETVAL", "VISIBLE"},
	// Header pairs as arrays, and two more names.
	{"header pairs as arrays", map[string]any{"h": []any{[]any{"X-Api-Key", "SECRETVAL"}, []any{"Accept", "VISIBLE"}}}, "SECRETVAL", "VISIBLE"},
	{"sessionid", map[string]any{"sessionid": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
	{"connectionString", map[string]any{"connectionString": "SECRETVAL", "ok": "VISIBLE"}, "SECRETVAL", "VISIBLE"},
}

func TestSecretShapesAreRedactedInToolLines(t *testing.T) {
	for _, tc := range secretShapes {
		got := ToolLine("run", tc.args, FlavourText)
		if tc.secret != "" && strings.Contains(got, tc.secret) {
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
		"api-key": true, "apiKey": true, "APIKey": true, "DBAuth": true, "myKey": true, "key2": true, "2key": true, "x-auth-token": true, "Authorization": true, "AWS_SECRET_ACCESS_KEY": true,
		"sessionid": true, "connectionString": true, "connectionstring": true, "connection_string": true, "session_id": true, "jwt": true, "signature": true, "dsn": true, "bearer": true, "pwd": true, "credentials": true,
		"monkey": false, "author": false, "keyboard": false, "max_tokens": false, "passed": false, "bypass": false, "name": false, "": false,
	} {
		if got := isSecretName(name); got != want {
			t.Errorf("isSecretName(%q) = %t, want %t", name, got, want)
		}
	}
}

// A string is cut to maxRedactBytes at a white-space boundary before it is redacted, so that the
// work is bounded and every word is whole or gone: no part of a secret survives the cut.
func TestLeavesAreBoundedAtAWordBeforeRedaction(t *testing.T) {
	leaf := func(v any) string { return redactArgs(map[string]any{"x": v}).(map[string]any)["x"].(string) }

	if got := leaf(strings.Repeat("a=", 512<<10)); len(got) > maxRedactBytes+len(Truncated) {
		t.Errorf("a 1 MB value kept %d bytes; want at most %d", len(got), maxRedactBytes+len(Truncated))
	}
	if got := leaf("short value"); got != "short value" {
		t.Errorf("a short value became %q", got)
	}
	// A secret that straddles the limit is dropped whole, not shown in part.
	straddle := strings.Repeat("x ", (maxRedactBytes-12)/2) + "password=SECRETVALUE tail"
	if got := leaf(straddle); strings.Contains(got, "SEC") || strings.Contains(got, "word=") {
		t.Errorf("a secret across the limit left %q at the end", got[len(got)-40:])
	}
	// A word of more than the limit has no boundary, and goes entirely.
	if got := leaf(strings.Repeat("y", 2*maxRedactBytes)); strings.Contains(got, "y") {
		t.Errorf("an unbroken run of %d bytes kept some of itself", 2*maxRedactBytes)
	}
	// Keys are bounded too.
	huge := redactArgs(map[string]any{strings.Repeat("k", 2*maxRedactBytes): "v"}).(map[string]any)
	for k := range huge {
		if len(k) > maxRedactBytes+len(Truncated) {
			t.Errorf("a key of %d bytes was kept", len(k))
		}
	}
}

func TestAHugeArgumentStillMakesAToolLine(t *testing.T) {
	got := ToolLine("run", map[string]any{"a": strings.Repeat("a=", 512<<10), "b": strings.Repeat("password=x ", 100000)}, FlavourText)
	if !strings.HasPrefix(got, "Calling run(") || len(got) > 1000 {
		t.Errorf("ToolLine of two huge arguments = %d bytes starting %q", len(got), got[:12])
	}
}

func BenchmarkToolLineLongArguments(b *testing.B) {
	for name, v := range map[string]string{
		"80k a=a=":       strings.Repeat("a=", 40000),
		"1MB a=a=":       strings.Repeat("a=", 512<<10),
		"80k name=value": strings.Repeat("name=value ", 8000),
	} {
		b.Run(name, func(b *testing.B) {
			args := map[string]any{"cmd": v}
			for range b.N {
				ToolLine("run", args, FlavourText)
			}
		})
	}
}

// FuzzSecretValueNeverShown: whatever follows a secret name is not shown, in any of the shapes.
func FuzzSecretValueNeverShown(f *testing.F) {
	for _, s := range []string{"hunter2hunter2", "Zq9\\nxx", "a b", "\"q\"", "x=y", "\u200b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		var b strings.Builder // the value, as one word of letters and digits long enough to be no accident
		for _, r := range v {
			if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		secret := "QX7" + b.String() + "7XQ"
		for _, text := range []string{
			"PGPASSWORD=" + secret + " psql", "password: " + secret, "tool --token " + secret + " x",
			"a: 1\nkey: " + secret, `{"password":"` + secret + `"}`, "{'secret' => '" + secret + "'}",
		} {
			if got := ToolLine("run", map[string]any{"cmd": text}, FlavourText); strings.Contains(got, secret) {
				t.Fatalf("%q shows %q", got, secret)
			}
		}
	})
}
