package redact

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The windows the entries are called with in production: an alert field is read up to 12000 bytes and a thread reply up to
// 4000 (docs/data.md). A path that is quadratic in the text takes nine times as long at the one as at the other, and a linear
// one three.
const (
	smallWindow = 4000
	largeWindow = 12000
)

// A run is quadratic if it takes more than quadraticRatio times as long at the large window as at the small one (a linear one
// takes three and a quadratic nine, so six is between them: twice what a linear path takes, and two thirds of what a quadratic one
// does, to leave room for a loaded machine), unless it took under quadraticFloor, and no run may take over slowLimit. A pair over
// suspectRatio is not taken at one measure, in either direction: a loaded machine can push a linear path's ratio up to the
// verdict's and a quadratic one's down to it (5.5 against 6, in RF-1a's audit), so it is measured again and the least of each counts.
const (
	quadraticRatio = 6
	suspectRatio   = 4
	quadraticFloor = 20 * time.Millisecond
	slowLimit      = 10 * time.Second
	extraRuns      = 5
)

// perfEntry is the call that a timing makes: the cut entry, the cut entry for text shown on one line (which reads the text twice,
// with its lines and joined) or the identifier entry.
type perfEntry struct {
	name string
	run  func(s string, window int)
}

var (
	perfCut      = perfEntry{"FreeTextCut", func(s string, n int) { FreeTextCut(s, n) }}
	perfCutLine  = perfEntry{"FreeTextCutLine", func(s string, n int) { FreeTextCutLine(s, n) }}
	perfIdentity = perfEntry{"IdentifierText", func(s string, _ int) { IdentifierText(s) }}
)

// perfShapes are texts of one unit repeated, which is how an agent or an alert makes a rule do the most work: a pair, a tag, a
// header, a table cell, a flag, a block header, a name that is a value. prefix comes once, before the first unit. via says which
// entries are timed on it (all of them, the cost under the race detector being what it is, would take minutes): the cut entry
// on all, and the others where their own work is the shape's.
var perfShapes = []struct {
	name, prefix, unit string
	via                []perfEntry
}{
	{"pairs", "", "password=x ", []perfEntry{perfCut, perfCutLine}},
	{"glued names in a cell", "| token | ", "pwd=", []perfEntry{perfCut, perfCutLine}}, // 3.4 s at 12000 in the round-3 audit of RF-1
	{"cells of pairs", "| token |", " pwd=x |", []perfEntry{perfCut, perfCutLine}},
	{"tags", "password: ", "!x ", []perfEntry{perfCut}},
	{"OAuth pairs", "", "access_token=x ", []perfEntry{perfCut, perfIdentity}},
	{"OAuth tags", "", "id_token: !x ", []perfEntry{perfCut, perfIdentity}},
	{"headers", "", "Authorization: Bearer ", []perfEntry{perfCut, perfIdentity}},
	{"cookies", "Cookie: ", "a=b; ", []perfEntry{perfCut}},
	{"user flags", "", "-u a:b ", []perfEntry{perfCut}},
	{"block headers", "", "password: | ", []perfEntry{perfCut}},
	{"block scalars", "", "password: |\n  x\n", []perfEntry{perfCut, perfCutLine}},
	{"nested names", "", "secret:\n  secret:\n", []perfEntry{perfCut}},
	{"continued flags", "", "--password \\\n  ", []perfEntry{perfCut, perfCutLine}},
	{"env entries", "", "- name: DB_PASSWORD\n", []perfEntry{perfCut}},
	{"open quotes", "", `password="`, []perfEntry{perfCut}},
	{"keys of keys", "", "password: token: ", []perfEntry{perfCut}},
}

// perfInput returns the shape's text, cut to n bytes.
func perfInput(i, n int) string {
	sh := perfShapes[i]
	return (sh.prefix + strings.Repeat(sh.unit, n/len(sh.unit)+1))[:n]
}

