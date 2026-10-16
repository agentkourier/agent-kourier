package config_test

import (
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// The Alertmanager trigger YAML of this package's loader tests, once loaded, is chattest.AlertmanagerTrigger, which the
// other packages' tests use, so a change to a regex of one fails here until the other follows. The prompt may differ, and the YAML also
// excludes the heartbeat.
func TestTheLoaderTestsUseTheSharedAlertmanagerTrigger(t *testing.T) {
	got := config.LoadedAlertmanagerTriggerForTest(t)
	want := chattest.AlertmanagerTrigger("B0123456789")
	want.PromptTemplate, want.Exclude = got.PromptTemplate, got.Exclude
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config's alertmanagerTriggerYAML differs from chattest.AlertmanagerTrigger:\n got  %+v\n want %+v", got, want)
	}
}
