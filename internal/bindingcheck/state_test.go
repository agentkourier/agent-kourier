package bindingcheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func cond(t ConditionType, s Status, r Reason) Condition {
	return Condition{Type: t, Status: s, Reason: r}
}

// find returns the condition of type t in conds.
func find(t *testing.T, conds []Condition, typ ConditionType) Condition {
	t.Helper()
	for _, c := range conds {
		if c.Type == typ {
			return c
		}
	}
	t.Fatalf("no %s condition in %+v", typ, conds)
	return Condition{}
}

func TestSetMovesLastTransitionTimeOnlyWhenStatusChanges(t *testing.T) {
	s := NewState()
	s.Set("b", t0, cond(BackendReachable, True, ReasonOK))
	get := func() Condition { return find(t, s.Snapshot()["b"], BackendReachable) }
	if c := get(); !c.LastTransitionTime.Equal(t0) || !c.LastProbeTime.Equal(t0) {
		t.Fatalf("new condition times = %v, %v; want both %v", c.LastTransitionTime, c.LastProbeTime, t0)
	}

	t1 := t0.Add(time.Minute)
	s.Set("b", t1, cond(BackendReachable, True, ReasonOK))
	if c := get(); !c.LastTransitionTime.Equal(t0) || !c.LastProbeTime.Equal(t1) {
		t.Errorf("same status: transition %v probe %v; want %v and %v", c.LastTransitionTime, c.LastProbeTime, t0, t1)
	}

	t2 := t0.Add(2 * time.Minute)
	s.Set("b", t2, cond(BackendReachable, False, ReasonAgentNotFound))
	if c := get(); !c.LastTransitionTime.Equal(t2) {
		t.Errorf("status change: transition = %v, want %v", c.LastTransitionTime, t2)
	}

	t3 := t0.Add(3 * time.Minute)
	s.Set("b", t3, cond(BackendReachable, False, ReasonAgentForbidden))
	if c := get(); !c.LastTransitionTime.Equal(t2) || c.Reason != ReasonAgentForbidden {
		t.Errorf("reason change: transition = %v reason %s; want %v and AgentForbidden", c.LastTransitionTime, c.Reason, t2)
	}
}

func TestSetIgnoresCallerTimesReadyAndUnknownTypes(t *testing.T) {
	s := NewState()
	c := cond(BackendReachable, True, ReasonOK)
	c.LastTransitionTime, c.LastProbeTime = t0.Add(-time.Hour), t0.Add(-time.Hour)
	s.Set("b", t0, c, cond(Ready, False, "Forced"), cond("Bogus", True, ReasonOK))
	got := s.Snapshot()["b"]
	if len(got) != 2 {
		t.Fatalf("conditions = %+v, want BackendReachable and the derived Ready", got)
	}
	if bc := find(t, got, BackendReachable); !bc.LastTransitionTime.Equal(t0) || !bc.LastProbeTime.Equal(t0) {
		t.Errorf("caller times were kept: %+v", bc)
	}
	if r := find(t, got, Ready); r.Status != True {
		t.Errorf("Ready = %+v, want True: a caller cannot set it", r)
	}
	if n := len(NewState().Set("b", t0, cond("Bogus", True, ReasonOK))); n != 0 {
		t.Errorf("an unknown type gave %d transitions", n)
	}
}

func TestSetStoresAnInvalidStatusAsUnknown(t *testing.T) {
	s := NewState()
	s.Set("b", t0, cond(BackendReachable, "Maybe", ReasonOK))
	if c := find(t, s.Snapshot()["b"], BackendReachable); c.Status != Unknown {
		t.Errorf("status = %q, want Unknown", c.Status)
	}
}

