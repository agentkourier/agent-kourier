//go:build !testhooks

package testhooks

import "context"

// AfterPost is called by the outbox between a successful post and recording it. In this build it
// does nothing.
func AfterPost() {}

// Overrides are what a unit test of a testhooks build sets in place of the test image's environment. In this build the type
// has no field, so nothing, not even this module's own code, can give a Slack API URL through it.
type Overrides struct{}

// SlackAPIURL returns the Slack API URL to use in place of Slack's, which the composition passes to
// the Slack client if it is not empty. In this build it is always empty.
func SlackAPIURL(Overrides) string { return "" }

// BeforeSend does nothing in a production build.
func BeforeSend(context.Context, string) error { return nil }
