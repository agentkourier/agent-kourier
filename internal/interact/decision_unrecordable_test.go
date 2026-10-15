package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A decision whose audit row cannot be written as built (the audit's error wraps store.ErrInvalid, and is the same on every
// try) ends the press at once: nothing is recorded or sent, the person is told, and the press is deleted, so that it does not
// hold the later presses of its interaction or the sweep. Any other audit failure is transient, and keeps the press as before.

const (
	approvalUnrecorded = "Your decision could not be recorded, so it was not taken. An operator must fix this."
	questionUnrecorded = "Your answer could not be recorded, so it was not taken. An operator must fix this."
)

// scriptedAudit is the rig's audit log whose next Decisions fail with errs, one each (a nil entry succeeds), and then succeed.
type scriptedAudit struct {
	*orderedAudit
	mu   sync.Mutex
	errs []error
}

func (a *scriptedAudit) Decision(ctx context.Context, x audit.Actor, d audit.Decision) error {
	a.mu.Lock()
	var scripted error
	if len(a.errs) > 0 {
		scripted, a.errs = a.errs[0], a.errs[1:]
	}
	a.mu.Unlock()
	err := a.orderedAudit.Decision(ctx, x, d)
	if scripted != nil {
		return scripted
	}
	return err
}

// unrecordableRig is a press rig of a pending approval (or, when ask is true, a question) whose decider and pipeline write
// their audit through a scriptedAudit that fails with errs. The pipeline's Notify is notify (a no-op if nil), and the
// decider's log is returned.
func unrecordableRig(t *testing.T, ask bool, notify func(store.Press), errs ...error) (*pressRig, *scriptedAudit, *logBuf) {
	t.Helper()
	var r *pressRig
	if ask {
		r = newPressRig(t)
		r.open(t)
	} else {
		r = newApprovalRig(t)
	}
	aud := &scriptedAudit{orderedAudit: r.aud, errs: errs}
	out := &logBuf{}
	b := approvalBinding()
	if ask {
		b = interact.Binding{Dialect: b.Dialect, AskUser: true, Timeout: time.Minute, Identity: "secret:ns/tok"}
	}
	d, err := interact.NewDecider(decideCfg(interact.DecideConfig{Store: r.st, Auditor: aud, Ephemerals: r.eph, Session: r.sess, Sessions: r.st, Replacer: r.rep, Outbox: r.out,
		Logger:     slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnDecision: func(res string) { r.mu.Lock(); r.hooks = append(r.hooks, res); r.mu.Unlock() },
		OnApprovalOutcome: func(binding string, o interact.Outcome) {
			r.mu.Lock()
			r.outcomes = append(r.outcomes, binding+": "+string(o))
			r.mu.Unlock()
		}}))
	if err != nil {
		t.Fatal(err)
	}
	if notify == nil {
		notify = func(store.Press) {}
	}
	r.pl, err = interact.NewPipeline(pressCfg(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{b}, Authorizer: r.az, Ephemerals: r.eph, Decider: d, Auditor: aud,
		Logger: quietLogger(), Now: func() time.Time { return r.now }, Notify: notify}))
	if err != nil {
		t.Fatal(err)
	}
	return r, aud, out
}

// pressOf is user's decision on the rig's prompt: an approval's Approve all, or a question's first choice.
func pressOf(t *testing.T, r *pressRig, ask bool, user string) chat.Interaction {
	t.Helper()
	if ask {
		return r.submit(t, user, 0)
	}
	return tap(user, chat.ActionApproveAll)
}

func unrecordedText(ask bool) string {
	if ask {
		return questionUnrecorded
	}
	return approvalUnrecorded
}

// errInvalidRow is what the audit writer returns for a row it cannot build: store.ErrInvalid, wrapped with its own words.
var errInvalidRow = fmt.Errorf("audit decision: the row would be redacted again when it is stored: %w", store.ErrInvalid)

