package v1alpha1_test

import (
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
)

// The trigger the regex tests of this package run against is the one every other package's tests use, so a change to
// a regex of one fails here until the other follows. The prompt may differ: the tests here do not render it.
func TestTheRegexTestsRunAgainstTheSharedAlertmanagerTrigger(t *testing.T) {
	got := v1alpha1.AlertmanagerTriggerForTest()
	want := chattest.AlertmanagerTrigger("B0123456789")
	want.PromptTemplate = got.PromptTemplate
	if !reflect.DeepEqual(got, want) {
		t.Errorf("api/v1alpha1's alertmanagerTrigger differs from chattest.AlertmanagerTrigger:\n got  %+v\n want %+v", got, want)
	}
}
