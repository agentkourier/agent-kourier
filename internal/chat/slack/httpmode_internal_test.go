package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/secret"
)

const fakeSigningSecret = "fake-signing-secret"

// signedAt is the headers of a request whose body is body, signed with the fake secret at the time at.
func signedAt(body string, at time.Time) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(fakeSigningSecret))
	_, _ = fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	return http.Header{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {"v0=" + hex.EncodeToString(mac.Sum(nil))}}
}

// The age of a request is read on the Client's clock, against signatureMaxAge, before the signature is checked: a request signed now is
// stale to a clock six minutes ahead, either way, and taken by one that agrees.
func TestTheAgeOfARequestIsReadOnTheClientsClock(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		clock time.Time
		want  string
	}{
		{"a clock that agrees", now, ""},
		{"a clock six minutes ahead", now.Add(6 * time.Minute), httpStale},
		{"a clock six minutes behind", now.Add(-6 * time.Minute), httpStale},
		{"a clock just inside the age", now.Add(signatureMaxAge - 5*time.Second), ""},
	} {
		c := New(Config{SigningSecret: secret.New(fakeSigningSecret), Now: func() time.Time { return tc.clock }})
		if got := c.verify(signedAt("{}", now), []byte("{}")); got != tc.want {
			t.Errorf("%s: verify = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// slack-go's verifier, which checks the signature after the Client has checked the age, refuses past signatureMaxAge on its own wall
// clock: were it stricter, a request the page says is taken would be refused. A slack-go upgrade that moves its bound fails here.
func TestSlackGosVerifierAllowsTheSameAge(t *testing.T) {
	now := time.Now()
	if _, err := slackgo.NewSecretsVerifier(signedAt("{}", now.Add(-signatureMaxAge+5*time.Second)), fakeSigningSecret); err != nil {
		t.Errorf("slack-go refuses a request just inside signatureMaxAge: %v", err)
	}
	if _, err := slackgo.NewSecretsVerifier(signedAt("{}", now.Add(-signatureMaxAge-5*time.Second)), fakeSigningSecret); !errors.Is(err, slackgo.ErrExpiredTimestamp) {
		t.Errorf("slack-go past signatureMaxAge = %v; want ErrExpiredTimestamp", err)
	}
}
