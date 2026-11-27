//go:build testhooks

package testhooks

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// armed is read once, at startup, and never changed.
var armed = os.Getenv(FaultEnv) == CrashAfterPost

// AfterPost ends the process with exit code 137 if AGENTKOURIER_FAULT was crash-after-post at startup:
// the message is in Slack and its record is not yet written, which is the case the outbox's
// reconciliation exists for (spec 8.6). The fault stays armed for the life of the Deployment's pod
// template: a restarted container reads the same variable and crashes at its next post too, until a
// rollout disarms it (kube.Pods.SetFault with an empty fault).
func AfterPost() {
	if armed {
		crashSelf()
	}
}

// crashSelf exits at once, as a SIGKILL would: no deferred call runs and nothing more is written.
// It does not send SIGKILL, because Agent Kourier is PID 1 in its container and the kernel ignores a
// signal that a PID namespace's init sends itself, so the process would carry on. 137 is the status
// a SIGKILLed process reports (128 + 9), which is what Kubernetes then shows for the container.
//
//go:noinline
func crashSelf() { os.Exit(137) }

// Overrides are the values a unit test sets in place of the ones a test image reads from its environment. The environment is
// the process's, one value for every test, so a test that sets it cannot run alongside another; an Overrides is per broker.
// The zero value overrides nothing. A production build's Overrides has no field (off.go).
type Overrides struct {
	// SlackAPIURL, if not empty, is what SlackAPIURL returns in place of AGENTKOURIER_SLACK_API_URL.
	SlackAPIURL string
	// TeamsURL and TeamsCA, if TeamsURL is not empty, are what Teams reads in place of AGENTKOURIER_TEAMS_URL and AGENTKOURIER_TEAMS_CA.
	TeamsURL, TeamsCA string
}

// SlackAPIURL returns o.SlackAPIURL if it is set, and AGENTKOURIER_SLACK_API_URL otherwise, to point Agent Kourier at the
// fake Slack.
func SlackAPIURL(o Overrides) string { return cmp.Or(o.SlackAPIURL, os.Getenv(SlackAPIURLEnv)) }

// AgentStreamTimeout shortens the stream watchdog in the AX kind tier.
func AgentStreamTimeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("AGENTKOURIER_TEST_AGENT_STREAM_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return 0
}

// Teams returns the fake Microsoft Teams service that AGENTKOURIER_TEAMS_URL names (o.TeamsURL in a unit test), and false when
// neither is set, which leaves a Teams connection talking to Microsoft. The URL is the fake's https base, with no path: its
// endpoints are the paths test/faketeams serves under it. The fake's TLS is checked against the certificate in AGENTKOURIER_TEAMS_CA
// (o.TeamsCA), which must hold one: a test image never trusts the system's roots for the fake, nor skips the check. An error says
// which of the two is wrong, and quotes neither.
func Teams(o Overrides) (TeamsFake, bool, error) {
	raw, ca := os.Getenv(TeamsURLEnv), os.Getenv(TeamsCAEnv)
	if o.TeamsURL != "" {
		raw, ca = o.TeamsURL, o.TeamsCA
	}
	if raw == "" {
		return TeamsFake{}, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return TeamsFake{}, false, fmt.Errorf("testhooks: %s is not the https base URL of the fake Teams service, with a host and no path", TeamsURLEnv)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ca)) {
		return TeamsFake{}, false, fmt.Errorf("testhooks: %s holds no PEM certificate for the fake Teams service at %s", TeamsCAEnv, u.Host)
	}
	base := u.Scheme + "://" + u.Host
	return TeamsFake{
		TokenURL: base + "/token", MetadataURL: base + "/openid", GraphURL: base + "/v1.0",
		SocketURL: func(region string) string { return base + "/" + url.PathEscape(region) + "/v3/websockets/connect" },
		TLS:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		Wrap:      func(next http.RoundTripper) http.RoundTripper { return onlyHost{host: u.Host, next: next} },
	}, true, nil
}

// errNotTheFake is the error of a call that a Teams connection of a test image made to a host that is not the fake Teams service's.
var errNotTheFake = errors.New("testhooks: a Microsoft Teams connection of a test image calls the fake Teams service alone")

// onlyHost is a transport that sends a request to host alone, and refuses any other, without a dial: a Teams connection of a test
// image is not to reach Microsoft, even through a Bot Connector URL that an activity named.
type onlyHost struct {
	host string
	next http.RoundTripper
}

func (o onlyHost) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != o.host {
		if r.Body != nil {
			_ = r.Body.Close() // a RoundTripper closes the body, even on an error
		}
		return nil, fmt.Errorf("%w (%s), not %s", errNotTheFake, o.host, r.URL.Host)
	}
	return o.next.RoundTrip(r)
}

// BeforeSend parks a test runner at the private stub control gate when the before-send fault is armed.
func BeforeSend(ctx context.Context, messageID string) error {
	if os.Getenv(FaultEnv) != "hold-before-send" {
		return nil
	}
	return holdBeforeSend(ctx, messageID)
}

//go:noinline
func holdBeforeSend(ctx context.Context, messageID string) error {
	sender, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("testhooks: hostname: %w", err)
	}
	body, err := json.Marshal(map[string]string{"message_id": messageID, "sender": sender})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://stubagent.e2e.svc.cluster.local:8080/_control/before-send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("testhooks: before-send gate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("testhooks: before-send gate status %d", resp.StatusCode)
	}
	return nil
}
