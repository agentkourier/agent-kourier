package runtime

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
)

func TestCommand(t *testing.T) {
	a := testAddon(newFakeStore(), &scripted{})
	for _, tc := range []struct {
		name       string
		args       []string
		env        map[string]string
		code       int
		stdout     string
		stderr     string // a part of it
		notInError string
	}{
		{name: "version", args: []string{"--version"}, code: ExitOK, stdout: "1.2.3-test\n"},
		{name: "an unknown flag", args: []string{"--nope"}, code: ExitUsage, stderr: "flag provided but not defined: -nope"},
		{name: "an argument", args: []string{"fake-secret-typed-as-an-argument"}, code: ExitUsage,
			stderr: "agent-kourier-kit: the agent takes no arguments; it is configured by its environment", notInError: "fake-secret"},
		{name: "a setting it cannot use", env: map[string]string{"LISTEN_ADDR": "nope"}, code: ExitUsage,
			stderr: `"msg":"configuration","error":"LISTEN_ADDR \"nope\" is not host:port`},
		{name: "a trace exporter it cannot use", env: map[string]string{"AUTH_TOKEN_FILE": "/tokens", "OTEL_TRACES_EXPORTER": "fake-exporter"},
			code: ExitUsage, stderr: `"msg":"configuration","error":"OTEL_TRACES_EXPORTER must be otlp or none"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Command(a, tc.args, &stdout, &stderr, func(k string) string { return tc.env[k] })
			if code != tc.code || stdout.String() != tc.stdout || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("Command = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
			if tc.notInError != "" && strings.Contains(stderr.String(), tc.notInError) {
				t.Fatalf("stderr shows %q: %s", tc.notInError, stderr.String())
			}
			if strings.Contains(tc.stderr, `"msg"`) && !json.Valid(bytes.TrimSpace(stderr.Bytes())) {
				t.Fatalf("stderr is not one JSON line: %s", stderr.String())
			}
		})
	}
}

// Parse reads OIDC_ISSUER, OIDC_AUDIENCE and OIDC_SUBJECT as a caller check only for an add-on with an OIDC check.
func TestParseReadsOIDCOnlyForAnAddonWithAnOIDCCheck(t *testing.T) {
	env := map[string]string{"AUTH_TOKEN_FILE": "/tokens", "OIDC_ISSUER": "https://issuer.test", "OIDC_AUDIENCE": "aud", "OIDC_SUBJECT": "sub"}
	getenv := func(k string) string { return env[k] }
	a := testAddon(newFakeStore(), &scripted{})
	if cfg, _, err := Parse(a, getenv); err != nil || cfg.OIDC() {
		t.Fatalf("Parse without an OIDC check = OIDC %v, %v; want the token check", cfg.OIDC(), err)
	}
	a.OIDC = func(string, string, string) (auth.Verifier, error) { return auth.Open{}, nil }
	delete(env, "AUTH_TOKEN_FILE")
	if cfg, _, err := Parse(a, getenv); err != nil || !cfg.OIDC() {
		t.Fatalf("Parse with an OIDC check = OIDC %v, %v; want OIDC", cfg.OIDC(), err)
	}
}
