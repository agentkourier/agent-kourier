//go:build !testhooks

package broker

import (
	"reflect"
	"testing"
)

// The production build has no way to start without interactions, and none to point Slack's API
// somewhere else, which would send the bot token there: the fields are not in RunConfig at all, so no
// code, and no config, can set them. (The Slack URL comes from testhooks.SlackAPIURL, which is "" here.
// 35h9 deletes NoInteractions from the testhooks build too, once there are interactions to run with.)
// There is no renderer option in any build, and no Sink field: Run builds the renderer itself.
func TestTheDefaultBuildHasNoNoInteractionsSinkOrSlackAPIURL(t *testing.T) {
	typ := reflect.TypeOf(RunConfig{})
	for _, name := range []string{"Sink", "NoRenderer", "NoInteractions", "SlackAPIURL"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Errorf("RunConfig has a %s field in the default build", name)
		}
	}
}
