package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// fakePassword is a made-up password, odd enough that it cannot appear in a message by chance.
const fakePassword = "pw-Zq9!x@/:%y"

// dsnForms are DSNs that hold fakePassword in every way the driver accepts one.
func dsnForms(host string) map[string]string {
	esc := url.QueryEscape(fakePassword)
	return map[string]string{
		"url":                     "postgres://courier:" + esc + "@" + host + "/courier?sslmode=disable",
		"postgresql scheme":       "postgresql://courier:" + esc + "@" + host + "/courier",
		"url, password parameter": "postgres://courier@" + host + "/courier?password=" + esc,
		"url, ssl password":       "postgres://courier@" + host + "/courier?sslpassword=" + esc,
		"key/value":               "host=" + strings.Split(host, ":")[0] + " port=" + strings.Split(host, ":")[1] + " user=courier password=" + fakePassword + " dbname=courier",
		"key/value, quoted":       "host=" + strings.Split(host, ":")[0] + " port=" + strings.Split(host, ":")[1] + " user=courier password='" + strings.ReplaceAll(fakePassword, `'`, `\'`) + "' dbname=courier",
	}
}

// leaks reports what an error shows of the secrets, as it prints in the ways an error is printed: the
// DSN, the password, its escaped form, and any piece of the password of four characters or more (a
// parser that quotes the text it stopped at shows a piece).
func leaks(err error, dsn string) []string {
	secrets := []string{dsn, url.QueryEscape(fakePassword)}
	for i := 0; i+4 <= len(fakePassword); i++ {
		for j := i + 4; j <= len(fakePassword); j++ {
			secrets = append(secrets, fakePassword[i:j])
		}
	}
	var found []string
	for _, form := range []string{"%v", "%+v", "%s", "%q"} {
		text := fmt.Sprintf(form, err)
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				found = append(found, fmt.Sprintf("%s shows %q", form, secret))
			}
		}
	}
	return found
}

// closedPort returns the address of a port that refuses connections.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestOpenErrorsNeverCarryTheDSN: whatever way Open fails, the DSN and its password are not in the
// error. The cases are the failures a DSN meets: one the driver cannot parse (the standard library's
// error quotes the whole URL), a server that is not there, and a parameter the driver rejects.
func TestOpenErrorsNeverCarryTheDSN(t *testing.T) {
	addr := closedPort(t)
	cases := map[string]string{
		"a port that is not a number":    "postgres://courier:" + url.QueryEscape(fakePassword) + "@127.0.0.1:port/courier",
		"a parameter the driver rejects": "postgres://courier:" + url.QueryEscape(fakePassword) + "@" + addr + "/courier?sslmode=bogus",
		"a bad connect_timeout":          "postgres://courier:" + url.QueryEscape(fakePassword) + "@" + addr + "/courier?connect_timeout=soon",
		"a password that is not escaped": "postgres://courier:" + fakePassword + "@" + addr + "/courier",
		"a host that is not closed":      "postgres://courier:" + url.QueryEscape(fakePassword) + "@[::1/courier",
	}
	for name, dsn := range dsnForms(addr) {
		cases["refused, "+name] = dsn
	}
	for name, dsn := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			s, err := Open(ctx, dsn)
			if err == nil {
				_ = s.Close()
				t.Fatal("Open succeeded")
			}
			if found := leaks(err, dsn); len(found) > 0 {
				t.Errorf("the error leaks: %v\n(its text, scrubbed of nothing: %q)", found, err.Error())
			}
		})
	}
}

// TestOpenAgainstAServerThatRefusesThePassword: the error the server gives for a wrong password names
// the user, and the redaction keeps the password out of whatever else the driver adds.
func TestOpenAgainstAServerThatRefusesThePassword(t *testing.T) {
	u, err := url.Parse(serverDSN(t))
	if err != nil {
		t.Fatal("the test server's DSN must be a URL") // the error would quote the DSN
	}
	u.User = url.UserPassword(u.User.Username(), fakePassword)
	dsn := u.String()
	_, err = Open(t.Context(), dsn)
	if err == nil {
		t.Fatal("Open with the wrong password succeeded")
	}
	if found := leaks(err, dsn); len(found) > 0 {
		t.Errorf("the error leaks: %v", found)
	}
	if !strings.Contains(err.Error(), "password authentication failed") {
		t.Errorf("the error = %q; want the server's reason, which holds no secret", err)
	}
}

// TestScrubKeepsWhatCallersTest: an error that holds a secret is scrubbed and still says what it is; one
// that does not is returned as it was, so that errors.As keeps working on the common error.
func TestScrubKeepsWhatCallersTest(t *testing.T) {
	dsn := "postgres://courier:" + url.QueryEscape(fakePassword) + "@db:5432/courier"
	r := newRedactor(dsn)

	clean := errors.New("nothing secret")
	var wrapped *redactedError
	if got := r.scrub(clean); !errors.Is(got, clean) || errors.As(got, &wrapped) {
		t.Errorf("scrub of an error with no secret = %v; want the same error", got)
	}
	if got := r.scrub(nil); got != nil {
		t.Errorf("scrub(nil) = %v", got)
	}

	dirty := fmt.Errorf("dial %s: %w", dsn, context.Canceled)
	got := r.scrub(dirty)
	if found := leaks(got, dsn); len(found) > 0 {
		t.Errorf("scrubbed error leaks: %v", found)
	}
	if !errors.Is(got, context.Canceled) {
		t.Error("a scrubbed error no longer matches context.Canceled")
	}
	if errors.Unwrap(got) != nil {
		t.Error("a scrubbed error can be unwrapped to the unredacted one")
	}
	if want := "dial " + redacted + ": context canceled"; got.Error() != want {
		t.Errorf("scrubbed text = %q; want %q", got.Error(), want)
	}
}

// TestStoreOperationsScrubTheirErrors: the one exit of every operation scrubs, so an error that arrives
// with the DSN in it (a driver that quotes it) leaves without.
func TestStoreOperationsScrubTheirErrors(t *testing.T) {
	dsn := "postgres://courier:" + url.QueryEscape(fakePassword) + "@db:5432/courier"
	s := &Store{red: newRedactor(dsn)}
	_, op := telemetry.Start(t.Context(), "courier.store.Test")
	err := s.finish(op, fmt.Errorf("connect %s failed", dsn))
	if found := leaks(err, dsn); len(found) > 0 {
		t.Errorf("the error leaks: %v", found)
	}
}
