//go:build testhooks

package testhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

// SlackAPIURL returns AGENTKOURIER_SLACK_API_URL, to point Agent Kourier at the fake Slack.
func SlackAPIURL() string { return os.Getenv(SlackAPIURLEnv) }

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
