package bindingcheck

import (
	"slices"
	"sync"
	"time"
)

// probed lists the condition types a caller can set, in the order State reports them and picks the first failing
// one for Ready. Ready follows them in a Snapshot.
var probed = []ConditionType{BackendReachable, ChannelJoined, ApproversReadable}

// Transition is a change of one condition of one Binding: of its status, or of its reason.
type Transition struct {
	// Binding is the Binding's name.
	Binding string
	// Old is the condition before the change. It is the zero Condition (empty Status) when the condition is new.
	Old Condition
	// New is the condition after the change.
	New Condition
}

// State holds the conditions of each Binding. It is safe for concurrent use. The zero value is not ready; use NewState.
type State struct {
	mu       sync.RWMutex
	bindings map[string]map[ConditionType]Condition // every condition of a Binding, Ready included
}

// NewState returns an empty State.
func NewState() *State {
	return &State{bindings: map[string]map[ConditionType]Condition{}}
}

// Set records conds for binding, probed at at, and recomputes the Binding's Ready condition. A condition's
// LastProbeTime becomes at, and its LastTransitionTime becomes at only when its Status differs from the one it
// replaces (or it is new). Times in conds are ignored, and so is any condition whose Type is Ready or not one of
// the three probed types. A Status that is not True, False or Unknown is stored as Unknown.
//
// It returns the transitions: one for each condition, Ready included, whose status or reason changed. A change of
// Message alone is stored and is not a transition.
func (s *State) Set(binding string, at time.Time, conds ...Condition) []Transition {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.bindings[binding]
	var out []Transition
	set := func(c Condition) {
		if !c.Status.valid() {
			c.Status = Unknown
		}
		old, had := cur[c.Type]
		c.LastProbeTime, c.LastTransitionTime = at, at
		if had && old.Status == c.Status {
			c.LastTransitionTime = old.LastTransitionTime
		}
		cur[c.Type] = c
		if !had || old.Status != c.Status || old.Reason != c.Reason {
			out = append(out, Transition{Binding: binding, Old: old, New: c})
		}
	}
	for _, c := range conds {
		if !slices.Contains(probed, c.Type) {
			continue
		}
		if cur == nil {
			cur = map[ConditionType]Condition{}
			s.bindings[binding] = cur
		}
		set(c)
	}
	if cur != nil {
		set(derive(cur))
	}
	return out
}

// derive computes the Ready condition from the probed conditions in cur. Ready is False when any is False, True when
// all are, and Unknown otherwise; when it is not True it names the first condition that holds it back, False before
// Unknown. Its times are filled by the caller.
func derive(cur map[ConditionType]Condition) Condition {
	var unknown *Condition
	for _, t := range probed {
		c, ok := cur[t]
		switch {
		case !ok || c.Status == True:
		case c.Status == False:
			return heldBack(c, False)
		case unknown == nil:
			unknown = &c
		}
	}
	if unknown != nil {
		return heldBack(*unknown, Unknown)
	}
	return Condition{Type: Ready, Status: True, Reason: ReasonOK}
}

func heldBack(c Condition, status Status) Condition {
	return Condition{Type: Ready, Status: status, Reason: Reason(string(c.Type) + "/" + string(c.Reason)), Message: c.Message}
}

// Snapshot returns a copy of every Binding's conditions: the probed ones in a fixed order (BackendReachable,
// ChannelJoined, ApproversReadable), then Ready. The caller owns the copy; changing it does not change the State.
func (s *State) Snapshot() map[string][]Condition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]Condition, len(s.bindings))
	for name, cur := range s.bindings {
		conds := make([]Condition, 0, len(cur))
		for _, t := range append(slices.Clone(probed), Ready) {
			if c, ok := cur[t]; ok {
				conds = append(conds, c)
			}
		}
		out[name] = conds
	}
	return out
}

// Prune drops every Binding whose name is not in keep, and returns the names it dropped, sorted. A caller that keeps
// per-Binding series elsewhere (metrics) deletes them for the names returned.
func (s *State) Prune(keep []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []string
	for name := range s.bindings {
		if !slices.Contains(keep, name) {
			gone = append(gone, name)
		}
	}
	for _, name := range gone {
		delete(s.bindings, name)
	}
	slices.Sort(gone)
	return gone
}
