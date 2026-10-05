package storetest

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// decisionSQL matches the query of docs/data.md that D33 is computed with: the one fenced block of SQL after "```sql".
var decisionSQL = regexp.MustCompile("(?s)```sql\n(WITH rated.*?)```")

// DecisionSQL returns the query that docs/data.md gives for the pilot's exit decision (D33), as written there, so that a store's
// own test can run the very text an operator would.
func DecisionSQL(t testing.TB) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("storetest: cannot find the source of the package")
	}
	doc, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "..", "docs", "data.md"))
	if err != nil {
		t.Fatalf("read docs/data.md: %v", err)
	}
	m := decisionSQL.FindSubmatch(doc)
	if m == nil {
		t.Fatal("docs/data.md has no SQL block for the pilot's exit decision")
	}
	return string(m[1])
}

// SeedDecisionVotes records the votes that the rule of D33 is tried on, and returns what the query must answer: how many
// investigations are rated (any vote on an investigation's message), and how many are useful (more useful votes than not).
// Threads: A has a majority useful; B a tie; C one not useful; D a vote changed to useful; E only an answer's votes, which are
// no investigation; and F two voters on one investigation and a third on an answer in the same thread, whose vote is no part of it.
func SeedDecisionVotes(t testing.TB, s store.Store) (rated, useful int) {
	t.Helper()
	vote := func(thread, user string, kind store.FeedbackKind, v store.Verdict) {
		t.Helper()
		_, err := s.RecordVote(t.Context(), store.FeedbackVote{
			Key: keyOf(thread), Turn: "turn-" + string(kind), User: user, Binding: "ns/b", Agent: "kagent/sre", Alert: "A" + thread,
			Kind: kind, Verdict: v, At: t0,
		})
		if err != nil {
			t.Fatalf("RecordVote(%s, %s): %v", thread, user, err)
		}
	}
	inv, ans := store.FeedbackInvestigation, store.FeedbackAnswer
	for _, v := range []struct {
		thread, user string
		kind         store.FeedbackKind
		verdict      store.Verdict
	}{
		{"A", "U1", inv, store.VerdictUseful}, {"A", "U2", inv, store.VerdictUseful}, {"A", "U3", inv, store.VerdictNotUseful},
		{"B", "U1", inv, store.VerdictUseful}, {"B", "U2", inv, store.VerdictNotUseful},
		{"C", "U1", inv, store.VerdictNotUseful},
		{"D", "U1", inv, store.VerdictNotUseful}, {"D", "U1", inv, store.VerdictUseful},
		{"E", "U1", ans, store.VerdictUseful}, {"E", "U2", ans, store.VerdictUseful},
		{"F", "U1", inv, store.VerdictUseful}, {"F", "U2", inv, store.VerdictUseful}, {"F", "U3", ans, store.VerdictNotUseful}, {"F", "U4", ans, store.VerdictNotUseful},
	} {
		vote(v.thread, v.user, v.kind, v.verdict)
	}
	return 5, 3 // A, B, C, D and F are rated; A, D and F are useful
}
