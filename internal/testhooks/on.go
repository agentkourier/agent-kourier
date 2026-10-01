//go:build testhooks

package testhooks

import "os"

// armed is read once, at startup, and never changed.
var armed = os.Getenv(FaultEnv) == CrashAfterPost

// AfterPost ends the process with exit code 137 if COURIER_FAULT was crash-after-post at startup:
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
// It does not send SIGKILL, because courier is PID 1 in its container and the kernel ignores a
// signal that a PID namespace's init sends itself, so the process would carry on. 137 is the status
// a SIGKILLed process reports (128 + 9), which is what Kubernetes then shows for the container.
//
//go:noinline
func crashSelf() { os.Exit(137) }

// SlackAPIURL returns COURIER_SLACK_API_URL, to point courier at the fake Slack.
func SlackAPIURL() string { return os.Getenv(SlackAPIURLEnv) }
