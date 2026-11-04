package crdcorpus

import (
	"slices"
	"testing"
)

// TestVariationsAreStableAndOftenAccepted keeps the generator honest. The cluster test holds the schema to "refuses
// nothing the broker accepts" on the variations, which proves something only while the generator repeats (a failure
// must reproduce) and keeps making objects that the broker accepts, and ones it refuses: a generator whose
// mutations all broke a rule would pass the test on nothing.
func TestVariationsAreStableAndOftenAccepted(t *testing.T) {
	all, err := Variations(VariationSeed, VariationCount)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Variations(VariationSeed, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := range again {
		if all[i].Name != again[i].Name {
			t.Fatalf("variation %d is not the same for the same seed:\n%s\n%s", i, all[i].Name, again[i].Name)
		}
	}
	other, err := Variations(VariationSeed+1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if slices.EqualFunc(again, other, func(a, b Entry) bool { return a.Name == b.Name }) {
		t.Error("a different seed gives the same variations")
	}

	// The first 1500 are enough to see the proportions, and the broker's file loader is slow under the race detector;
	// the cluster test sends all of them.
	sample := all[:1500]
	accepted, refused := map[string]int{}, map[string]int{}
	for _, e := range sample {
		if Broker(t.Context(), e) == nil {
			accepted[e.Kind]++
		} else {
			refused[e.Kind]++
		}
	}
	t.Logf("broker accepts %v and refuses %v of the first %d variations", accepted, refused, len(sample))
	for _, kind := range []string{"AgentBackend", "Binding"} {
		n := accepted[kind] + refused[kind]
		if n == 0 {
			t.Fatalf("no %s variations", kind)
		}
		// Enough of each for the schema to be tested on what the broker accepts, and enough of the others for the
		// rules to be shown to fire at all.
		if accepted[kind]*10 < n || refused[kind]*10 < n {
			t.Errorf("%s: the broker accepts %d and refuses %d of %d variations; each must be at least a tenth", kind, accepted[kind], refused[kind], n)
		}
	}
}
