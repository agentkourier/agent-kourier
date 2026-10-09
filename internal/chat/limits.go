package chat

import (
	"context"
	"fmt"
	"strconv"

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

// checks says whether any limit is set to check: a cap of 0 is none, and a trigger with none is not counted.
func checks(l store.TriggerRunLimits) bool {
	return (l.MaxRuns > 0 && l.Per > 0) || l.MaxConcurrent > 0 || l.MaxPerDay > 0
}

// admit asks the trigger's limits whether the match may start a turn, and records the start if it may, in one atomic
// step in the store (so the count survives a restart). It returns the limit that stops it, or "" if it may start. It
// is a local read and write, as the acknowledgement path allows. Asking again for the same message, as a redelivery
// does, is the same start: it is counted once. Without IntakeConfig.TriggerRuns, or a trigger with no limits, nothing
// stops a start.
func (in *Intake) admit(ctx context.Context, m TriggerMatch) (store.TriggerLimit, error) {
	limits := runLimits(m.Limits)
	if in.cfg.TriggerRuns == nil || !checks(limits) {
		return "", nil
	}
	run := store.TriggerRun{
		Binding: m.Binding, Trigger: m.Index, At: m.Received,
		Key: store.ConversationKey{Connection: in.conn, Channel: m.Message.At.Channel, Thread: m.Message.At.ID},
	}
	adm, err := in.cfg.TriggerRuns.AdmitTriggerRun(ctx, run, limits)
	if err != nil {
		return "", fmt.Errorf("check the limits of trigger %d of %s for %s: %w", m.Index, m.Binding, m.MessageID, err)
	}
	return adm.Limit, nil
}

// stopThread is what a limit does to a fire that reserved a thread: it gives the reservation back and counts the
// decision. The key is as if it had not fired, so the next fire of it may start when there is capacity; and nothing
// is posted, since a note per alert is what a storm must not do (D26). If the release fails the decision is left
// unset and the error returned: the redelivery finds its own reservation and is checked again.
func (in *Intake) stopThread(ctx context.Context, m TriggerMatch, hash string, out decided, limit store.TriggerLimit) (decided, error) {
	if _, err := in.cfg.TriggerThreads.ReleaseTriggerThread(ctx, m.Binding, hash, m.Message.At.ID); err != nil {
		out.decision = ""
		return out, fmt.Errorf("release the thread of key %s: %w", out.keyHash, err)
	}
	in.countLimited(ctx, m, limit)
	out.decision = DecisionLimited
	return out, nil
}

// countLimited counts and logs a start a limit stopped. The log names the Binding, the trigger and the limit and
// where the alert was, and never what it said.
func (in *Intake) countLimited(ctx context.Context, m TriggerMatch, limit store.TriggerLimit) {
	telemetry.Count(ctx, "agentkourier_trigger_limited_total", m.Binding, strconv.Itoa(m.Index), string(limit))
	in.log.InfoContext(ctx, "chat: a trigger's start was not made; a limit is reached", "binding", m.Binding, "trigger", m.Index, "limit", limit, "channel", m.Message.At.Channel, "message", m.Message.At.ID)
}