func TestReadyIsDerived(t *testing.T) {
	be, ch, ap := BackendReachable, ChannelJoined, ApproversReadable
	tests := []struct {
		name   string
		conds  []Condition
		status Status
		reason Reason
	}{
		{"all true", []Condition{cond(be, True, ReasonOK), cond(ch, True, ReasonOK)}, True, ReasonOK},
		{"one condition, true", []Condition{cond(ch, True, ReasonOK)}, True, ReasonOK},
		{"optional approvers true", []Condition{cond(be, True, ReasonOK), cond(ch, True, ReasonOK), cond(ap, True, ReasonOK)}, True, ReasonOK},
		{"first failing wins", []Condition{cond(be, False, ReasonAgentNotFound), cond(ch, False, ReasonNotInChannel)}, False, "BackendReachable:AgentNotFound"},
		{"second failing", []Condition{cond(be, True, ReasonOK), cond(ch, False, ReasonNotInChannel)}, False, "ChannelJoined:NotInChannel"},
		{"approvers failing", []Condition{cond(be, True, ReasonOK), cond(ch, True, ReasonOK), cond(ap, False, ReasonGroupNotFound)}, False, "ApproversReadable:GroupNotFound"},
		{"false beats unknown", []Condition{cond(be, Unknown, ReasonPending), cond(ch, False, ReasonChannelArchived)}, False, "ChannelJoined:ChannelArchived"},
		{"pending is unknown", []Condition{cond(be, True, ReasonOK), cond(ch, Unknown, ReasonPending)}, Unknown, "ChannelJoined:Pending"},
		{"unknown names the first unknown", []Condition{cond(be, Unknown, ReasonProbeTimeout), cond(ch, Unknown, ReasonPending)}, Unknown, "BackendReachable:ProbeTimeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewState()
			s.Set("b", t0, tt.conds...)
			conds := s.Snapshot()["b"]
			r := find(t, conds, Ready)
			if r.Status != tt.status || r.Reason != tt.reason {
				t.Errorf("Ready = %s/%s, want %s/%s", r.Status, r.Reason, tt.status, tt.reason)
			}
			if last := conds[len(conds)-1]; last.Type != Ready {
				t.Errorf("Ready is not last: %+v", conds)
			}
		})
	}
}

func TestReadyKeepsTheMessageOfTheConditionHoldingItBack(t *testing.T) {
	s := NewState()
	c := cond(BackendReachable, False, ReasonAgentNotFound)
	c.Message = "card not found"
	s.Set("b", t0, c, cond(ChannelJoined, True, ReasonOK))
	if r := find(t, s.Snapshot()["b"], Ready); r.Message != "card not found" {
		t.Errorf("Ready message = %q", r.Message)
	}
}

func TestSetReturnsTransitionsForStatusOrReasonOnly(t *testing.T) {
	s := NewState()
	trs := s.Set("b", t0, cond(BackendReachable, True, ReasonOK))
	if len(trs) != 2 || trs[0].New.Type != BackendReachable || trs[1].New.Type != Ready {
		t.Fatalf("first Set transitions = %+v, want BackendReachable then Ready", trs)
	}
	if trs[0].Old.Status != "" || trs[0].Binding != "b" {
		t.Errorf("a new condition's Old should be zero and Binding set: %+v", trs[0])
	}

	same := cond(BackendReachable, True, ReasonOK)
	same.Message = "a new message"
	if trs := s.Set("b", t0.Add(time.Minute), same); len(trs) != 0 {
		t.Errorf("a message-only change gave transitions %+v", trs)
	}
	if c := find(t, s.Snapshot()["b"], BackendReachable); c.Message != "a new message" {
		t.Errorf("message not stored: %q", c.Message)
	}

	trs = s.Set("b", t0.Add(2*time.Minute), cond(BackendReachable, False, ReasonUnreachable))
	if len(trs) != 2 || trs[0].Old.Status != True || trs[0].New.Status != False {
		t.Errorf("True to False transitions = %+v", trs)
	}
	trs = s.Set("b", t0.Add(3*time.Minute), cond(BackendReachable, False, ReasonAgentNotFound))
	if len(trs) != 2 || trs[0].Old.Reason != ReasonUnreachable || trs[0].New.Reason != ReasonAgentNotFound {
		t.Errorf("reason-only transitions = %+v (BackendReachable and Ready both change reason)", trs)
	}
}

func TestSnapshotIsADeepCopy(t *testing.T) {
	s := NewState()
	s.Set("b", t0, cond(BackendReachable, True, ReasonOK))
	snap := s.Snapshot()
	snap["b"][0].Status = False
	snap["b"][0].Reason = "Mutated"
	snap["b"] = append(snap["b"], cond(ChannelJoined, True, ReasonOK))
	snap["other"] = nil
	delete(snap, "b")

	again := s.Snapshot()
	if len(again) != 1 || len(again["b"]) != 2 {
		t.Fatalf("State changed through its Snapshot: %+v", again)
	}
	if c := find(t, again["b"], BackendReachable); c.Status != True || c.Reason != ReasonOK {
		t.Errorf("condition changed through its Snapshot: %+v", c)
	}
}

