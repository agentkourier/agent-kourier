package chat_test

import "testing"

// What the start hook is given of the trigger: the preset and the template after defaulting, so that it needs no
// second look at the config.
func TestTheStartCarriesThePresetAndThePromptTemplate(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.send(t, "100.1", kindFiring)
	got := h.got()
	if len(got) != 1 || got[0].Preset != "alertmanager" || got[0].PromptTemplate == "" {
		t.Fatalf("start given %+v; want the alertmanager preset and its template", got)
	}
}