// TestADecisionWhoseAuditRowCannotBeWrittenEndsThePressAtOnce: the press is Done with no error, the person is told in the fixed
// words, one Error line names the Binding and the interaction, and only the unrecorded decision metric is counted; nothing is recorded, sent or replaced. The
// interaction is still pending and the press is gone.
func TestADecisionWhoseAuditRowCannotBeWrittenEndsThePressAtOnce(t *testing.T) {
	for name, ask := range map[string]bool{"an approval": false, "a question": true} {
		for how, err := range map[string]error{"bare": store.ErrInvalid, "wrapped": errInvalidRow, "wrapped twice": fmt.Errorf("again: %w", errInvalidRow)} {
			t.Run(name+", "+how, func(t *testing.T) {
				r, _, logs := unrecordableRig(t, ask, nil, err)
				if res, perr := r.process(t, pressOf(t, r, ask, "UALICE1")); perr != nil || res != interact.Done {
					t.Fatalf("Process = %v, %v; want Done with no error", res, perr)
				}
				if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: " + unrecordedText(ask)}) {
					t.Errorf("told %q; want the fixed words, once", got)
				}
				row := r.row(t)
				if row.State != store.InteractionPending || row.Decider != "" || len(row.Answer) != 0 || len(r.sess.answers()) != 0 || len(r.counted()) != 0 || !slices.Equal(r.hooked(), []string{"unrecorded"}) ||
					len(r.rep.replaced()) != 0 || r.waiting(t) != 0 || len(r.out.list()) != 0 {
					t.Errorf("state %s, decider %q, answer %q, %d sends, counted %q, hooks %q, %d replaced, %d waiting, %d enqueued; want it pending, only the unrecorded hook counted, nothing sent, recorded or replaced, and the press gone",
						row.State, row.Decider, row.Answer, len(r.sess.answers()), r.counted(), r.hooked(), len(r.rep.replaced()), r.waiting(t), len(r.out.list()))
				}
				if got := r.flow.list(); !slices.Equal(got, []string{"audit:answered"}) {
					t.Errorf("steps %q; want the audit alone: nothing was submitted, sent or replaced", got)
				}
				var line map[string]any
				if lines := strings.Split(strings.TrimSpace(logs.String()), "\n"); len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &line) != nil {
					t.Fatalf("log:\n%s\nwant one line", logs.String())
				}
				if line["level"] != "ERROR" || line["binding"] != "ns/b" || line["interaction"] != "ix-1" {
					t.Errorf("line = %v; want an Error naming the Binding and the interaction", line)
				}
				if told := strings.Join(r.eph.list(), "|"); strings.Contains(told, "redact") || strings.Contains(told, "audit") {
					t.Errorf("told %q; want nothing from the error", told)
				}
			})
		}
	}
}

// TestAnUnwritableDecisionDoesNotHoldTheLaneOfTheNextPress is the lane test on the real Worker: the first person's decision
// cannot be audited and its press is ended at once; the second person's press, behind it in the same interaction, is
// processed without waiting for the worker's cap and decides.
func TestAnUnwritableDecisionDoesNotHoldTheLaneOfTheNextPress(t *testing.T) {
	for name, ask := range map[string]bool{"an approval": false, "a question": true} {
		t.Run(name, func(t *testing.T) {
			var w *interact.Worker
			r, _, _ := unrecordableRig(t, ask, func(p store.Press) { w.Submit(p) }, errInvalidRow)
			deleted := make(chan struct{}, 2)
			r.st.afterDelete = func() { deleted <- struct{}{} }
			w, _, _ = startWorker(t, r.pl, func(c *interact.WorkerConfig) { c.Backoff, c.MaxBackoff = 2*time.Millisecond, 2*time.Millisecond })
			for _, user := range []string{"UALICE1", "UBOB22"} {
				if err := r.pl.Press(t.Context(), pressOf(t, r, ask, user)); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for range 2 {
				select {
				case <-deleted:
				case <-deadline.C:
					t.Fatal("timed out waiting for both presses to be deleted")
				}
			}
			if row := r.row(t); r.waiting(t) != 0 || row.State != store.InteractionAnswered || row.Decider != "UBOB22" || len(r.sess.answers()) != 1 {
				t.Fatalf("state %s, decider %q, %d waiting, %d sends; want the second person's decision taken without waiting", row.State, row.Decider, r.waiting(t), len(r.sess.answers()))
			}
			if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: " + unrecordedText(ask)}) {
				t.Errorf("told %q; want only the first person, in the fixed words", got)
			}
		})
	}
}

// TestATransientAuditFailureStillKeepsThePress: any audit error that does not wrap store.ErrInvalid is as before: Retry with
// the error, nothing told, sent or recorded, one try each time, and once the log is back the same press is the decision. An
// unaudited decision never takes effect, and a failure that may pass never loses one.
func TestATransientAuditFailureStillKeepsThePress(t *testing.T) {
	for name, ask := range map[string]bool{"an approval": false, "a question": true} {
		for how, transient := range map[string]error{"a plain failure": errBoom, "a timeout": fmt.Errorf("audit: %w", context.DeadlineExceeded), "a failure that names a limit": errors.New("audit decision: the row is 7000 bytes, over the limit")} {
			t.Run(name+", "+how, func(t *testing.T) {
				r, _, logs := unrecordableRig(t, ask, nil, transient)
				res, err := r.process(t, pressOf(t, r, ask, "UALICE1"))
				if !errors.Is(err, transient) || res != interact.Retry {
					t.Fatalf("Process = %v, %v; want Retry with the error", res, err)
				}
				if row := r.row(t); row.State != store.InteractionPending || len(r.sess.answers()) != 0 || len(r.eph.list()) != 0 || r.waiting(t) != 1 || len(r.hooked()) != 0 || len(r.rep.replaced()) != 0 {
					t.Errorf("state %s, %d sends, told %q, %d waiting, hooks %q; want it pending, nothing sent or told, and the press kept", row.State, len(r.sess.answers()), r.eph.list(), r.waiting(t), r.hooked())
				}
				if strings.Contains(logs.String(), "cannot be written as built") {
					t.Errorf("log:\n%s\nwant no end-of-press line for a failure that may pass", logs.String())
				}
				presses, _ := r.st.ListPresses(t.Context())
				if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || r.row(t).State != store.InteractionAnswered || len(r.sess.answers()) != 1 {
					t.Errorf("Process again = %v, %v, state %s, %d sends; want the same press decided once the log is back", res, err, r.row(t).State, len(r.sess.answers()))
				}
			})
		}
	}
}
