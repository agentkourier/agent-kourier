package stubagent

import (
	"reflect"
	"testing"
)

// TestWithoutHITLPartsRemovesOnlyTheHITLEntry: no fixture has an agent message with another
// extension, so this checks the rule directly.
func TestWithoutHITLPartsRemovesOnlyTheHITLEntry(t *testing.T) {
	const other = "https://example.test/other"
	for _, tc := range []struct {
		name string
		in   obj
		want any // the extensions left, nil for none
	}{
		{"the only entry, as the stub builds it", obj{"role": "ROLE_AGENT", "extensions": []string{HITLExtension}}, nil},
		{"one of two, as the stub builds it", obj{"role": "ROLE_AGENT", "extensions": []string{other, HITLExtension}}, []string{other}},
		{"one of two, as decoded from JSON", obj{"role": "ROLE_AGENT", "extensions": []any{HITLExtension, other}}, []string{other}},
		{"a user message is left alone", obj{"role": "ROLE_USER", "extensions": []any{HITLExtension, other}}, []any{HITLExtension, other}},
	} {
		got := withoutHITLParts(tc.in)["extensions"]
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: extensions = %v, want %v", tc.name, got, tc.want)
		}
	}
	in := obj{"role": "ROLE_AGENT", "extensions": []string{HITLExtension}}
	withoutHITLParts(in)
	if in["extensions"] == nil {
		t.Errorf("withoutHITLParts changed its argument")
	}
}
