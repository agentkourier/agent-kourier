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
// two sizes: n is how many times the line has its pattern, small and large are the n of the two, and bench is the n of a line of
// about 40000 units. The spans grow from about 2.5 kB to 64 times that, and the runs from about 0.7 kB to about 680 times that,
// with 27 times as many runs.
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
	}, 37, 1000, 280},
}

// timed is how long reps runs of Inert on md take, each cut at limit. It starts after a collection, so that the garbage of the
// timing before is not paid for in it.
func timed(md string, limit, reps int) time.Duration {
	runtime.GC()
	start := time.Now()
	for range reps {
		commonmark.Inert(md, limit)
	}
	return time.Since(start)
}

// Inert takes time in proportion to the text. A long line is timed once, against as many runs of a short line as make the same
// number of bytes: in proportion, the two take about as long. A writer that looks for each span's close to the end of the line
// took 61 and 64 times as long on the spans, and 6.5 times as long on the runs, where the work it does for each character
// anyway dilutes the search. The two are timed in turn, eleven times each, and the fastest of each is kept, so that a load on
// the machine, which slows both alike while it lasts, does not move the ratio. Each line is cut one unit short of its whole,
// which reads it twice. The bound is 4.5 times.
func TestInertTakesTimeInProportionToTheText(t *testing.T) {
	if testing.Short() {
		t.Skip("times texts of up to 500 kB")
	}
	for _, tc := range dense {
		small, large := tc.line(tc.small), tc.line(tc.large)
		reps := (len(large) + len(small)/2) / len(small)
		ls := chat.Units(commonmark.Inert(small, 1<<62)) - 1
		ll := chat.Units(commonmark.Inert(large, 1<<62)) - 1
		bs, bl := time.Duration(1<<62), time.Duration(1<<62)
		for range 11 {
			bs = min(bs, timed(small, ls, reps))
			bl = min(bl, timed(large, ll, 1))
		}
		ratio := float64(bl) / float64(bs)
		t.Logf("%s: %d bytes once took %.2f times as long as %d bytes %d times", tc.name, len(large), ratio, len(small), reps)
		if ratio > 4.5 {
			t.Errorf("%s: a line of %d bytes took %.2f times as long as %d lines of %d bytes, over 4.5 times",
				tc.name, len(large), ratio, reps, len(small))
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
