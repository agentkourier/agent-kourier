package redact

import (
	"flag"
	"fmt"
	"hash/fnv"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

// The leak oracle. It plants a unique marker as the secret value of each field of a generated input (see leakgen_test.go) and
// fails when an entry point shows one that its field's shape says must be hidden, at full length and, for the Cut entries, at
// every cut limit. What must be hidden comes from the shape of the field and never from the code under test; what main does not
// hide is in leaklimits_test.go, one entry per way it leaks, each pinned so that a fix shows.
//
// A work package that changes how redaction reads a value runs it as its leak gate:
//
//	go test -race ./internal/redact                                                   the sample, the timings, the fuzz seeds
//	go test ./internal/redact -run TestLeakOracleFull -leak.full [-leak.extra] -timeout 1h   every field, pair and triple, at every cut limit
//
// A work package that fixes a way main leaks deletes its row of leaklimits_test.go, or narrows the row's predicate, and nothing
// else: the oracle holds every row to the inputs where main shows a secret, and a row never excuses what a change makes leak. A
// work package that adds a key form adds it to the generator (leakExtra shows how). To see what the oracle reports on another
// version of the package, overlay its non-test files over the working tree with "go test -overlay" (a JSON file that maps each path of
// internal/redact to the other copy), and add -leak.extra.all to put the key forms that main does not read in every set.

var (
	leakFull      = flag.Bool("leak.full", false, "run the whole leak sweep (every pair of fields at every cut limit, and the three-field set) instead of the sample")
	leakExtraKeys = flag.Bool("leak.extra", false, "also check the key forms main does not read (emphasis, backticks, :=), alone and in a wrap")
	leakExtraAll  = flag.Bool("leak.extra.all", false, "put those key forms in every set, pairs and triples too, to run the oracle on a branch that reads them (main leaks them, and the run fails)")
)

// leakEntry is an entry point of the package, as the oracle calls it. fn gets the limit of the Cut entries and ignores it
// otherwise.
type leakEntry struct {
	name  string
	ident bool // IdentifierText: only what has a shape of its own is required hidden
	line  bool // the entries that join the lines also read a value that a line break parts from its name
	cut   bool
	fn    func(s string, limit int) string
}

var leakEntries = []leakEntry{
	{"FreeText", false, false, false, func(s string, _ int) string { return FreeText(s) }},
	{"FreeTextLine", false, true, false, func(s string, _ int) string { return FreeTextLine(s) }},
	{"FreeTextCut", false, false, true, FreeTextCut},
	{"FreeTextCutLine", false, true, true, FreeTextCutLine},
	{"IdentifierText", true, false, false, func(s string, _ int) string { return IdentifierText(s) }},
}

// leakFinding is a planted marker that an entry showed (kind leaked), or a known limit that applies to a marker that the entry hid
// (kind tooBroad: the limit says main leaks it there, and it does not).
type leakFinding struct {
	c     leakCase
	m     leakMarker
	entry string
	limit int    // the cut limit, or 0 at full length
	out   string // what the entry returned
	known string // the known limit that applies to the slot (see leakLimits), or ""
	broad bool   // the limit applies and the entry hid the marker
}

// requiredFrom reports whether the case requires the marker hidden from the entry.
func (m leakMarker) requiredFrom(e leakEntry) bool {
	switch {
	case e.ident:
		return m.ident
	case e.line:
		return m.line
	}
	return m.free
}

// noCut is a cut limit that cuts nothing.
const noCut = 1 << 20

// leakSlot is a marker of a case, an entry that is required to hide it, and the limit the entry reads (noCut for one that is not
// cut), with the text the entry reads: for a Cut entry, the text the token layer leaves, cut at limit bytes (the cut comes after the
// token layer, which can make the text longer or shorter) and whether it was cut.
type leakSlot struct {
	c     leakCase
	m     leakMarker
	e     leakEntry
	limit int
	read  string
	cut   bool
}

// cutFactor is how many times limit bytes of a text a Cut entry reads as it came, before the token layer (cutRawFactor of text.go; the
// text is cut there to bound the work). TestLeakCutPipelineIsWhatTheOracleAssumes holds the model to the entries.
const cutFactor = 4

// cutRead returns what a Cut entry reads of a case at a limit: the raw text cut at cutFactor times limit bytes, the token layer
// run on it, and the result cut at limit bytes; and whether that cut anything (the raw cut counts). rs is the token layer on the whole text, which is
// the same unless the raw cut is short of it. The generator's text is ASCII. The token layer is run only to see where the cut
// falls: what it reads decides when a limit is too broad, never what the entry is excused of.
func cutRead(text, rs string, limit int) (string, bool) {
	capped := len(text) > cutFactor*limit
	if capped {
		rs = Normalize(rawSecrets(text[:cutFactor*limit]))
	}
	if len(rs) <= limit {
		return rs, capped
	}
	return rs[:limit], true
}

// checkLeaks runs the entries over the case, at full length and, for the Cut entries, at every cut limit from markerWindow bytes up
// to past the end of the text (more than the text is long, by the most the token layer lengthens it, or what it leaves if that is
// longer): the limits come from the input alone, so that a Cut entry that leaks a marker where the model of the entry's reading
// says it cannot is still seen. A slot is a marker the case requires hidden from an entry at a limit. A slot that shows its marker
// is a finding, explained if a known limit applies to it (and none is, where the model says the entry reads none of the marker: that
// leak is the cut's); a slot that hides its marker with a known limit applying is a finding too
// (the limit is too broad), but only where the model says the entry reads a window of the marker. So a known limit never excuses a
// marker that main hides, and what the model of the cut says can cause a false failure and never an excuse.
func checkLeaks(entries []leakEntry, c leakCase) []leakFinding {
	rs := Normalize(rawSecrets(c.text))
	hi := max(len(c.text)+64, len(rs))
	if c.wrap == leakLongWrap.id { // the text past the last marker only makes the raw cut short of the text; it is cut, not scanned
		last := 0
		for _, m := range c.markers {
			last = max(last, m.pos+len(m.text))
		}
		hi = min(hi, last+64+24*len(c.atoms))
	}
	var out []leakFinding
	for _, e := range entries {
		limits := []int{noCut}
		if e.cut {
			for l := markerWindow; l <= hi; l++ {
				limits = append(limits, l)
			}
		}
		for _, l := range limits {
			got := e.fn(c.text, l)
			slot := leakSlot{c: c, e: e, limit: l, read: c.text}
			if l != noCut {
				slot.read, slot.cut = cutRead(c.text, rs, l)
			}
			for _, m := range c.markers {
				if !m.requiredFrom(e) {
					continue
				}
				shown := m.shownIn(got)
				read := l == noCut || m.shownIn(slot.read)
				if !shown && !read {
					continue // the entry hides it and the model says it reads too little of it: no row is claimed to apply here
				}
				slot.m = m
				known := ""
				if read { // a marker shown where the model says the entry never read it is the cut's leak, and no row explains that
					known = knownLimit(slot)
				}
				if !shown && known == "" {
					continue
				}
				f := leakFinding{c: c, m: m, entry: e.name, out: got, known: known, broad: !shown}
				if l != noCut {
					f.limit = l
				}
				out = append(out, f)
			}
		}
	}
	return out
}

// leakResult is what a sweep found: the leaks that no known limit explains and the limits that apply where main hides the marker,
// and how many leaks each known limit explained.
type leakResult struct {
	unexplained []leakFinding
	tooBroad    []leakFinding
	explained   map[string]leakTally
	total       int
}

// leakTally counts the slots that a known limit explains and digests them: the sum and the exclusive or of a 64-bit hash of the key of
// each slot (its case, its marker, its entry and its limit), which do not depend on the order the slots are found in. The pins of
// leakpins_test.go hold a row to exactly the slots it explained when they were made.
type leakTally struct {
	n        int
	sum, xor uint64
}

func (t *leakTally) add(key string) {
	h := fnv.New64a()
	h.Write([]byte(key))
	v := h.Sum64()
	t.n++
	t.sum += v
	t.xor ^= v
}

func (t *leakTally) merge(o leakTally) {
	t.n += o.n
	t.sum += o.sum
	t.xor ^= o.xor
}

func (t leakTally) digest() string { return fmt.Sprintf("%016x%016x", t.sum, t.xor) }

// sweepLeaks checks the cases on workers goroutines. The cases are independent.
func sweepLeaks(entries []leakEntry, cases []leakCase, workers int) leakResult {
	res := leakResult{explained: map[string]leakTally{}}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		next int
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local, broad []leakFinding
			explained := map[string]leakTally{}
			total := 0
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= len(cases) {
					break
				}
				ck := cases[i].String()
				for _, f := range checkLeaks(entries, cases[i]) {
					switch {
					case f.broad:
						broad = append(broad, f)
					case f.known != "":
						total++
						t := explained[f.known]
						t.add(fmt.Sprintf("%s|%d.%d|%s|%d", ck, f.m.atom, f.m.idx, f.entry, f.limit))
						explained[f.known] = t
					default:
						total++
						local = append(local, f)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			res.unexplained = append(res.unexplained, local...)
			res.tooBroad = append(res.tooBroad, broad...)
			res.total += total
			for k, n := range explained {
				t := res.explained[k]
				t.merge(n)
				res.explained[k] = t
			}
		}()
	}
	wg.Wait()
	return res
}

// leakSet is one set of cases to check, and what to check them with.
type leakSet struct {
	name    string
	cases   []leakCase
	entries []leakEntry
}

// runLeakSets checks the sets on workers goroutines. It fails t for each distinct way an entry shows a secret that no known limit
// explains (with how many times and one example) and for each known limit that applies where main hides the marker ("limit too
// broad"), and returns how many findings each known limit explained.
func runLeakSets(t *testing.T, sets []leakSet, workers int) map[string]leakTally {
	t.Helper()
	explained := map[string]leakTally{}
	for _, set := range sets {
		if len(set.cases) == 0 {
			t.Fatalf("%s: no cases", set.name)
		}
		start := time.Now()
		res := sweepLeaks(set.entries, set.cases, workers)
		t.Logf("%s: %d cases checked in %v, %d leaks, %d of them known limits", set.name, len(set.cases), time.Since(start).Round(time.Millisecond), res.total, res.total-len(res.unexplained))
		for k, n := range res.explained {
			t := explained[k]
			t.merge(n)
			explained[k] = t
		}
		reportGroups(t, set.name, "leaks", res.unexplained, func(f leakFinding) string {
			prev := "-"
			if f.m.atom > 0 {
				prev = f.c.atoms[f.m.atom-1].id
			}
			return fmt.Sprintf("%s after %s in a %s", f.c.atoms[f.m.atom].id, prev, f.c.layout.id)
		})
		reportGroups(t, set.name, "slots where the limit applies and the entry hides the marker", res.tooBroad, func(f leakFinding) string {
			return fmt.Sprintf("limit too broad: %s %s %s", f.known, f.c, f.entry)
		})
	}
	return explained
}

// reportGroups fails t once for each distinct key of the findings, with how many there are, the entries and one example.
func reportGroups(t *testing.T, set, what string, fs []leakFinding, keyOf func(leakFinding) string) {
	t.Helper()
	type group struct {
		n       int
		entries map[string]bool
		ex      leakFinding
	}
	groups := map[string]*group{}
	for _, f := range fs {
		key := keyOf(f)
		g := groups[key]
		if g == nil {
			g = &group{entries: map[string]bool{}, ex: f}
			groups[key] = g
		}
		g.n++
		g.entries[f.entry] = true
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i == 50 {
			t.Errorf("%s: ... and %d more kinds of %s", set, len(keys)-i, what)
			break
		}
		g := groups[k]
		names := make([]string, 0, len(g.entries))
		for n := range g.entries {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Errorf("%s: %s: %d %s in %s\n\texample: %s(%q, limit %d) = %q, marker %s", set, k, g.n, what, strings.Join(names, ", "), g.ex.entry, g.ex.c.text, g.ex.limit, g.ex.out, g.ex.m.text)
	}
}

// fullLength returns the entries with the Cut ones run at full length only.
func fullLength(entries []leakEntry) []leakEntry {
	out := make([]leakEntry, len(entries))
	for i, e := range entries {
		e.cut = false
		out[i] = e
	}
	return out
}

// The sample the default run checks, in CPU time a few seconds under the race detector (a call of an entry takes about 2 ms
// there, and a text is called once at full length and, for the Cut entries, at every cut limit): every field alone, every pair of
// a core alphabet, and a stride over the pairs of the whole alphabet and the triples of the interplay alphabet, each every
// step-th. The cut limits multiply the work of an input by the length of its text, so fewer inputs are checked at every limit
// than at full length. -leak.full checks everything at every limit.
const (
	singleStep      = 4     // single fields, at full length
	singleCutStep   = 48    // single fields, at every cut limit
	wrapSingleStep  = 40    // single fields in a wrap (prose, a tail, more lines), at full length
	wrapPairStep    = 80    // pairs of the core fields in a wrap, at full length
	opSingleStep    = 20    // single fields with another way to write the operator, at full length
	opPairStep      = 17    // pairs of those of the interplay alphabet, at full length
	endCrossStep    = 83    // a field of the end matrix and a core field, at full length
	pairFullStep    = 751   // pairs of the whole alphabet, at full length
	pairCutStep     = 24029 // pairs of the whole alphabet, at every cut limit
	tripleFullStep  = 463   // triples of the interplay alphabet, at full length
	tripleCutStep   = 22013 // triples of the interplay alphabet, at every cut limit
	leakStrideStart = 7     // where the strides start
	sampleWorkers   = 2     // the default run takes two CPUs, as a small CI runner has them; -leak.full takes all
)

// coreFields are the fields whose every pair the default run checks, in every layout: the value forms that run on into what
// follows them, and one of each other kind. A field is named by its style, class, operator and value form, whatever the place of
// its marker.
var coreFields = []struct{ style, class, op, form string }{
	{"plain", "secret", ": ", "word"}, {"plain", "secret", ": ", "tag"}, {"plain", "secret", ": ", "baretag"}, {"plain", "secret", ": ", "block"},
	{"plain", "secret", ": ", "empty"}, {"plain", "secret", ": ", "barecaret"}, {"plain", "secret", ": ", "barebasic"}, {"plain", "oauth", ": ", "word"}, {"plain", "oauth", ": ", "tag"}, {"plain", "oauth", ": ", "empty"},
	{"flag", "secret", " ", "word"}, {"flag", "secret", " ", "empty"}, {"header", "secret", ": ", "basic"}, {"cookie", "secret", ": ", "pairs"}, {"entry", "secret", "newline", "word"}, {"label", "secret", "", "none"},
}

// coreAtoms returns the first field of atoms for each of coreFields.
func coreAtoms(atoms []*leakAtom) []*leakAtom {
	var out []*leakAtom
	for _, c := range coreFields {
		for _, a := range atoms {
			if a.style == c.style && a.class == c.class && a.op == c.op && a.form == c.form && (a.style != "cookie" || a.markers == 2) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// longWrapped returns the fields that the token layer reads without a name (the tokens with a shape of their own, a Bearer and a Basic
// credential, an OAuth field) in a long text, in the space layout: at the limits that matter the raw cut is short of the text.
func longWrapped(atoms []*leakAtom) []leakCase {
	var out []leakCase
	for _, a := range atoms {
		if a.style == "token" && a.form != "url" || a.scheme != "" && a.op == ": " && a.style == "plain" && a.class == "secret" || a.class == "oauth" && a.style == "plain" && a.op == ": " && a.form == "word" {
			out = append(out, wrapCase(newLeakCase([]*leakAtom{a}, leakLayouts[0]), leakLongWrap))
		}
	}
	return out
}

func leakAtoms() []*leakAtom { return leakAlphabet(*leakExtraAll) }

// extraSets are the sets of the key forms that main does not read, which -leak.extra adds: each alone, and in a wrap.
func extraSets() []leakSet {
	extras := leakExtra()
	return []leakSet{
		{"key forms main does not read", leakSingles(extras), leakEntries},
		{"key forms main does not read, wrapped", wrapped(leakSingles(extras)), leakEntries},
	}
}

// sampleOf returns every step-th of cases.
func sampleOf(cases []leakCase, step int) []leakCase {
	var out []leakCase
	for i := 0; i < len(cases); i += step {
		out = append(out, cases[i])
	}
	return out
}

// endSingles are the fields of the end matrix alone, in the layouts where a field stands apart (a line and a table cell).
func endSingles(atoms []*leakAtom) []leakCase {
	return leakSinglesIn(endMatrix(atoms), "space", "cell")
}

// endCross are the fields of the end matrix with each core field, before and after it, in every layout, a stride of them.
func endCross(atoms, core []*leakAtom) []leakCase {
	return leakCross(endMatrix(atoms), core, endCrossStep, 0)
}

// TestLeakEndMatrixCoversEveryPathAndCharacter pins the reach of the end matrix: every internal character ends a value on every
// rule path, and the default run checks each of them alone and with a core field. A path or a character that a change to the
// generator drops from the matrix fails here, not silently.
func TestLeakEndMatrixCoversEveryPathAndCharacter(t *testing.T) {
	atoms := leakAtoms()
	paths := []string{"names", "entry", "flag", "userflag", "cookie", "oauth", "bearer", "basic"}
	want := map[string]bool{}
	for _, p := range paths {
		for _, c := range endChars(p) {
			want[p+" "+c] = true
		}
	}
	for _, c := range []string{"+", "/"} {
		if !slices.Contains(endChars("basic"), c) {
			t.Errorf("a Basic credential is base64 and holds %q", c)
		}
	}
	for _, c := range internalChars {
		if !slices.Contains(endChars("names"), c) {
			t.Errorf("internal character %q is missing from the end matrix", c)
		}
	}
	for _, e := range endPaths {
		if !slices.Contains(paths, e.path) {
			t.Errorf("end path %q is not one of the rule paths", e.path)
		}
	}
	core := coreAtoms(atoms)
	m := endMatrix(atoms)
	check := func(what string, atomsOf func() map[string]bool) {
		got := atomsOf()
		for k := range want {
			if !got[k] {
				t.Errorf("%s: no field ends a value on the %q path with %q", what, strings.Fields(k)[0], strings.SplitN(k, " ", 2)[1])
			}
		}
		for k := range got {
			if !want[k] {
				t.Errorf("%s: unexpected cell %q", what, k)
			}
		}
	}
	check("the matrix", func() map[string]bool {
		got := map[string]bool{}
		for _, a := range m {
			got[a.path+" "+a.endChar] = true
		}
		return got
	})
	for _, e := range endPaths { // every representative has every character it can hold
		for _, c := range endChars(e.path) {
			found := false
			for _, a := range m {
				found = found || a.path == e.path && a.endChar == c && a.style == e.style && a.class == e.class && a.op == e.op && a.form == e.form &&
					strings.Contains(a.text, endPrefix+c+"{0}")
			}
			if !found {
				t.Errorf("end path %s/%s/%q/%s has no field with %q", e.path, e.style, e.op, e.form, c)
			}
		}
	}
	for what, cases := range map[string][]leakCase{"alone": endSingles(atoms), "with a core field": endCross(atoms, core)} {
		check("the default run, "+what, func() map[string]bool {
			got := map[string]bool{}
			for _, c := range cases {
				for _, a := range c.atoms {
					if a.endChar != "" {
						got[a.path+" "+a.endChar] = true
					}
				}
			}
			return got
		})
	}
}

// sampleSets are the sets of the default run.
func sampleSets(t *testing.T, atoms []*leakAtom) []leakSet {
	t.Helper()
	core, mix := coreAtoms(atoms), interplay(atoms)
	if len(core) != len(coreFields) {
		t.Fatalf("coreFields names %d fields and the generator has %d of them", len(coreFields), len(core))
	}
	singles := leakSingles(atoms)
	return []leakSet{
		{"single fields", sampleOf(singles, singleStep), fullLength(leakEntries)},
		{"single fields, cut", sampleOf(singles, singleCutStep), leakEntries},
		{"core pairs", leakPairs(core, 1, 0), fullLength(leakEntries)},
		{"end matrix", endSingles(atoms), fullLength(leakEntries)},
		{"end matrix with core fields", endCross(atoms, core), fullLength(leakEntries)},
		{"wrapped single fields", wrapped(sampleOf(singles, wrapSingleStep)), fullLength(leakEntries)},
		{"wrapped core pairs", wrapped(leakPairs(core, wrapPairStep, 0)), fullLength(leakEntries)},
		{"tokens and credentials in a text the raw cut falls short of", longWrapped(atoms), leakEntries},
		{"other operators", sampleOf(leakSingles(opVariants(atoms)), opSingleStep), fullLength(leakEntries)},
		{"pairs with other operators", leakPairs(opVariants(mix), opPairStep, leakStrideStart), fullLength(leakEntries)},
		{"pairs", leakPairs(atoms, pairFullStep, leakStrideStart), fullLength(leakEntries)},
		{"pairs, cut", leakPairs(atoms, pairCutStep, leakStrideStart), leakEntries},
		{"triples", leakTriples(mix, tripleFullStep, leakStrideStart), fullLength(leakEntries)},
		{"triples, cut", leakTriples(mix, tripleCutStep, leakStrideStart), leakEntries},
	}
}

// TestLeakOracleSample is the default run of the oracle: a sample of the generated set (see the constants above). -leak.full
// runs it all.
func TestLeakOracleSample(t *testing.T) {
	if *leakFull {
		t.Skip("-leak.full runs TestLeakOracleFull")
	}
	explained := runLeakSets(t, sampleSets(t, leakAtoms()), sampleWorkers)
	if !*leakExtraAll {
		checkPins(t, "the sample", explained, samplePins)
	}
}

// TestLeakOracleFull is the whole sweep: every single field, every pair of fields in every layout and every triple of the interplay
// alphabet, each at full length and at every cut limit, on all the CPUs; and then that every known limit explained a leak (one that
// explains none is stale and goes). It takes minutes: go test ./internal/redact -run TestLeakOracleFull -leak.full -timeout 1h.
func TestLeakOracleFull(t *testing.T) {
	if !*leakFull {
		t.Skip("the whole sweep takes minutes: -leak.full")
	}
	atoms := leakAtoms()
	sets := []leakSet{
		{"single fields", leakSingles(atoms), leakEntries},
		{"end matrix", leakSingles(endMatrix(atoms)), leakEntries},
		{"end matrix with core fields", leakCross(endMatrix(atoms), coreAtoms(atoms), 1, 0), leakEntries},
		{"pairs", leakPairs(atoms, 1, 0), leakEntries},
		{"triples", leakTriples(interplay(atoms), 1, 0), leakEntries},
		{"wrapped single fields", wrapped(leakSingles(atoms)), leakEntries},
		{"wrapped pairs of the interplay alphabet", wrapped(leakPairs(interplay(atoms), 1, 0)), leakEntries},
		{"other operators", leakSingles(opVariants(atoms)), leakEntries},
		{"pairs with other operators", leakPairs(opVariants(interplay(atoms)), 1, 0), leakEntries},
	}
	if *leakExtraKeys {
		sets = append(sets, extraSets()...)
	}
	explained := runLeakSets(t, sets, runtime.GOMAXPROCS(0))
	if !*leakExtraAll {
		pins := fullPins
		if *leakExtraKeys {
			pins = mergePins(fullPins, extraPins)
		}
		checkPins(t, "the full sweep", explained, pins)
	}
	for _, l := range leakLimits {
		t.Logf("known limit %-34s explained %d leaks, digest %s", l.id, explained[l.id].n, explained[l.id].digest())
		if explained[l.id].n == 0 && !t.Failed() && !*leakExtraAll && (!l.extra || *leakExtraKeys) {
			t.Errorf("known limit %q explained no leak: it is fixed or stale, move it out of leakLimits", l.id)
		}
	}
}

// The oracle sees a leak: an entry that returns its text as it came shows every marker that must be hidden, in every case, and
// a perfect one shows none.
func TestLeakOracleBites(t *testing.T) {
	leaky := []leakEntry{
		{"leaky", false, false, false, func(s string, _ int) string { return s }},
		{"leakyLines", false, true, false, func(s string, _ int) string { return s }},
		{"leakyIdentifier", true, false, false, func(s string, _ int) string { return s }},
	}
	hides := []leakEntry{
		{"hides", false, false, true, func(string, int) string { return "" }},
		{"hidesLines", false, true, false, func(string, int) string { return "" }},
		{"hidesIdentifier", true, false, false, func(string, int) string { return "" }},
	}
	atoms := leakAtoms()
	for _, c := range append(leakSingles(atoms)[:50], leakPairs(atoms, 5003, 0)...) {
		want := 0
		for _, m := range c.markers {
			for _, ok := range []bool{m.free, m.line, m.ident} {
				if ok {
					want++
				}
			}
		}
		if got := len(checkLeaks(leaky, c)); got != want {
			t.Errorf("%s: %q: %d findings from entries that hide nothing, want %d", c, c.text, got, want)
		}
		for _, f := range checkLeaks(hides, c) {
			if !f.broad {
				t.Errorf("%s: %q: a leak from entries that hide everything: %s", c, c.text, f.entry)
			}
		}
	}
}

// A limit that applies where the entry hides the marker is found: a limit that applies to everything fails on an entry that hides
// everything, and does not when the entry shows the marker.
func TestLeakOracleSeesALimitThatIsTooBroad(t *testing.T) {
	was := leakLimits
	defer func() { leakLimits = was }()
	leakLimits = []leakLimit{{id: "everything", reason: "x", examples: nil, applies: func(leakSlot) bool { return true }}}
	c := newLeakCase([]*leakAtom{{id: "t", class: "secret", text: "password={0}", markers: 1}}, leakLayouts[0])
	hide := []leakEntry{{"hides", false, false, false, func(string, int) string { return "" }}}
	if fs := checkLeaks(hide, c); len(fs) != 1 || !fs[0].broad || fs[0].known != "everything" {
		t.Errorf("a limit that applies to a hidden marker: findings %v", fs)
	}
	show := []leakEntry{{"shows", false, false, false, func(s string, _ int) string { return s }}}
	if fs := checkLeaks(show, c); len(fs) != 1 || fs[0].broad || fs[0].known != "everything" {
		t.Errorf("a limit that applies to a shown marker: findings %v", fs)
	}
}

// A cut entry that shows a marker at a limit where the model says it reads none of it leaks through the cut itself, and no known limit
// explains that: the limit that applies to the marker where the entry reads it does not excuse it where the entry cannot have.
func TestLeakOracleDoesNotExcuseAMarkerTheEntryCannotRead(t *testing.T) {
	was := leakLimits
	defer func() { leakLimits = was }()
	leakLimits = []leakLimit{{id: "everything", reason: "x", examples: nil, applies: func(leakSlot) bool { return true }}}
	c := newLeakCase([]*leakAtom{{id: "t", class: "secret", text: "password={0}", markers: 1}}, leakLayouts[0])
	show := []leakEntry{{"shows", false, false, true, func(s string, _ int) string { return s }}}
	short, long := 0, 0
	for _, f := range checkLeaks(show, c) {
		switch {
		case f.limit != 0 && f.limit < c.markers[0].pos+markerWindow && f.known != "":
			t.Errorf("limit %d is short of the marker, and a known limit excuses it: %q", f.limit, f.known)
		case f.limit != 0 && f.limit < c.markers[0].pos+markerWindow:
			short++
		case f.known == "everything":
			long++
		}
	}
	if short == 0 || long == 0 {
		t.Errorf("%d findings short of the marker with no excuse, %d past it with one: want some of both", short, long)
	}
}

// The cut limits are all tried: an entry that shows the text cut at one limit and nothing else is caught at that limit, and a
// marker cut short of markerWindow bytes is no leak.
func TestLeakOracleTriesEveryCutLimit(t *testing.T) {
	c := newLeakCase([]*leakAtom{{id: "t", class: "secret", text: "password={0}", markers: 1}}, leakLayouts[0])
	at := c.markers[0].pos + markerWindow
	var seen []int
	probe := []leakEntry{{"probe", false, false, true, func(s string, limit int) string {
		seen = append(seen, limit)
		if limit == at {
			return s[:limit]
		}
		return ""
	}}}
	fs := checkLeaks(probe, c)
	if len(fs) != 1 || fs[0].limit != at {
		t.Fatalf("findings %v, want one at the limit %d", fs, at)
	}
	hi := max(len(c.text)+64, len(Normalize(rawSecrets(c.text))))
	if want := 1 + hi - markerWindow + 1; len(seen) != want {
		t.Errorf("%d limits tried, want %d: full length and every limit from %d to %d", len(seen), want, markerWindow, hi)
	}
}

// The generator's markers can be told apart: every window of markerWindow bytes of a marker is in the text once, as part of the
// marker, and in no key, tag or other value; no window is shared by two markers.
func TestLeakMarkersAreUnique(t *testing.T) {
	atoms := leakAtoms()
	cases := append(leakSingles(atoms), leakPairs(atoms, 211, 0)...)
	for _, c := range append(cases, leakTriples(interplay(atoms), 911, 0)...) {
		for i, m := range c.markers {
			if strings.Count(c.text, m.text) != 1 {
				t.Fatalf("%s: %q does not hold %s once", c, c.text, m.text)
			}
			for w := 0; w+markerWindow <= len(m.text); w++ {
				if n := strings.Count(c.text, m.text[w:w+markerWindow]); n != 1 {
					t.Fatalf("%s: %q holds the window %q of %s %d times", c, c.text, m.text[w:w+markerWindow], m.text, n)
				}
			}
			for _, o := range c.markers[i+1:] {
				if windowIn(m.text, o.text) {
					t.Fatalf("%s: two markers share a window: %s, %s", c, m.text, o.text)
				}
			}
		}
	}
	for i, a := range markerWords {
		for _, b := range markerWords[i+1:] {
			if windowIn(a, b) {
				t.Errorf("the markers %s and %s share a window", a, b)
			}
		}
	}
}

// What the oracle requires is decided by the shape of a field alone: the value of a secret-named key is hidden by the free-text
// entries; IdentifierText must hide an OAuth field's first run and any Bearer or Basic credential; a key that is no secret name has
// no value to hide, but a credential under it is still one.
func TestLeakRequirementsFollowTheShapeOfTheField(t *testing.T) {
	for _, c := range []struct {
		id                string
		free, line, ident bool
	}{
		{`plain/secret/": "/word`, true, true, false},
		{`plain/secret/": "/tag`, true, true, false},
		{`plain/secret/": "/bearer`, true, true, true},
		{`plain/secret/": "/baretag`, false, false, false},
		{`plain/oauth/": "/word`, true, true, true},
		{`plain/oauth/": "/double`, true, true, true},
		{`plain/oauth/": "/tag`, true, true, false},
		{`plain/oauth/": "/block`, true, true, false},
		{`plain/oauth/": "/basic`, true, true, true},
		{`plain/oauth/" => "/word`, true, true, false},
		{`plain/oauth/": "/empty`, false, false, false},
		{`flag/oauth/" "/word`, true, true, false},
		{`flag/oauth/"="/word`, true, true, true},
		{`env/oauth/"="/word`, true, true, true},
		{`json/oauth/":"/double`, true, true, true},
		{`plain/decoy/": "/word`, false, false, false},
		{`plain/decoy/": "/bearer`, true, true, true},
		{`plain/decoy/": "/basic`, true, true, true},
		{`header/secret/": "/word`, true, true, false},
		{`header/decoy/": "/bearer`, true, true, true},
		{`userflag/secret/-u admin:{0}`, true, true, false},
		{`label/secret/token`, false, false, false},
		{`flag/secret/" "/nextline`, false, true, false},
		{`userflag/secret/nextline`, false, true, false},
		{`token/github`, true, true, true},
		{`token/gitlab`, true, true, true},
		{`token/url`, true, true, false},
	} {
		var a *leakAtom
		for _, x := range leakAtoms() {
			if strings.SplitN(x.id, "#", 2)[0] == c.id && a == nil {
				a = x
			}
		}
		if a == nil {
			t.Errorf("no field %s", c.id)
			continue
		}
		if got := a.mustFree(0); got != c.free {
			t.Errorf("%s: free-text entries must hide it: %t, want %t", c.id, got, c.free)
		}
		if got := a.mustLine(0); got != c.line {
			t.Errorf("%s: the entries that join lines must hide it: %t, want %t", c.id, got, c.line)
		}
		if got := a.mustIdent(0); got != c.ident {
			t.Errorf("%s: IdentifierText must hide it: %t, want %t", c.id, got, c.ident)
		}
	}
}

// shownSet returns which markers of the case each entry shows when it reads text, at full length.
func shownSet(c leakCase, text string) string {
	var b strings.Builder
	for _, e := range leakEntries {
		out := e.fn(text, noCut)
		b.WriteString(e.name + ":")
		for _, m := range c.markers {
			if m.shownIn(out) {
				b.WriteString(" " + m.text[:markerWindow])
			}
		}
		b.WriteString(";")
	}
	return b.String()
}

// What leaks does not change with invisible characters or a space of another width: normalization deletes the first and the rules read
// the second as a space (docs/data.md), so a text with a zero-width space or a soft hyphen after each colon and equals sign, with
// "\r\n" or U+2028 for each line break, or with a no-break space for each space, shows the markers that the plain text shows.
func TestLeakInvisiblesDoNotChangeWhatLeaks(t *testing.T) {
	atoms := leakAtoms()
	cases := append(sampleOf(leakSingles(atoms), 41), leakPairs(coreAtoms(atoms), 43, 3)...)
	for _, d := range []struct {
		name string
		do   func(string) string
	}{
		{"zero-width space after the operators", func(s string) string {
			return strings.NewReplacer(":", ":\u200b", "=", "=\u200b").Replace(s)
		}},
		{"soft hyphen before the operators", func(s string) string {
			return strings.NewReplacer(":", "\u00ad:", "=", "\u00ad=").Replace(s)
		}},
		{"CR LF for each line break", func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }},
		{"U+2028 for each line break", func(s string) string { return strings.ReplaceAll(s, "\n", " ") }},
		{"no-break space for each space", func(s string) string { return strings.ReplaceAll(s, " ", " ") }},
	} {
		for _, c := range cases {
			if want, got := shownSet(c, c.text), shownSet(c, d.do(c.text)); got != want {
				t.Errorf("%s: %s: %q shows %s, and %q shows %s", d.name, c, c.text, want, d.do(c.text), got)
				break
			}
		}
	}
}

// modelDropTail is the last run of the text of a Cut entry that was cut, which the entry drops: back to the last white space.
func modelDropTail(s string) string {
	if i := strings.LastIndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i]
	}
	return ""
}

// The oracle models what a Cut entry reads (cutRead: the raw text cut at cutFactor times the limit, the token layer, the cut at the
// limit, and the last run dropped), and uses the model only for the too-broad side of its checks. This holds the model to the entries on
// texts that the rules after the token layer leave alone: if the entries' pipeline changes, it fails here and not by a quiet shift of
// the slots.
func TestLeakCutPipelineIsWhatTheOracleAssumes(t *testing.T) {
	words := strings.Repeat("alpha beta gamma ", 2)
	for _, text := range []string{
		words + "Bearer " + markerWords[0] + " " + words,
		words + "Basic " + markerWords[1] + "AbCdEfGh " + words + "xoxb-1234-" + markerWords[2] + " tail",
		"Bearer " + markerWords[0] + " " + strings.Repeat("padding words follow here ", 6), // 4*limit is short of the text
		strings.Repeat("padding words follow here ", 5) + "Bearer " + markerWords[3] + " end",
		"eyJhbGciOiJIUzI1NiJ9." + markerWords[4] + ".c2lnbmF0dXJl " + strings.Repeat("more words ", 10),
	} {
		rs := Normalize(rawSecrets(text))
		for limit := 1; limit < len(text)+16; limit++ {
			read, cut := cutRead(text, rs, limit)
			want, wantLine := read, strings.Join(strings.Fields(read), " ")
			if cut {
				want, wantLine = modelDropTail(read)+Truncated, modelDropTail(wantLine)+Truncated
			}
			if got := FreeTextCut(text, limit); got != want {
				t.Fatalf("FreeTextCut(%.40q..., %d) = %q, the model reads %q", text, limit, got, want)
			}
			if got := FreeTextCutLine(text, limit); got != wantLine {
				t.Fatalf("FreeTextCutLine(%.40q..., %d) = %q, the model reads %q", text, limit, got, wantLine)
			}
		}
	}
}
