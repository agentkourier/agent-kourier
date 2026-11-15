package e2e

import "fmt"

// CheckTier validates the fast, full (kagent), and AX runner tiers from Env.Tier. A
// tier is never guessed: the empty value is an error too. Every tier has Agent Kourier, so a test that needs it
// fails, and does not skip, when its pod is missing, which is what a Deployment that was rejected looks like.
func CheckTier(tier string) error {
	switch tier {
	case "fast", "full", "ax":
		return nil
	}
	return fmt.Errorf("E2E_TIER = %q; want fast, full or ax (hack/kind/e2e-run.sh sets it)", tier)
}

// The modes of E2E_SLACK_MODE (Env.SlackMode): how the fake Slack sends the tier's Agent Kourier its events and presses.
const (
	SlackModeSocket = "socket" // over Socket Mode, the default
	SlackModeHTTP   = "http"   // as signed requests to Agent Kourier's chat listener
)

// CheckSlackMode validates Env.SlackMode. NewEnv gives an unset E2E_SLACK_MODE the default, socket, so an empty value here is an error:
// a mode that was set is never guessed at.
func CheckSlackMode(mode string) error {
	switch mode {
	case SlackModeSocket, SlackModeHTTP:
		return nil
	}
	return fmt.Errorf("E2E_SLACK_MODE = %q; want %s or %s", mode, SlackModeSocket, SlackModeHTTP)
}
