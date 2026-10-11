package v1alpha1

import (
	"cmp"
	"time"
)

// Bounds of a trigger's limits. A rate window is held to them so that every count the limits need can be
// answered from the last MaxLimitWindow of starts, which is also how long Agent Kourier keeps them (the daily cap is
// a window of this length).
const (
	// MinRateWindow is the shortest rateLimit.per. A shorter one is a typo for a longer unit, and it would
	// make the digest line a message a few seconds apart.
	MinRateWindow = 10 * time.Second
	// MaxLimitWindow is the longest rateLimit.per, and the length of the window of maxRunsPerDay.
	MaxLimitWindow = 24 * time.Hour
	// DefaultDigestWindow is how long a digest line covers for a trigger that has a daily cap but no
	// rateLimit, so no rateLimit.per to take the window from.
	DefaultDigestWindow = time.Hour
)

// TriggerLimits bounds how many investigations a botMessage trigger starts (spec 8.2, D26, D31). It has the shape
// of the Binding's alerts.rateLimit and alerts.maxRunsPerDay (spec 7), and the preset supplies the defaults, so a
// Binding sets only what it changes. Only a start is limited: a repeat, a close and a note never are.
type TriggerLimits struct {
	// RateLimit allows MaxRuns starts in any window of Per, and MaxConcurrent investigations running at once.
	// What the Binding leaves out of it comes from the preset, field by field.
	// +optional
	RateLimit *RateLimit `json:"rateLimit,omitempty"`
	// MaxRunsPerDay caps starts in any 24 hours (D31). An explicit 0 means no cap; leaving it out takes the
	// preset's cap.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxRunsPerDay *int32 `json:"maxRunsPerDay,omitempty"`
}

// IsZero says whether no limit is set, so that a trigger with none omits the field and is unlimited.
func (l TriggerLimits) IsZero() bool { return l.RateLimit == nil && l.MaxRunsPerDay == nil }

// withDefaults returns l with what it leaves unset taken from defaults, field by field: a rate limit the Binding
// sets in part gets the rest from the preset, and an unset cap gets the preset's (an explicit 0 is kept: no cap).
// It copies, so the result shares nothing with defaults, and applying it twice changes nothing.
func (l TriggerLimits) withDefaults(defaults TriggerLimits) TriggerLimits {
	out := TriggerLimits{}
	switch {
	case l.RateLimit != nil && defaults.RateLimit != nil:
		out.RateLimit = &RateLimit{
			MaxRuns:       cmp.Or(l.RateLimit.MaxRuns, defaults.RateLimit.MaxRuns),
			MaxConcurrent: cmp.Or(l.RateLimit.MaxConcurrent, defaults.RateLimit.MaxConcurrent),
		}
		out.RateLimit.Per.Duration = cmp.Or(l.RateLimit.Per.Duration, defaults.RateLimit.Per.Duration)
	case l.RateLimit != nil:
		r := *l.RateLimit
		out.RateLimit = &r
	case defaults.RateLimit != nil:
		r := *defaults.RateLimit
		out.RateLimit = &r
	}
	switch {
	case l.MaxRunsPerDay != nil:
		n := *l.MaxRunsPerDay
		out.MaxRunsPerDay = &n
	case defaults.MaxRunsPerDay != nil:
		n := *defaults.MaxRunsPerDay
		out.MaxRunsPerDay = &n
	}
	return out
}

// DigestWindow is how long one digest line covers for a trigger with these limits: rateLimit.per, or
// DefaultDigestWindow if there is none.
func (l TriggerLimits) DigestWindow() time.Duration {
	if l.RateLimit != nil && l.RateLimit.Per.Duration > 0 {
		return l.RateLimit.Per.Duration
	}
	return DefaultDigestWindow
}
