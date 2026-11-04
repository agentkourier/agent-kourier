package crdcorpus

import (
	"strings"
	"testing"
)

func root(t *testing.T) string {
	t.Helper()
	r, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestEveryValidEntryIsAcceptedByTheBroker keeps the valid half honest: what is called valid is what the broker's
// file loader accepts, so that the cluster test can hold the schema to "never refuses it".
func TestEveryValidEntryIsAcceptedByTheBroker(t *testing.T) {
	entries, err := Valid(root(t))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range entries {
		kinds[e.Kind]++
		if err := Broker(t.Context(), e); err != nil {
			t.Errorf("%s: the broker refuses a valid entry: %v", e.Name, err)
		}
	}
	// A glob that stopped matching would empty the corpus and let the cluster test pass on nothing.
	for _, kind := range []string{"AgentBackend", "ChatConnection", "Binding"} {
		if kinds[kind] < 5 {
			t.Errorf("only %d valid %s entries: a valid root no longer holds them", kinds[kind], kind)
		}
	}
	t.Logf("%d valid entries: %v", len(entries), kinds)
}

// TestEveryInvalidEntryIsRefusedByTheBroker is the broker half of the invariant: each invalid entry is refused by
// Default and Validate (through the loader), for the reason it names.
func TestEveryInvalidEntryIsRefusedByTheBroker(t *testing.T) {
	entries, err := Invalid(root(t))
	if err != nil {
		t.Fatal(err)
	}
	var refuses int
	for _, e := range entries {
		err := Broker(t.Context(), e)
		switch {
		case err == nil:
			t.Errorf("%s: the broker accepts an invalid entry", e.Name)
		case !strings.Contains(err.Error(), e.Want):
			t.Errorf("%s: the broker's refusal does not contain %q:\n%v", e.Name, e.Want, err)
		}
		if e.SchemaRefuses {
			refuses++
		}
	}
	// The count of rules is the plan's, grown by the caps and the cases that one rule has several branches of.
	if len(entries) < 120 {
		t.Errorf("%d invalid entries; a rule has lost its entry", len(entries))
	}
	t.Logf("%d invalid entries, %d of which the schema refuses and %d only the broker", len(entries), refuses, len(entries)-refuses)
}
