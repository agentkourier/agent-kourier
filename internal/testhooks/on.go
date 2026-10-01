//go:build testhooks

package testhooks

import (
	"os"
	"syscall"
)

// armed is read once, at startup, and never changed.
var armed = os.Getenv(FaultEnv) == CrashAfterPost

// AfterPost kills the process with SIGKILL if COURIER_FAULT was crash-after-post at startup: the
// message is in Slack and its record is not yet written, which is the case the outbox's
// reconciliation exists for (spec 8.6).
func AfterPost() {
	if armed {
		crashSelf()
	}
}

// crashSelf cannot be caught or deferred around: SIGKILL ends the process at once. The select
// holds the goroutine until it does.
//
//go:noinline
func crashSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}

// SlackAPIURL returns COURIER_SLACK_API_URL, to point courier at the fake Slack.
func SlackAPIURL() string { return os.Getenv(SlackAPIURLEnv) }
