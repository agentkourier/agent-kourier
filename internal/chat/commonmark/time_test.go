package commonmark_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/commonmark"
)

// dense are lines whose reading costs the most per character: a span every few characters, of ASCII or of CJK, and runs of
// backticks that close nothing, each of another length, so that each would be looked for to the end of the line. Each is made at
// two sizes, of about 2.5 kB and 64 times that: n is how many times the line has its pattern, and bench is the n of a line of
// about 40000 units.
var dense = []struct {
	name         string
	line         func(n int) string
	small, large int
	bench        int
}{
	{"spans of ASCII", func(n int) string { return strings.Repeat("`a`b", n) }, 625, 40000, 10000},
	{"spans of CJK", func(n int) string { return strings.Repeat("`中`中", n) }, 313, 20000, 10000},
	{"runs that close nothing", func(n int) string {
		var b strings.Builder
		for k := n; k > 0; k-- { // each run is longer than every run after it
			b.WriteString(strings.Repeat("`", k))
			b.WriteString("a")
		}
		return b.String()
	}, 70, 566, 280},
}

// fastest is the shortest of five timings of reps runs of Inert on md, each cut one unit short of md's whole length, which reads md
// twice. Each timing starts after a collection, so that the garbage of the one before is not paid for in it.
func fastest(md string, reps int) time.Duration {
	limit := chat.Units(commonmark.Inert(md, 1<<62)) - 1
	best := time.Duration(1<<63 - 1)
	for range 5 {
		runtime.GC()
		start := time.Now()
		for range reps {
			commonmark.Inert(md, limit)
		}
		best = min(best, time.Since(start))
	}
	return best
}

// Inert takes time in proportion to the text. A line 64 times as long is timed once, against as many runs of the short line
// as make the same number of bytes: in proportion, the two take about as long; a cost that grows with the square of a line takes
// 64 times as long, and one that grows with its length times the runs in it eight times. The bound is three times. Both timings are
// of the same work, long enough to measure, on the same machine in the same run, so that neither a slow machine nor the race
// detector moves the ratio.
func TestInertTakesTimeInProportionToTheText(t *testing.T) {
	if testing.Short() {
		t.Skip("times texts of 160 kB")
	}
	for _, tc := range dense {
		small, large := tc.line(tc.small), tc.line(tc.large)
		reps := (len(large) + len(small)/2) / len(small)
		ratio := float64(fastest(large, 1)) / float64(fastest(small, reps))
		t.Logf("%s: %d bytes once took %.2f times as long as %d bytes %d times", tc.name, len(large), ratio, len(small), reps)
		if ratio > 3 {
			t.Errorf("%s: a line of %d bytes took %.2f times as long as %d lines of %d bytes, over three times", tc.name, len(large), ratio, reps, len(small))
		}
	}
}

// BenchmarkInert is Inert on lines of about 40000 units dense with spans or with runs, cut one unit short of the limit, as a live
// update of a long body may be.
func BenchmarkInert(b *testing.B) {
	for _, tc := range dense {
		md := tc.line(tc.bench)
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				commonmark.Inert(md, 39999)
			}
		})
	}
}
