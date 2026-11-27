//go:build !testhooks

package testhooks

import (
	"context"
	"time"
)

// AfterPost is called by the outbox between a successful post and recording it. In this build it
// does nothing.
func AfterPost() {}

// Overrides are what a unit test of a testhooks build sets in place of the test image's environment. In this build the type
// has no field, so nothing, not even this module's own code, can give a Slack API URL or a fake Teams service through it.
type Overrides struct{}

// SlackAPIURL returns the Slack API URL to use in place of Slack's, which the composition passes to
// the Slack client if it is not empty. In this build it is always empty.
func SlackAPIURL(Overrides) string { return "" }

// Teams returns the fake Microsoft Teams service to point a Teams connection at in place of Microsoft. In this build there is
// none: a Teams connection talks to Microsoft alone.
func Teams(Overrides) (TeamsFake, bool, error) { return TeamsFake{}, false, nil }

// BeforeSend does nothing in a production build.
func BeforeSend(context.Context, string) error { return nil }

// AgentStreamTimeout uses the production watchdog default.
func AgentStreamTimeout() time.Duration { return 0 }
