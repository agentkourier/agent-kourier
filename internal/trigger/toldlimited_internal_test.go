package trigger

import (
	"fmt"
	"testing"
	"time"
)

// toldLimited holds at most maxToldLimited windows. Past that it drops the ones that ended, and if none has, all of them: the cost
// of a flood of distinct people is more audit rows, never a person not told.
func TestToldLimitedIsBoundedAndFailsOpen(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var told toldLimited
	for i := range maxToldLimited {
		if !told.first(fmt.Sprint(i), now, now.Add(time.Hour)) {
			t.Fatalf("person %d was not told the first time", i)
		}
	}
	if told.first("0", now, now.Add(time.Hour)) {
		t.Fatal("person 0 was told twice in one window")
	}
	if !told.first("new", now, now.Add(time.Hour)) || len(told.until) != 1 {
		t.Fatalf("a new person past the bound: told %d windows held; want told, and the full set forgotten", len(told.until))
	}
	if !told.first("0", now, now.Add(time.Hour)) {
		t.Error("person 0 was not told again after the windows were forgotten")
	}

	told = toldLimited{}
	for i := range maxToldLimited {
		told.first(fmt.Sprint(i), now, now.Add(time.Duration(i%2)*time.Hour)) // the even ones' windows have ended
	}
	if !told.first("new", now, now.Add(time.Hour)) || len(told.until) != maxToldLimited/2+1 {
		t.Errorf("held %d windows; want the ended ones dropped and the rest kept", len(told.until))
	}
}
