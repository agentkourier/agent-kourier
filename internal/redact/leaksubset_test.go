package redact

import (
	"crypto/sha256"
	_ "embed" // the committed slots
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The sample's pins. testdata/leaksample.slots holds two things, and the sample fails unless the run matches both exactly.
//
// The slots: for every row of leakLimits, the 64-bit hash of the key (case, marker, entry, limit) of each slot the row explained when
// the file was made. A row that explains a slot outside its set fails ("outside"): a predicate was widened or a leak added, even when
// the same change also fixes a leak in the row, which a count and a digest cannot see once the fix has lowered the pin. A row whose set
// holds a slot that it no longer explains fails too ("stale"): a leak was fixed or a predicate narrowed. So a fix's re-pin is a diff
// of the file that only deletes lines, which can be checked mechanically, and a leak that comes back after its fix is outside the set.
//
// The composition: the "sample" line, the number of cases of the sample and a digest over everything that decides what a slot is: the
// oracle's parameters (the detection window, the length of a marker, the raw cut factor), each set's name and entries (names and flags),
// each case's String() and text (so the generator's seed and wording), and each marker's position and what it must be hidden from (the
// generator's mustFree, mustLine and mustIdent). The slots mean nothing for another sample: a sample with fewer cases, other steps, another
// seed or a marker that no longer has to be hidden explains other slots, and would pass a subset check, or look like a fix when the slots
// only go. Any change of the composition fails, and only -leak.pins.resample takes a new one. What the digest leaves out is held by tests:
// the range of cut limits (TestLeakOracleTriesEveryCutLimit), the detection (TestLeakOracleBites) and which marker an entry must hide
// (TestLeakOracleBites counts them from the flags).
//
// A stale slot is a fix only if non-test files of internal/redact changed. A diff of this file that only deletes lines is also what an
// oracle that checks less leaves (a marker dropped from what is checked, a predicate that no longer reaches a slot), so the review reads
// the production diff beside it.
//
// The file is generated and is not edited by hand:
//
//	go test ./internal/redact -run TestLeakOracleSample -leak.pins.write                       drops stale slots (a fix); refuses growth
//	go test ./internal/redact -run TestLeakOracleSample -leak.pins.write -leak.pins.widen -v   writes growth too, and logs every new slot
//	go test ./internal/redact -run TestLeakOracleSample -leak.pins.write -leak.pins.resample   takes a new composition
//
// A widening (-leak.pins.widen) or a new composition (-leak.pins.resample) excuses more leaks than before, or checks other inputs than
// before: it needs the orchestrator's sign-off. -leak.pins.out=<path> writes the regeneration there and leaves the tree alone, so that
// a reviewer can regenerate a branch's file and compare it with the committed one. A change of the generator's steps or seed changes
// the composition (it adds, removes or rewrites cases), and an added or removed case adds or removes slots; the keys of the cases that
// stay are the same.

var (
	leakPinsWrite    = flag.Bool("leak.pins.write", false, "rewrite testdata/leaksample.slots from the sample; refuses when a row grows or the composition changed")
	leakPinsWiden    = flag.Bool("leak.pins.widen", false, "with -leak.pins.write: write even when a row grows (a widening, which needs the orchestrator's sign-off); log every new slot with -v")
	leakPinsResample = flag.Bool("leak.pins.resample", false, "with -leak.pins.write: write a new composition of the sample (needs the orchestrator's sign-off)")
	leakPinsOut      = flag.String("leak.pins.out", "", "with -leak.pins.write: write there and not to testdata/leaksample.slots")
)

const sampleSlotsPath = "testdata/leaksample.slots"

//go:embed testdata/leaksample.slots
var sampleSlotsFile string

// slotSets is, for each row of leakLimits, the hashes of the slots it explains.
type slotSets map[string]map[uint64]struct{}

// samplePin is the composition of the sample: how many cases it checks and a digest of what they are.
type samplePin struct {
	cases  int
	digest string
}

func (p samplePin) String() string { return fmt.Sprintf("%d cases, digest %s", p.cases, p.digest) }

// sampleComposition returns the pin of the sets: the digest runs over the oracle's parameters,
// each set's name and entries and, for each case, its String(), its text and what each marker must be hidden from.
func sampleComposition(sets []leakSet) samplePin {
	h := sha256.New()
	put := func(format string, args ...any) { h.Write(fmt.Appendf(nil, format, args...)) }
	put("parameters window=%d length=%d cutfactor=%d nocut=%d\n", markerWindow, markerLen, cutFactor, noCut)
	n := 0
	for _, s := range sets {
		put("set %q %d\n", s.name, len(s.cases))
		for _, e := range s.entries {
			put("entry %q cut=%t line=%t ident=%t\n", e.name, e.cut, e.line, e.ident)
		}
		for _, c := range s.cases {
			put("case %q %q\n", c.String(), c.text)
			for _, m := range c.markers {
				put("marker %d.%d at %d free=%t line=%t ident=%t\n", m.atom, m.idx, m.pos, m.free, m.line, m.ident)
			}
			n++
		}
	}
	return samplePin{n, hex.EncodeToString(h.Sum(nil)[:16])}
}

const sampleSlotsHeader = `# The slots of the leak oracle's sample that each known limit explains: the 64-bit FNV-1a hash of the key of each slot (leakTally),
# and the composition of the sample (the "sample" line). Generated by go test ./internal/redact -run TestLeakOracleSample -leak.pins.write
# -- do not edit (leaksubset_test.go). A row that grows, or a new composition, needs the orchestrator's sign-off; a fix only deletes lines.
`

// formatSampleSlots returns the file for the pin and the sets: the sample line, then the rows in order of their ids, each with its
// count and its hashes in order, one a line, so that a growth is a line in a diff.
func formatSampleSlots(pin samplePin, sets slotSets) string {
	ids := make([]string, 0, len(sets))
	for id, s := range sets {
		if len(s) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString(sampleSlotsHeader)
	fmt.Fprintf(&b, "sample %d %s\n", pin.cases, pin.digest)
	for _, id := range ids {
		hs := make([]uint64, 0, len(sets[id]))
		for h := range sets[id] {
			hs = append(hs, h)
		}
		slices.Sort(hs)
		fmt.Fprintf(&b, "row %s %d\n", id, len(hs))
		for _, h := range hs {
			fmt.Fprintf(&b, "%016x\n", h)
		}
	}
	return b.String()
}

// parseSampleSlots reads a file of formatSampleSlots.
func parseSampleSlots(text string) (samplePin, slotSets, error) {
	var (
		pin    samplePin
		seen   bool
		id     string
		want   int
		sets   = slotSets{}
		noPin  = errors.New("no \"sample\" line before the first row")
		closed = func() error {
			if id != "" && len(sets[id]) != want {
				return fmt.Errorf("the row %q has a count of %d and holds %d", id, want, len(sets[id]))
			}
			return nil
		}
	)
	for i, line := range strings.Split(text, "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "sample "):
			f := strings.Fields(line)
			if len(f) != 3 {
				return pin, nil, fmt.Errorf("line %d: want \"sample <cases> <digest>\", got %q", i+1, line)
			}
			n, err := strconv.Atoi(f[1])
			switch {
			case err != nil || n < 1:
				return pin, nil, fmt.Errorf("line %d: want \"sample <cases> <digest>\", got %q", i+1, line)
			case seen || id != "":
				return pin, nil, fmt.Errorf("line %d: the sample line twice, or after a row", i+1)
			}
			pin, seen = samplePin{n, f[2]}, true
		case strings.HasPrefix(line, "row "):
			if !seen {
				return pin, nil, noPin
			}
			if err := closed(); err != nil {
				return pin, nil, err
			}
			f := strings.Fields(line)
			if len(f) != 3 {
				return pin, nil, fmt.Errorf("line %d: want \"row <id> <count>\", got %q", i+1, line)
			}
			n, err := strconv.Atoi(f[2])
			if err != nil || n < 1 {
				return pin, nil, fmt.Errorf("line %d: bad count in %q", i+1, line)
			}
			if _, dup := sets[f[1]]; dup {
				return pin, nil, fmt.Errorf("line %d: the row %q twice", i+1, f[1])
			}
			id, want = f[1], n
			sets[id] = map[uint64]struct{}{}
		case id == "" || len(line) != 16:
			return pin, nil, fmt.Errorf("line %d: want a row or a 16-digit hash, got %q", i+1, line)
		default:
			h, err := strconv.ParseUint(line, 16, 64)
			if err != nil {
				return pin, nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			if _, dup := sets[id][h]; dup {
				return pin, nil, fmt.Errorf("line %d: the hash %s twice in %q", i+1, line, id)
			}
			sets[id][h] = struct{}{}
		}
	}
	if !seen {
		return pin, nil, noPin
	}
	if err := closed(); err != nil {
		return pin, nil, err
	}
	return pin, sets, nil
}

// committedSample returns the pin and the sets of the committed file.
func committedSample(t *testing.T) (samplePin, slotSets) {
	t.Helper()
	pin, sets, err := parseSampleSlots(sampleSlotsFile)
	if err != nil {
		t.Fatalf("%s: %v (it is generated: -leak.pins.write)", sampleSlotsPath, err)
	}
	return pin, sets
}

// growthKeys returns, for each row, the keys of the slots it explains that are not in its committed set, in order.
func growthKeys(explained map[string]leakTally, committed slotSets) map[string][]string {
	out := map[string][]string{}
	for id, tally := range explained {
		for h, k := range tally.slots {
			if _, ok := committed[id][h]; !ok {
				out[id] = append(out[id], k)
			}
		}
		sort.Strings(out[id])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// slotsOutside says, for each row that explains a slot outside its committed set (or has no set), how many and an example. It is empty
// exactly when every row is a subset of its set.
func slotsOutside(explained map[string]leakTally, committed slotSets) []string {
	growth := growthKeys(explained, committed)
	var out []string
	for _, id := range sortedKeys(growth) {
		keys := growth[id]
		if len(keys) == 0 {
			continue
		}
		what := "outside its committed set of " + strconv.Itoa(len(committed[id]))
		if _, ok := committed[id]; !ok {
			what = "and has no committed set (a new row)"
		}
		out = append(out, fmt.Sprintf("OUTSIDE: the row %q explains %d slots %s, e.g. %q: a predicate was widened or a leak added (regenerating needs the orchestrator's sign-off)", id, len(keys), what, keys[0]))
	}
	return out
}

// slotsStale says, for each row whose committed set holds slots that the sample no longer explains, how many. It is empty exactly when
// every committed slot is explained.
func slotsStale(explained map[string]leakTally, committed slotSets) []string {
	var out []string
	for _, id := range sortedKeys(committed) {
		n := 0
		for h := range committed[id] {
			if _, ok := explained[id].slots[h]; !ok {
				n++
			}
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("STALE: the row %q holds %d committed slots that the sample no longer explains. If non-test files of internal/redact changed (a leak was fixed), run -leak.pins.write and commit the deletions (a diff that only deletes lines). If they did not, the oracle checks less than before: stop, that needs the orchestrator's sign-off", id, n))
		}
	}
	return out
}

// writeMode is what a regeneration is allowed: failed is that the run already failed.
type writeMode struct{ widen, resample, failed bool }

// slotsToWrite returns the file that a regeneration writes, or why it refuses: the run failed; the composition of the sample is not the
// committed one and resample is not given; a row explains a slot outside its committed set (or a row is new) and widen is not given. A
// shrink needs no flag. A sample that explains nothing writes the sample line alone: that is where the work ends, with every known limit
// fixed.
func slotsToWrite(pin, committedPin samplePin, explained map[string]leakTally, committed slotSets, m writeMode) (string, error) {
	if m.failed {
		return "", errors.New("the sample failed: nothing is written")
	}
	if pin != committedPin && !m.resample {
		return "", fmt.Errorf("the composition of the sample is %v and the committed one is %v: a new composition needs the orchestrator's sign-off, then -leak.pins.resample", pin, committedPin)
	}
	if grown := slotsOutside(explained, committed); len(grown) > 0 && !m.widen {
		return "", fmt.Errorf("a regeneration would excuse more leaks than the committed slots do:\n\t%s\nit is a widening and needs the orchestrator's sign-off, then -leak.pins.widen", strings.Join(grown, "\n\t"))
	}
	sets := slotSets{}
	for id, tally := range explained {
		sets[id] = map[uint64]struct{}{}
		for h := range tally.slots {
			sets[id][h] = struct{}{}
		}
	}
	return formatSampleSlots(pin, sets), nil
}

// checkSampleSlots holds the run to the committed composition and slots, or rewrites the file (-leak.pins.write).
func checkSampleSlots(t *testing.T, pin samplePin, explained map[string]leakTally) {
	t.Helper()
	if !*leakPinsWrite && (*leakPinsWiden || *leakPinsResample || *leakPinsOut != "") {
		t.Fatal("-leak.pins.widen, -leak.pins.resample and -leak.pins.out go with -leak.pins.write")
	}
	committedPin, committed := committedSample(t)
	for _, id := range sortedKeys(explained) {
		t.Logf("sample slots %q: %d explained, %d committed", id, len(explained[id].slots), len(committed[id]))
	}
	if *leakPinsWrite {
		writeSampleSlots(t, pin, committedPin, explained, committed)
		return
	}
	for _, p := range sampleProblems(pin, committedPin, explained, committed) {
		t.Errorf("the sample: %s", p)
	}
}

// sampleProblems says how the run differs from the committed file: first the composition, then the slots outside the sets, then the
// stale ones. It is empty exactly when they are the same.
func sampleProblems(pin, committedPin samplePin, explained map[string]leakTally, committed slotSets) []string {
	var out []string
	if pin != committedPin {
		out = append(out, fmt.Sprintf("COMPOSITION: it is %v and the committed one is %v: the cases, the entries, what a marker must be hidden from or the generator changed, so the committed slots are for another sample (a new composition needs the orchestrator's sign-off: -leak.pins.write -leak.pins.resample)", pin, committedPin))
	}
	out = append(out, slotsOutside(explained, committed)...)
	return append(out, slotsStale(explained, committed)...)
}

// writeSampleSlots rewrites the file (or -leak.pins.out) as slotsToWrite says, and logs every slot that a widening adds.
func writeSampleSlots(t *testing.T, pin, committedPin samplePin, explained map[string]leakTally, committed slotSets) {
	t.Helper()
	text, err := slotsToWrite(pin, committedPin, explained, committed, writeMode{widen: *leakPinsWiden, resample: *leakPinsResample, failed: t.Failed()})
	if err != nil {
		t.Fatalf("not writing: %v", err)
	}
	growth := growthKeys(explained, committed)
	for _, id := range sortedKeys(growth) {
		if len(growth[id]) == 0 {
			continue
		}
		t.Logf("WIDENING, needs sign-off: the row %q grows by %d slots:", id, len(growth[id]))
		for _, k := range growth[id] {
			t.Logf("\t+ %s", k)
		}
	}
	if pin != committedPin {
		t.Logf("NEW COMPOSITION, needs sign-off: %v, was %v", pin, committedPin)
	}
	path := sampleSlotsPath
	if *leakPinsOut != "" {
		path = *leakPinsOut
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Logf("wrote %s", path)
}

// The committed file is what the formatter writes (so that a file that is out of order, or has a stray line, fails here). A hash added
// by hand is not caught here: if the sample does not explain it, the sample fails it as stale, and if it does (a hash copied from a
// widening) no code can tell, and the review catches it, since the diff of the file adds lines.
func TestSampleSlotsFileIsCanonical(t *testing.T) {
	pin, sets := committedSample(t)
	if got := formatSampleSlots(pin, sets); got != sampleSlotsFile {
		t.Errorf("%s is not what -leak.pins.write writes: it is generated and is not edited by hand", sampleSlotsPath)
	}
}

func TestSampleSlotsParse(t *testing.T) {
	const pin = "sample 3 0123456789abcdef0123456789abcdef\n"
	for _, c := range []struct {
		name, text, err string
	}{
		{"no sample line", "", "no \"sample\" line"},
		{"a row before the sample line", "row a 1\n0000000000000001\n" + pin, "no \"sample\" line"},
		{"a bad sample line", "sample x y\n", "want \"sample <cases> <digest>\""},
		{"the sample line twice", pin + pin, "twice"},
		{"a count that is short", pin + "row a 2\n0000000000000001\n", "count of 2 and holds 1"},
		{"a count that is long", pin + "row a 1\n0000000000000001\n0000000000000002\n", "count of 1 and holds 2"},
		{"a hash before any row", pin + "0000000000000001\n", "want a row or a 16-digit hash"},
		{"a short hash", pin + "row a 1\n1\n", "want a row or a 16-digit hash"},
		{"a hash that is no hash", pin + "row a 1\n000000000000000g\n", "invalid syntax"},
		{"a row twice", pin + "row a 1\n0000000000000001\nrow a 1\n0000000000000002\n", "twice"},
		{"a hash twice", pin + "row a 2\n0000000000000001\n0000000000000001\n", "twice"},
		{"an empty row", pin + "row a 0\n", "bad count"},
	} {
		if _, _, err := parseSampleSlots(c.text); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: error %v, want one with %q", c.name, err, c.err)
		}
	}
	in := slotSets{"b": {3: {}, 1: {}}, "a": {2: {}}, "empty": {}}
	gotPin, out, err := parseSampleSlots(formatSampleSlots(samplePin{7, "abc"}, in))
	if err != nil || gotPin != (samplePin{7, "abc"}) || len(out) != 2 || len(out["a"]) != 1 || len(out["b"]) != 2 {
		t.Errorf("round trip: %v, %v, %v", gotPin, out, err)
	}
}

// Every row of the committed file is a row of leakLimits: a made-up row, or one left after its entry is gone, fails here and not only
// as stale slots in the sample.
func TestSampleSlotsNameTheRows(t *testing.T) {
	rows := map[string]bool{}
	for _, l := range leakLimits {
		rows[l.id] = true
	}
	_, committed := committedSample(t)
	for _, id := range sortedKeys(committed) {
		if !rows[id] {
			t.Errorf("%s holds slots of %q, which is no row of leakLimits: regenerate the file (-leak.pins.write)", sampleSlotsPath, id)
		}
	}
}

func tallyOf(slots map[uint64]string) leakTally { return leakTally{n: len(slots), slots: slots} }

// A row is held exactly to its committed set. The same slots pass. A fix (fewer slots) fails as stale, never as outside; one slot
// outside fails as outside, alone or together with a fix in the same row (the count then equals the pin's or is below it, which is why
// a count cannot see it), and so does a row with no set.
func TestSampleSlotsAreHeldExactly(t *testing.T) {
	committed := slotSets{"row": {1: {}, 2: {}, 3: {}}}
	for _, c := range []struct {
		name      string
		explained map[string]leakTally
		outside   string // what the outside problems say, or "" for none
		stale     string // what the stale problems say, or "" for none
	}{
		{"the same slots", map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 2: "k2", 3: "k3"})}, "", ""},
		{"a fix: fewer slots", map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1"})}, "", `holds 2 committed slots that the sample no longer explains`},
		{"a row that explains nothing", map[string]leakTally{"row": tallyOf(nil)}, "", `holds 3 committed slots`},
		{"a row that is gone", map[string]leakTally{}, "", `holds 3 committed slots`},
		{"a widening", map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 2: "k2", 3: "k3", 4: "k4"})}, `explains 1 slots outside its committed set of 3, e.g. "k4"`, ""},
		{"a fix and a widening in one row", map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 4: "k4"})}, `e.g. "k4"`, `holds 2 committed slots`},
		{"a slot that moves to another row", map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 3: "k3"}), "other": tallyOf(map[uint64]string{2: "k2"})}, `the row "other" explains 1 slots and has no committed set`, `holds 1 committed slots`},
	} {
		for what, got := range map[string]struct {
			problems []string
			want     string
		}{
			"outside": {slotsOutside(c.explained, committed), c.outside},
			"stale":   {slotsStale(c.explained, committed), c.stale},
		} {
			switch {
			case got.want == "" && len(got.problems) != 0:
				t.Errorf("%s: %s problems %v, want none", c.name, what, got.problems)
			case got.want != "" && (len(got.problems) != 1 || !strings.Contains(got.problems[0], got.want)):
				t.Errorf("%s: %s problems %v, want one with %q", c.name, what, got.problems, got.want)
			}
		}
	}
}