// slowOrQuadratic says why the times at the small and the large window are not a linear path's, or returns "": the large is over
// slowLimit, or over ratio times the small (and over quadraticFloor).
func slowOrQuadratic(small, large time.Duration, ratio int) string {
	switch {
	case large > slowLimit:
		return "over " + slowLimit.String()
	case large > time.Duration(ratio)*small && large > quadraticFloor:
		return fmt.Sprintf("more than %d times as long as at the small window, so a quadratic path", ratio)
	}
	return ""
}

// timeWindows times run at the two windows, in the CPU time the process uses (see cpuTime), the small and the large taken in turn,
// runs times, and says whether the pair is linear. A pair over suspectRatio is timed again, extraRuns more times, and the least of
// each counts: a loaded machine can slow a run, and not the least of several.
func timeWindows(run func(window int), runs int) (small, large time.Duration, verdict string) {
	measure := func(tries int) {
		for range tries {
			for _, w := range []int{smallWindow, largeWindow} {
				start := cpuTime()
				run(w)
				d := cpuTime() - start
				switch {
				case w == smallWindow && (small == 0 || d < small):
					small = d
				case w == largeWindow && (large == 0 || d < large):
					large = d
				}
			}
		}
	}
	measure(runs)
	if slowOrQuadratic(small, large, suspectRatio) != "" {
		measure(extraRuns)
	}
	return small, large, slowOrQuadratic(small, large, quadraticRatio)
}

// TestEntriesAreLinearInTheirWindow holds the entries to their work on repeated units at the production windows: the time at 12000
// bytes is under six times the time at 4000, and no run takes ten seconds. A quadratic path fails it: RF-1's table cell of
// pairs took 0.3 s at 4000 and 3.4 s at 12000 where main takes 2 ms and 6 ms. Under -race it takes about 8 s (the entries are
// about 20 times slower there), and it is skipped under -short.
func TestEntriesAreLinearInTheirWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("timing, several seconds under the race detector")
	}
	for i, sh := range perfShapes {
		for _, e := range sh.via {
			small, large, verdict := timeWindows(func(n int) { e.run(perfInput(i, n), n) }, 1)
			t.Logf("%-24s %-16s %10v at %d, %10v at %d", sh.name, e.name, small.Round(time.Microsecond), smallWindow, large.Round(time.Microsecond), largeWindow)
			if verdict != "" {
				t.Errorf("%s through %s: %v at %d bytes and %v at %d: %s", sh.name, e.name, small, smallWindow, large, largeWindow, verdict)
			}
		}
	}
}

// selfTestRuns is how many times the self-test times each loop.
const selfTestRuns = 5

// The check sees a quadratic path and passes a linear one, with the work of a loop standing in for a rule: a linear loop and a
// quadratic one, sized to take milliseconds at the small window. The loops are cheap, so each is timed selfTestRuns times and the least
// of each counts, which keeps the ratios steady on a loaded machine: the quadratic loop's was seen at 5.5 against the verdict's 6 once.
func TestTimingSeesAQuadraticPath(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	var sink int
	linear := func(n int) {
		sum := 0
		for i := 0; i < 6000*n; i++ {
			sum += i
		}
		sink += sum
	}
	quadratic := func(n int) {
		sum := 0
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				sum += i ^ j
			}
		}
		sink += sum
	}
	small, large, verdict := timeWindows(linear, selfTestRuns)
	t.Logf("linear loop: %v at %d, %v at %d, %.1f times", small, smallWindow, large, largeWindow, float64(large)/float64(small))
	if verdict != "" {
		t.Errorf("a linear loop: %v at %d, %v at %d: %s", small, smallWindow, large, largeWindow, verdict)
	}
	small, large, verdict = timeWindows(quadratic, selfTestRuns)
	t.Logf("quadratic loop: %v at %d, %v at %d, %.1f times", small, smallWindow, large, largeWindow, float64(large)/float64(small))
	if verdict == "" {
		t.Errorf("a quadratic loop passed: %v at %d, %v at %d", small, smallWindow, large, largeWindow)
	}
	_ = sink
}
