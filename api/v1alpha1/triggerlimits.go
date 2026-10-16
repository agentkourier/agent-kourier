package v1alpha1

import "time"

// Bounds of a trigger's limits. A rate window is held to them so that every count the limits need can be
// answered from the last MaxLimitWindow of starts, which is also how long Agent Kourier keeps them (the daily cap is
// a window of this length).
const (
	// MinRateWindow is the shortest rateLimit.per. A shorter one is a typo for a longer unit, and it would
	// make the digest line a message a few seconds apart.
	MinRateWindow = 10 * time.Second
	// MaxLimitWindow is the longest rateLimit.per, and the length of the window of maxRunsPerDay.
	MaxLimitWindow = 24 * time.Hour
	// DefaultDigestWindow is how long a digest line covers for limits that carry no rateLimit.per. A loaded
	// botMessage trigger always has a rateLimit, so config never reaches this default; it is a defensive value for
	// limits built in code.
	DefaultDigestWindow = time.Hour
)

// TriggerLimits bounds how many investigations a botMessage trigger starts (spec 8.2, D26, D31): a rate limit over
// a window and a cap for a day. A botMessage trigger sets both. Only a start is limited: a repeat, a close and a
// note never are.
type TriggerLimits struct {
	// RateLimit allows MaxRuns starts in any window of Per, and MaxConcurrent investigations running at once.
	// Required for a botMessage trigger, with all three fields set.
	// +optional
	RateLimit *RateLimit `json:"rateLimit,omitempty"`
	// MaxRunsPerDay caps starts in any 24 hours (D31). Required for a botMessage trigger: an explicit 0 means
	// no cap.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxRunsPerDay *int32 `json:"maxRunsPerDay,omitempty"`
}

// IsZero says whether no limit is set, so that a trigger with none omits the field.
func (l TriggerLimits) IsZero() bool { return l.RateLimit == nil && l.MaxRunsPerDay == nil }

// DigestWindow is how long one digest line covers for a trigger with these limits: rateLimit.per. The
// DefaultDigestWindow fallback is defensive: a loaded botMessage trigger always has a rateLimit, so it is never
// reached through config.
func (l TriggerLimits) DigestWindow() time.Duration {
	if l.RateLimit != nil && l.RateLimit.Per.Duration > 0 {
		return l.RateLimit.Per.Duration
	}
	return DefaultDigestWindow
}
