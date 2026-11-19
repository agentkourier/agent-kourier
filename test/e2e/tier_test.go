package e2e

import "testing"

func TestCheckTier(t *testing.T) {
	for _, tier := range []string{"fast", "full", "ax"} {
		if err := CheckTier(tier); err != nil {
			t.Errorf("CheckTier(%q) = %v; want nil", tier, err)
		}
	}
	for _, tier := range []string{"", "Fast", "fast ", "prod", "none"} {
		if err := CheckTier(tier); err == nil {
			t.Errorf("CheckTier(%q) succeeded; want an error: a tier is never guessed", tier)
		}
	}
}

func TestCheckSlackMode(t *testing.T) {
	for _, mode := range []string{SlackModeSocket, SlackModeHTTP} {
		if err := CheckSlackMode(mode); err != nil {
			t.Errorf("CheckSlackMode(%q) = %v; want nil", mode, err)
		}
	}
	for _, mode := range []string{"", "HTTP", "http ", "events", "websocket"} {
		if err := CheckSlackMode(mode); err == nil {
			t.Errorf("CheckSlackMode(%q) succeeded; want an error: a mode is never guessed", mode)
		}
	}
}