func TestPruneDropsBindingsNotKept(t *testing.T) {
	s := NewState()
	for _, b := range []string{"a", "b", "c"} {
		s.Set(b, t0, cond(BackendReachable, True, ReasonOK))
	}
	gone := s.Prune([]string{"b", "unknown"})
	if fmt.Sprint(gone) != "[a c]" {
		t.Errorf("Prune returned %v, want [a c]", gone)
	}
	if snap := s.Snapshot(); len(snap) != 1 || snap["b"] == nil {
		t.Errorf("after Prune: %+v", snap)
	}
	if gone := s.Prune([]string{"b"}); gone != nil {
		t.Errorf("a second Prune returned %v", gone)
	}
}

func TestStateIsSafeForConcurrentUse(*testing.T) {
	s := NewState()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			b := fmt.Sprintf("b%d", i%3)
			for j := range 200 {
				st := True
				if j%2 == 0 {
					st = False
				}
				s.Set(b, t0.Add(time.Duration(j)*time.Second), cond(BackendReachable, st, ReasonOK), cond(ChannelJoined, True, ReasonOK))
				_ = s.Snapshot()
				if j%50 == 0 {
					s.Prune([]string{"b0", "b1"})
				}
			}
		})
	}
	wg.Wait()
}

func TestAPendingConditionHasNoProbeTimeAndReadyTakesTheLatest(t *testing.T) {
	s := NewState()
	s.Set("b", t0, cond(BackendReachable, Unknown, ReasonPending), cond(ChannelJoined, Unknown, ReasonPending))
	for _, c := range s.Snapshot()["b"] {
		if !c.LastProbeTime.IsZero() {
			t.Errorf("%s is Pending and has LastProbeTime %v", c.Type, c.LastProbeTime)
		}
		if !c.LastTransitionTime.Equal(t0) {
			t.Errorf("%s LastTransitionTime = %v, want %v", c.Type, c.LastTransitionTime, t0)
		}
	}

	t1 := t0.Add(time.Minute)
	s.Set("b", t1, cond(BackendReachable, True, ReasonOK))
	got := s.Snapshot()["b"]
	if c := find(t, got, BackendReachable); !c.LastProbeTime.Equal(t1) {
		t.Errorf("probed condition LastProbeTime = %v, want %v", c.LastProbeTime, t1)
	}
	if c := find(t, got, ChannelJoined); !c.LastProbeTime.IsZero() {
		t.Errorf("still-Pending condition LastProbeTime = %v, want zero", c.LastProbeTime)
	}
	if c := find(t, got, Ready); !c.LastProbeTime.Equal(t1) {
		t.Errorf("Ready LastProbeTime = %v, want the latest probe %v", c.LastProbeTime, t1)
	}
}

// reasonConstants returns the value of every constant of type Reason that conditions.go declares, read from the
// source so that a reason added later is covered without a change here.
func reasonConstants(t *testing.T) []Reason {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "conditions.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []Reason
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Reason" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("Reason constant %v is not a string literal", vs.Names)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, Reason(s))
			}
		}
	}
	if len(out) < 20 {
		t.Fatalf("found only %d Reason constants; the parser lost some", len(out))
	}
	return out
}

// metav1.Condition's Reason must match this pattern (its kubebuilder validation), or the apiserver rejects the status.
var reasonPattern = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

func TestEveryReasonAndEveryReadyReasonMatchTheMetav1Pattern(t *testing.T) {
	reasons := reasonConstants(t)
	for _, r := range reasons {
		if !reasonPattern.MatchString(string(r)) {
			t.Errorf("Reason %q does not match %s", r, reasonPattern)
		}
	}
	for _, typ := range probed {
		for _, r := range reasons {
			for _, st := range []Status{False, Unknown} {
				s := NewState()
				s.Set("b", t0, cond(typ, st, r))
				got := find(t, s.Snapshot()["b"], Ready)
				if got.Status != st || !reasonPattern.MatchString(string(got.Reason)) {
					t.Errorf("Ready from %s %s/%s = %s/%q, want status %s and a matching reason", typ, st, r, got.Status, got.Reason, st)
				}
			}
		}
	}
}
