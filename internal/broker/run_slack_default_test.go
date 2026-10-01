//go:build !testhooks

package broker

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// A production binary can never send its tokens anywhere but Slack: the Slack client's API URL
// comes only from testhooks.SlackAPIURL, which is empty in this build, and from nothing else, so
// not even an environment variable can redirect it.
func TestSlackConfigTakesItsAPIURLOnlyFromTestHooks(t *testing.T) {
	t.Setenv(testhooks.SlackAPIURLEnv, "http://evil.example/")

	if got := slackConfig(SlackConnection{}, nil).APIURL; got != "" {
		t.Errorf("the Slack config's APIURL = %q in the default build, want empty: Slack's own", got)
	}
}
