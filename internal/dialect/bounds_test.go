package dialect

import "testing"

// The bounds are numbers the documentation states (docs/explanation/security.md, docs/how-to/upgrade.md and the page of limits),
// the upgrade note says the operator can raise none of them, and an operator sizes a pod by them. A change of one is a decision
// that changes those pages too, so it is a change of this test, and of them, and not of a constant alone. Other tests follow the
// constants; this one does not, so that they cannot all move together unseen.
func TestBoundsAreWhatTheDocumentationStates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"MaxCardBytes", MaxCardBytes, 1 << 20},
		{"MaxResponseBytes", MaxResponseBytes, 4 << 20},
		{"MaxEventBytes", MaxEventBytes, 2 << 20},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, the documentation says %d: change it there too, and say so in the upgrade note", tc.name, tc.got, tc.want)
		}
	}
}
