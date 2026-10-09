package redact

import (
	"sort"
	"testing"
)

// The pins hold each known limit to exactly the slots it explains. The full sweep counts, for every row of leakLimits, the slots it
// explained and digests them (leakTally); a run fails if a row explains more slots than its pin, a row with no pin explains any, or a
// digest differs. That is what keeps a later work package from widening a predicate, which the oracle's own check cannot see when the
// widening excuses a leak that the same work package adds. A fix that removes a leak lowers a count and changes a digest too, and so does
// a growth of the generator: re-pinning is a deliberate edit of this file that the orchestrator signs off, from the numbers that the
// run logs ("pin" lines with -v). The default run's sample is held to committed slots instead (leaksubset_test.go), where a fix's
// re-pin is a diff that only deletes lines.
type leakPin struct {
	n      int
	digest string
}

func mergePins(ms ...map[string]leakPin) map[string]leakPin {
	out := map[string]leakPin{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// checkPins fails t for each row whose tally differs from its pin, and says how.
func checkPins(t *testing.T, what string, got map[string]leakTally, pins map[string]leakPin) {
	t.Helper()
	ids := map[string]bool{}
	for id := range got {
		ids[id] = true
	}
	for id := range pins {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	for _, id := range sorted {
		g, p, pinned := got[id], pins[id], false
		_, pinned = pins[id]
		t.Logf("pin %q: {%d, %q},", id, g.n, g.digest())
		switch {
		case !pinned && g.n > 0:
			t.Errorf("%s: the row %q is new: it explains %d slots and has no pin (a new row needs the orchestrator's sign-off)", what, id, g.n)
		case g.n > p.n:
			t.Errorf("%s: the row %q explains %d slots, more than its pin of %d: a predicate was widened or a leak added", what, id, g.n, p.n)
		case g.n < p.n:
			t.Errorf("%s: the row %q explains %d slots, fewer than its pin of %d: a leak was fixed or a predicate narrowed (re-pin it)", what, id, g.n, p.n)
		case pinned && g.digest() != p.digest:
			t.Errorf("%s: the row %q explains as many slots as its pin, but not the same ones (digest %s, pinned %s)", what, id, g.digest(), p.digest)
		}
	}
}

// fullPins are the pins of TestLeakOracleFull without -leak.extra.
var fullPins = map[string]leakPin{
	"at-sign-ends-a-user-password":            {72505, "ea7fdbce69f2c55f8c3c907a2dbb6681"},
	"block-header-takes-the-rest-of-its-line": {1206, "0abe983a5abb8f6f4d784c22753fd3af"},
	"block-header-under-the-name":             {347019, "333d42449717711b263e5fe6d1a945a3"},
	"continuation-takes-the-next-key":         {203428, "3cd128f32cb48942a15936868f8ab980"},
	"doubled-equals-hides-the-mark":           {30116, "0b9e5200cffd5ad572bfa9be9152fe13"},
	"empty-takes-the-next-run":                {128182, "5c066899e35aaeb623758adfcda6bd40"},
	"heredoc-body-is-not-read":                {366868, "86544132fb012ec41e5e14bbfa3b7e2a"},
	"oauth-empty-takes-the-next-key":          {390762, "07f8f341f0b67cf96c8ee4f56063280d"},
	"oauth-hash-rocket":                       {2067360, "7669ef2b384123dc54c3b06cf9ef4faa"},
	"oauth-value-is-the-mark":                 {23106749, "b3308faad285f3b34037bdd0abf98499"},
	"oauth-value-stops-at-a-backslash":        {1088248, "92208c5ebd0e26bd9854da908c746305"},
	"question-equals-is-no-operator":          {42880, "dea44f78be501c4b963ee5ae79084b89"},
	"scheme-takes-the-next-key":               {34814, "a1231dea68635f8e87c4917415e1f67a"},
	"tag-takes-the-next-key":                  {771358, "8dfeeb70219d2556e7afdb9e1f6a41b0"},
	"taken-out-marker-ends-the-value":         {2326000, "d0fd2727697f682aea6639abb4b58cac"},
}

// extraPins are the pins of the rows that only -leak.extra exercises.
var extraPins = map[string]leakPin{
	"key-forms-are-not-read": {223220, "45f7a749d6a166376829c3e72ad629d5"},
}

// TestLeakPinsNameTheRows holds the set of rows: every row of leakLimits has a pin of the full sweep (a row that only -leak.extra
// exercises has one in extraPins), and no pin names a row that is gone. A row added to leakLimits, or one kept after it explains
// nothing, fails here and not only in the sweep that takes minutes.
func TestLeakPinsNameTheRows(t *testing.T) {
	rows := map[string]bool{}
	for _, l := range leakLimits {
		rows[l.id] = true
		_, full := fullPins[l.id]
		_, extra := extraPins[l.id]
		switch {
		case l.extra && (!extra || full):
			t.Errorf("the row %q is exercised only with -leak.extra: its pin belongs in extraPins and not in fullPins", l.id)
		case !l.extra && (!full || extra):
			t.Errorf("the row %q has no pin in fullPins (a new row needs the orchestrator's sign-off)", l.id)
		}
	}
	for name, pins := range map[string]map[string]leakPin{"fullPins": fullPins, "extraPins": extraPins} {
		for id := range pins {
			if !rows[id] {
				t.Errorf("%s pins %q, which is no row of leakLimits: the row is gone, remove its pin", name, id)
			}
		}
	}
}
