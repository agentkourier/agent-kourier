package e2e

import (
	"os"
	"strings"
	"testing"
)

func TestExitReasonsAndStoppedByFence(t *testing.T) {
	stale := `{"level":"ERROR","msg":"leadership lost: stopping at once, without a drain","reason":"a write found its epoch stale: another replica has led since"}`
	lease := `{"level":"ERROR","msg":"leadership lost: stopping at once, without a drain","reason":"` + leaseLostReason + `"}`
	noise := `{"level":"INFO","msg":"leading","epoch":3}` + "\nnot json at all\n"
	for _, tc := range []struct {
		name    string
		log     string
		reasons int
		fenced  bool
	}{
		{"a stale epoch", noise + stale, 1, true},
		{"a lost lease alone", noise + lease, 1, false},
		{"both: a write was refused too", lease + "\n" + stale, 2, true},
		{"no such line", noise, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExitReasons(tc.log)
			if len(got) != tc.reasons || StoppedByFence(got) != tc.fenced {
				t.Errorf("ExitReasons = %q, fenced %v; want %d reasons, fenced %v", got, StoppedByFence(got), tc.reasons, tc.fenced)
			}
		})
	}
}

// The lease-lost reason is a copy of the broker's, which test/e2e does not import: if the broker's wording changed, a lease loss
// would count as a fence. Pin the two equal.
func TestLeaseLostReasonIsTheBrokers(t *testing.T) {
	src, err := os.ReadFile("../../internal/broker/election.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := "const leaseLostReason = \"" + leaseLostReason + "\""; !strings.Contains(string(src), want) {
		t.Errorf("internal/broker/election.go has no %s: the broker's lease-lost reason changed; copy it to test/e2e/fence.go", want)
	}
}
