package chat

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// runLimits is the store's form of a trigger's limits. The digest window is the trigger's rate window.
func runLimits(l v1alpha1.TriggerLimits) store.TriggerRunLimits {
	out := store.TriggerRunLimits{DigestWindow: l.DigestWindow()}
	if r := l.RateLimit; r != nil {
		out.MaxRuns, out.Per, out.MaxConcurrent = int(r.MaxRuns), r.Per.Duration, int(r.MaxConcurrent)
	}
	if l.MaxRunsPerDay != nil {
		out.MaxPerDay = int(*l.MaxRunsPerDay)
	}
	return out
}

// checks says whether any limit is set to check: a cap of 0 is none, and limits with none are not counted. A loaded
// botMessage trigger always has a rate limit, so this is false only for limits built in code.
func checks(l store.TriggerRunLimits) bool {
	return (l.MaxRuns > 0 && l.Per > 0) || l.MaxConcurrent > 0 || l.MaxPerDay > 0
}

// admit asks the trigger's limits whether the match may start a turn, and records the start if it may, in one atomic
// step in the store (so the count survives a restart). It returns what they said: the limit that stops it, if one does. It
// is a local read and write, as the acknowledgement path allows. Asking again for the same message, as a redelivery
// does, is the same start: it is counted once. Without IntakeConfig.TriggerRuns, or with limits that check nothing,
// nothing stops a start; a loaded botMessage trigger always has a rate limit, so the second case arises only for a
// match built in code.
func (in *Intake) admit(ctx context.Context, m TriggerMatch) (store.TriggerAdmission, error) {
	limits := runLimits(m.Limits)
	if in.cfg.TriggerRuns == nil || !checks(limits) {
		return store.TriggerAdmission{Admitted: true}, nil
	}
	run := store.TriggerRun{
		Binding: m.Binding, Trigger: m.Index, At: m.Received,
		Key: store.ConversationKey{Connection: in.conn, Channel: m.Message.At.Channel, Thread: m.Message.At.ID},
	}
	if m.Mention || m.Person { // each holds its place for its own turn alone, not for the later turns of its thread
		run.SessionThread = cmp.Or(m.Message.At.Thread, m.Message.At.ID)
	}
	adm, err := in.cfg.TriggerRuns.AdmitTriggerRun(ctx, run, limits)
	if err != nil {
		return store.TriggerAdmission{}, fmt.Errorf("check the limits of trigger %d of %s for %s: %w", m.Index, m.Binding, m.MessageID, err)
	}
	return adm, nil
}

// stopThread is what a limit does to a fire that reserved a thread: it gives the reservation back and counts the
// decision. The key is as if it had not fired, so the next fire of it may start when there is capacity; and nothing
// is posted, since a note per alert is what a storm must not do (D26). If the release fails the decision is left
// unset and the error returned: the redelivery finds its own reservation and is checked again. A message stopped before is
// a redelivery, and was counted then.
func (in *Intake) stopThread(ctx context.Context, m TriggerMatch, hash string, out decided, adm store.TriggerAdmission) (decided, error) {
	if _, err := in.cfg.TriggerThreads.ReleaseTriggerThread(ctx, m.Binding, hash, m.Message.At.ID); err != nil {
		out.decision = ""
		return out, fmt.Errorf("release the thread of key %s: %w", out.keyHash, err)
	}
	if !adm.Again {
		in.countLimited(ctx, m, adm.Limit)
	}
	out.decision = DecisionLimited
	return out, nil
}

// countLimited counts and logs a start a limit stopped. The log names the Binding, the trigger and the limit and
// where the alert was, and never what it said.
func (in *Intake) countLimited(ctx context.Context, m TriggerMatch, limit store.TriggerLimit) {
	telemetry.Count(ctx, "agentkourier_trigger_limited_total", m.Binding, strconv.Itoa(m.Index), string(limit))
	in.log.InfoContext(ctx, "chat: a trigger's start was not made; a limit is reached", "binding", m.Binding, "trigger", m.Index, "limit", limit, "channel", m.Message.At.Channel, "message", m.Message.At.ID)
}

// maxToldLimited bounds how many people's windows toldLimited holds at once.
const maxToldLimited = 4096

// toldLimited holds, for each person, Binding and trigger, when the window ends in which a stopped start of theirs was told to
// IntakeConfig.OnLimited, so that a flood of one person's reports is audited once for each window of the trigger's rate limit
// (rateLimit.per), not once for each message: the row names the person, and the metric and the digest count the messages. It is
// held in memory, so a restart or a change of leader (a new Intake) may audit one more for a window. Past maxToldLimited windows that
// have not ended it forgets them all: the cost is more audit rows, never fewer.
type toldLimited struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// first says whether the stopped start of key at now is the first of its window, and if so opens a window that ends at until.
func (t *toldLimited) first(key string, now, until time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if end, ok := t.until[key]; ok && now.Before(end) {
		return false
	}
	if t.until == nil || len(t.until) >= maxToldLimited {
		for k, end := range t.until {
			if !now.Before(end) {
				delete(t.until, k)
			}
		}
		if t.until == nil || len(t.until) >= maxToldLimited {
			t.until = map[string]time.Time{}
		}
	}
	t.until[key] = until
	return true
}

// forget closes the window of key, so that its next stopped start is the first again.
func (t *toldLimited) forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.until, key)
}

// tellLimited tells IntakeConfig.OnLimited of a start that a limit stopped: a bot's mention always, a person's message once for each
// person and window of the trigger (toldLimited), and an alert's never, since it is only counted. A person's window stays open only
// if the record was written: after a failed write the next stopped start of theirs is told again.
func (in *Intake) tellLimited(ctx context.Context, m TriggerMatch, limit store.TriggerLimit) {
	if in.cfg.OnLimited == nil {
		return
	}
	switch {
	case m.Mention:
		_ = in.cfg.OnLimited(ctx, m, limit) // OnLimited logs its own failure, and a mention is told each time anyway
	case m.Person:
		key := fmt.Sprintf("%s#%d/%s", m.Binding, m.Index, m.Message.User)
		if !in.toldLimited.first(key, m.Received, m.Received.Add(m.Limits.DigestWindow())) {
			return
		}
		if err := in.cfg.OnLimited(ctx, m, limit); err != nil {
			in.toldLimited.forget(key)
		}
	}
}
