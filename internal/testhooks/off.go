//go:build !testhooks

package testhooks

// AfterPost is called by the outbox between a successful post and recording it. In this build it
// does nothing.
func AfterPost() {}

// SlackAPIURL returns the Slack API URL to use in place of Slack's, which the composition passes to
// the Slack client if it is not empty. In this build it is always empty.
func SlackAPIURL() string { return "" }
