package config

import (
	"context"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// LoadedAlertmanagerTriggerForTest loads the package tests' hand-written Alertmanager trigger YAML through the loader and
// returns the trigger, for the external test package to compare with chattest.AlertmanagerTrigger: chattest imports this
// package, so the internal tests cannot.
func LoadedAlertmanagerTriggerForTest(t *testing.T) v1alpha1.Trigger {
	t.Helper()
	doc := withTriggers(alertmanagerTrigger("", alertmanagerLimitsYAML))
	set, err := Load(context.Background(), write(t, map[string]string{"b.yaml": doc}))
	if err != nil {
		t.Fatal(err)
	}
	return set.Bindings[0].Spec.Chat.Triggers[0]
}

// NewDirSource returns the Source of the config directory dir, which is what a Reloader reads when it is given Dir and no
// Source, for the conformance test of the sources (sourcetest) in this package's external tests.
func NewDirSource(dir string) Source { return dirSource{dir} }
