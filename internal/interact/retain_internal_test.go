package interact

import (
	"testing"
	"time"
)

// TestRetentionRunsEveryTenMinutesWithAnHoursGraceByDefault: configured by nothing, it is the backstop of
// the worker's own cap, which is an hour.
func TestRetentionRunsEveryTenMinutesWithAnHoursGraceByDefault(t *testing.T) {
	rt, err := NewRetention(RetentionConfig{Store: struct{ PressPurger }{}})
	if err != nil {
		t.Fatal(err)
	}
	if rt.cfg.Interval != 10*time.Minute || rt.cfg.Grace != time.Hour {
		t.Errorf("Interval %v, Grace %v; want 10 minutes and an hour", rt.cfg.Interval, rt.cfg.Grace)
	}
}