// What the oracle checks is the sample's composition: a case, a text, an entry's cut flag or a set's name that differs, a case more or
// fewer, or a marker that must be hidden from other entries or stands elsewhere, is another digest. (A seed is in the text of every marker.)
func TestSampleCompositionSeesWhatTheSampleIs(t *testing.T) {
	atoms := leakAtoms()
	cases := leakSingles(atoms)[:6]
	base := []leakSet{{"a", cases[:3], leakEntries}, {"b", cases[3:], fullLength(leakEntries)}}
	want := sampleComposition(base)
	if again := sampleComposition(base); again != want || want.cases != 6 {
		t.Fatalf("the composition of one sample is %v then %v, want 6 cases", want, again)
	}
	reseeded := slices.Clone(cases)
	reseeded[2].text = strings.Replace(reseeded[2].text, reseeded[2].markers[0].text, "ZZZZZZZZZZZZ", 1)
	// a marker that must no longer be hidden, or that is somewhere else, with the text as it was
	marker := func(change func(m *leakMarker)) []leakCase {
		out := slices.Clone(cases)
		out[1].markers = slices.Clone(out[1].markers)
		change(&out[1].markers[0])
		return out
	}
	for name, other := range map[string][]leakSet{
		"a case fewer":                      {{"a", cases[:2], leakEntries}, {"b", cases[3:], fullLength(leakEntries)}},
		"a case's text":                     {{"a", reseeded[:3], leakEntries}, {"b", cases[3:], fullLength(leakEntries)}},
		"a set's name":                      {{"a", cases[:3], leakEntries}, {"c", cases[3:], fullLength(leakEntries)}},
		"an entry's cut":                    {{"a", cases[:3], fullLength(leakEntries)}, {"b", cases[3:], fullLength(leakEntries)}},
		"an entry fewer":                    {{"a", cases[:3], leakEntries[:2]}, {"b", cases[3:], fullLength(leakEntries)}},
		"the sets reversed":                 {base[1], base[0]},
		"a marker's free-text requirement":  {{"a", marker(func(m *leakMarker) { m.free = !m.free })[:3], leakEntries}, base[1]},
		"a marker's line requirement":       {{"a", marker(func(m *leakMarker) { m.line = !m.line })[:3], leakEntries}, base[1]},
		"a marker's identifier requirement": {{"a", marker(func(m *leakMarker) { m.ident = !m.ident })[:3], leakEntries}, base[1]},
		"a marker's position":               {{"a", marker(func(m *leakMarker) { m.pos++ })[:3], leakEntries}, base[1]},
	} {
		if got := sampleComposition(other); got == want {
			t.Errorf("%s: the composition is %v, the same as the sample's", name, got)
		}
	}
}

