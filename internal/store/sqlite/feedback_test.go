package sqlite

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store/storetest"
)

// The query that docs/reference/stored-data.md gives for the pilot's exit decision (D33) answers by the rule of the owner: per investigation, the
// majority of its votes, a tie not useful, and only an investigation's own message.
func TestTheDocumentedDecisionQueryCountsInvestigationsByMajority(t *testing.T) {
	s := openTemp(t)
	wantRated, wantUseful := storetest.SeedDecisionVotes(t, s)
	var rated, useful int
	if err := s.a.read.QueryRowContext(t.Context(), storetest.DecisionSQL(t)).Scan(&rated, &useful); err != nil {
		t.Fatal(err)
	}
	if rated != wantRated || useful != wantUseful {
		t.Errorf("the query says %d rated, %d useful; want %d and %d", rated, useful, wantRated, wantUseful)
	}
}
