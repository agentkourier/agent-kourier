package store

import (
	"context"
	"fmt"
	"time"
)

// AdmitTriggerRun implements TriggerRuns.
func (g *Guarded) AdmitTriggerRun(ctx context.Context, run TriggerRun, limits TriggerRunLimits) (admission TriggerAdmission, err error) {
	err = g.run(ctx, "courier.store.AdmitTriggerRun", func(ctx context.Context) error {
		if run.Binding == "" || run.Trigger < 0 || !validKey(run.Key) || !validTime(run.At) || limits.DigestWindow <= 0 {
			return fmt.Errorf("admit trigger run of %q: the Binding, trigger, key, a time after the Unix epoch and a digest window are required: %w", run.Binding, ErrInvalid)
		}
		var err error
		admission, err = g.a.AdmitTriggerRun(ctx, run, limits)
		return err
	})
	return admission, err
}

// ListTriggerDigests implements TriggerRuns.
func (g *Guarded) ListTriggerDigests(ctx context.Context, now time.Time) (digests []TriggerDigest, err error) {
	err = g.run(ctx, "courier.store.ListTriggerDigests", func(ctx context.Context) error {
		if !validTime(now) {
			return fmt.Errorf("list trigger digests: the time must be after the Unix epoch: %w", ErrInvalid)
		}
		var err error
		digests, err = g.a.ListTriggerDigests(ctx, now)
		return err
	})
	return digests, err
}

// MarkTriggerDigested implements TriggerRuns.
func (g *Guarded) MarkTriggerDigested(ctx context.Context, binding string, trigger int, start time.Time) error {
	return g.run(ctx, "courier.store.MarkTriggerDigested", func(ctx context.Context) error {
		if binding == "" || trigger < 0 || !validTime(start) {
			return fmt.Errorf("mark trigger digested of %q: the Binding, trigger and a window start after the Unix epoch are required: %w", binding, ErrInvalid)
		}
		return g.a.MarkTriggerDigested(ctx, binding, trigger, start)
	})
}

// PurgeTriggerRuns implements TriggerRuns.
func (g *Guarded) PurgeTriggerRuns(ctx context.Context, before time.Time) (purged int, err error) {
	err = g.run(ctx, "courier.store.PurgeTriggerRuns", func(ctx context.Context) error {
		if !validTime(before) {
			return fmt.Errorf("purge trigger runs: the time must be after the Unix epoch: %w", ErrInvalid)
		}
		var err error
		purged, err = g.a.PurgeTriggerRuns(ctx, before)
		return err
	})
	return purged, err
}

// OpenTriggerThread implements TriggerThreads.
func (g *Guarded) OpenTriggerThread(ctx context.Context, t TriggerThread, stale TriggerStale) (current TriggerThread, opened bool, err error) {
	err = g.run(ctx, "courier.store.OpenTriggerThread", func(ctx context.Context) error {
		if t.Binding == "" || t.KeyHash == "" || t.Thread == "" || !validTime(t.Opened) {
			return fmt.Errorf("open trigger thread of %q: the Binding, key hash, thread and an opening time after the Unix epoch are required: %w", t.Binding, ErrInvalid)
		}
		var err error
		current, opened, err = g.a.OpenTriggerThread(ctx, t, stale)
		return err
	})
	return current, opened, err
}

// MarkTriggerStarted implements TriggerThreads.
func (g *Guarded) MarkTriggerStarted(ctx context.Context, binding, keyHash, thread string, cooldownUntil time.Time) error {
	return g.run(ctx, "courier.store.MarkTriggerStarted", func(ctx context.Context) error {
		if binding == "" || keyHash == "" || thread == "" || !validTime(cooldownUntil) {
			return fmt.Errorf("mark trigger thread of %q started: the Binding, key hash, thread and a cooldown end after the Unix epoch are required: %w", binding, ErrInvalid)
		}
		return g.a.MarkTriggerStarted(ctx, binding, keyHash, thread, cooldownUntil)
	})
}

// ReleaseTriggerThread implements TriggerThreads.
func (g *Guarded) ReleaseTriggerThread(ctx context.Context, binding, keyHash, thread string) (released bool, err error) {
	err = g.run(ctx, "courier.store.ReleaseTriggerThread", func(ctx context.Context) error {
		if binding == "" || keyHash == "" || thread == "" {
			return fmt.Errorf("release trigger thread of %q: the Binding, key hash and thread are required: %w", binding, ErrInvalid)
		}
		var err error
		released, err = g.a.ReleaseTriggerThread(ctx, binding, keyHash, thread)
		return err
	})
	return released, err
}

// GetTriggerThread implements TriggerThreads.
func (g *Guarded) GetTriggerThread(ctx context.Context, binding, keyHash string) (t TriggerThread, err error) {
	err = g.run(ctx, "courier.store.GetTriggerThread", func(ctx context.Context) error {
		if binding == "" || keyHash == "" {
			return fmt.Errorf("get trigger thread of %q: the Binding and key hash are required: %w", binding, ErrInvalid)
		}
		var err error
		t, err = g.a.GetTriggerThread(ctx, binding, keyHash)
		return err
	})
	return t, err
}

// ReopenTriggerThread implements TriggerThreads.
func (g *Guarded) ReopenTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) error {
	return g.run(ctx, "courier.store.ReopenTriggerThread", func(ctx context.Context) error {
		if binding == "" || keyHash == "" || !validTime(at) {
			return fmt.Errorf("reopen trigger thread of %q: the Binding, key hash and a time after the Unix epoch are required: %w", binding, ErrInvalid)
		}
		return g.a.ReopenTriggerThread(ctx, binding, keyHash, at)
	})
}

// CloseTriggerThread implements TriggerThreads.
func (g *Guarded) CloseTriggerThread(ctx context.Context, binding, keyHash string, at time.Time) (closed bool, err error) {
	err = g.run(ctx, "courier.store.CloseTriggerThread", func(ctx context.Context) error {
		if binding == "" || keyHash == "" || !validTime(at) {
			return fmt.Errorf("close trigger thread of %q: the Binding, key hash and a time after the Unix epoch are required: %w", binding, ErrInvalid)
		}
		var err error
		closed, err = g.a.CloseTriggerThread(ctx, binding, keyHash, at)
		return err
	})
	return closed, err
}

// PurgeTriggerThreads implements TriggerThreads.
func (g *Guarded) PurgeTriggerThreads(ctx context.Context, now, idleBefore time.Time) (purged int, err error) {
	err = g.run(ctx, "courier.store.PurgeTriggerThreads", func(ctx context.Context) error {
		if !validTime(now) {
			return fmt.Errorf("purge trigger threads: the time must be after the Unix epoch: %w", ErrInvalid)
		}
		var err error
		purged, err = g.a.PurgeTriggerThreads(ctx, now, idleBefore)
		return err
	})
	return purged, err
}