// A regeneration writes only what the rules say: a shrink writes; growth refuses without widen and writes with it; another
// composition refuses without resample; a failed run refuses whatever the flags; a sample that explains nothing writes the sample line.
func TestSlotsToWrite(t *testing.T) {
	committed := slotSets{"row": {1: {}, 2: {}}}
	pin, other := samplePin{10, "aa"}, samplePin{11, "bb"}
	same := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 2: "k2"})}
	shrunk := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1"})}
	grown := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 2: "k2", 3: "k3"})}
	fixAndGrown := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 3: "k3"})}
	newRow := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1"}), "new": tallyOf(map[uint64]string{9: "k9"})}
	for _, c := range []struct {
		name      string
		pin       samplePin
		explained map[string]leakTally
		m         writeMode
		refuses   string         // "" if it writes
		rows      map[string]int // the rows written and their sizes
	}{
		{"the same", pin, same, writeMode{}, "", map[string]int{"row": 2}},
		{"a shrink", pin, shrunk, writeMode{}, "", map[string]int{"row": 1}},
		{"growth", pin, grown, writeMode{}, "more leaks than the committed slots do", nil},
		{"growth with widen", pin, grown, writeMode{widen: true}, "", map[string]int{"row": 3}},
		{"a fix and growth", pin, fixAndGrown, writeMode{}, "more leaks", nil},
		{"a fix and growth with widen", pin, fixAndGrown, writeMode{widen: true}, "", map[string]int{"row": 2}},
		{"a new row", pin, newRow, writeMode{}, "more leaks", nil},
		{"a new row with widen", pin, newRow, writeMode{widen: true}, "", map[string]int{"row": 1, "new": 1}},
		{"another composition", other, same, writeMode{}, "composition", nil},
		{"another composition with widen only", other, same, writeMode{widen: true}, "composition", nil},
		{"another composition with resample", other, same, writeMode{resample: true}, "", map[string]int{"row": 2}},
		{"nothing explained", pin, map[string]leakTally{}, writeMode{}, "", map[string]int{}},
		{"a failed run", pin, shrunk, writeMode{failed: true}, "failed", nil},
		{"a failed run with every flag", other, grown, writeMode{widen: true, resample: true, failed: true}, "failed", nil},
	} {
		text, err := slotsToWrite(c.pin, pin, c.explained, committed, c.m)
		if c.refuses != "" {
			if err == nil || !strings.Contains(err.Error(), c.refuses) || text != "" {
				t.Errorf("%s: wrote %d bytes, error %v, want a refusal with %q", c.name, len(text), err, c.refuses)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
			continue
		}
		gotPin, sets, err := parseSampleSlots(text)
		if err != nil || gotPin != c.pin || len(sets) != len(c.rows) {
			t.Errorf("%s: wrote %v, %v, %v, want the pin %v and rows %v", c.name, gotPin, sets, err, c.pin, c.rows)
			continue
		}
		for id, n := range c.rows {
			if len(sets[id]) != n {
				t.Errorf("%s: the row %q has %d slots, want %d", c.name, id, len(sets[id]), n)
			}
		}
	}
}

