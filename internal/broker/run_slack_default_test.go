//go:build !testhooks

package broker

import (
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// A production binary can never send its tokens anywhere but Slack: the Slack client's API URL
// comes only from testhooks.SlackAPIURL, which is empty in this build, and from nothing else, so
// not even an environment variable can redirect it. Nor can RunConfig's overrides of the test hooks:
// in this build they have no field to set.
func TestSlackConfigTakesItsAPIURLOnlyFromTestHooks(t *testing.T) {
	t.Setenv(testhooks.SlackAPIURLEnv, "http://evil.example/")

	if got := (RunConfig{}).slackConfig(Connection{}, nil).APIURL; got != "" {
		t.Errorf("the Slack config's APIURL = %q in the default build, want empty: Slack's own", got)
	}
	if n := reflect.TypeFor[testhooks.Overrides]().NumField(); n != 0 {
		t.Errorf("testhooks.Overrides has %d fields in the default build, want none", n)
	}
}
