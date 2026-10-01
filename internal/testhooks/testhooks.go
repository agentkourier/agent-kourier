// Package testhooks holds the hooks that only a test image of courier carries: a crash between a
// Slack post and its record (spec 8.6, S5), armed through COURIER_FAULT, and an override of the Slack
// API URL so courier can talk to the fake Slack.
//
// Both exist only when the binary is built with the testhooks tag. In the default build, which is the
// production build, AfterPost does nothing and SlackAPIURL returns "", and the binary contains
// neither the crash nor the names of the environment variables (buildcheck_test.go proves it). The
// override moves where the API calls go, never which token they carry.
package testhooks

const (
	// FaultEnv names the environment variable that arms a fault, read once at startup.
	FaultEnv = "COURIER_FAULT"
	// CrashAfterPost is the FaultEnv value that makes AfterPost kill the process.
	CrashAfterPost = "crash-after-post"
	// SlackAPIURLEnv names the environment variable that SlackAPIURL returns.
	SlackAPIURLEnv = "COURIER_SLACK_API_URL"
)