// A run is compared with the committed file in every way: the composition, the slots outside the sets and the stale ones each make a
// problem of their own, and the problems come together.
func TestSampleProblems(t *testing.T) {
	committed := slotSets{"row": {1: {}, 2: {}, 3: {}}}
	pin, other := samplePin{10, "aa"}, samplePin{11, "bb"}
	same := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 2: "k2", 3: "k3"})}
	fixed := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 2: "k2"})}
	grown := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 2: "k2", 3: "k3", 4: "k4"})}
	fixedAndGrown := map[string]leakTally{"row": tallyOf(map[uint64]string{1: "k1", 2: "k2", 4: "k4"})}
	for _, c := range []struct {
		name      string
		pin       samplePin
		explained map[string]leakTally
		want      []string // what each problem says, in order
	}{
		{"nothing differs", pin, same, nil},
		{"the composition", other, same, []string{"COMPOSITION: it is 11 cases, digest bb and the committed one is 10 cases, digest aa"}},
		{"a slot outside", pin, grown, []string{`OUTSIDE: the row "row" explains 1 slots outside its committed set of 3, e.g. "k4"`}},
		{"a stale slot", pin, fixed, []string{`STALE: the row "row" holds 1 committed slots that the sample no longer explains. If non-test files of internal/redact changed`}},
		{"a slot outside and a stale one", pin, fixedAndGrown, []string{"OUTSIDE:", "STALE:"}},
		{"all three", other, fixedAndGrown, []string{"COMPOSITION:", "OUTSIDE:", "STALE:"}},
	} {
		got := sampleProblems(c.pin, pin, c.explained, committed)
		if len(got) != len(c.want) {
			t.Errorf("%s: %d problems %q, want %d", c.name, len(got), got, len(c.want))
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: problem %d is %q, want one with %q", c.name, i, got[i], w)
			}
		}
	}
}
