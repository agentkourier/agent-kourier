// Package testshards holds the test that keeps ci.yml's split test passes whole: each pass's matrix legs take their
// part of the packages by the strategy context, so together they run every package; each leg's cache has its own key,
// which cache-prune prunes; and the required checks test and test-testhooks pass only when every job they need passed.
// A leg left out of the split, or a check that a skipped or cancelled leg let pass, would drop tests without failing
// anything else.
package testshards
