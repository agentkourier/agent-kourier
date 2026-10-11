package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// A group check that can never succeed until an operator fixes the Binding's groups (chat.ErrGroupsMisconfigured) ends the
// press at once. In these tests S1 is readable and lists UCAROL1 alone, S2 is the bad group (and would list UBOB22), so the check
// fails for everyone not in S1: UALICE1 and UBOB22, an approver whose group the platform cannot read.

const groupsBroken = "Your decision could not be checked, because this Binding's approver groups cannot be read, so it was not taken. An operator must fix this."

// errNoScope is what a platform returns for a group it can never read: the sentinel, wrapped with the platform's own words.
var errNoScope = fmt.Errorf("slack usergroups.users.list: missing_scope: %w", chat.ErrGroupsMisconfigured)

// brokenGroupRig is a group rig whose second group fails with err, with UCAROL1 (in the first group) a member too, and a pipeline
// that logs to the returned buffer and whose Notify is notify (a no-op if nil).
func brokenGroupRig(t *testing.T, err error, notify func(store.Press)) (*pressRig, *logBuf) {
	t.Helper()
	r := newGroupRig(t, groupBinding(), store.InteractionToolApproval)
	r.az.members["UCAROL1"] = true
	r.az.unreadable = map[string]error{"S2": err}
	out := &logBuf{}
	if notify == nil {
		notify = func(store.Press) {}
	}
	pl, perr := interact.NewPipeline(pressCfg(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{groupBinding()}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud,
		Logger: slog.New(telemetry.NewJSONHandler(out, slog.LevelDebug)), Now: func() time.Time { return r.now }, Notify: notify}))
	if perr != nil {
		t.Fatal(perr)
	}
	r.pl = pl
	return r, out
}

// TestAPermanentGroupFailureEndsThePressAtOnce: the press is Done with no error, the person is told in the fixed words, one Error
// line names the Binding and holds the platform's error, and nothing is sent, counted, replaced or audited (not as a denial
// either). The interaction is still pending. It is the same for the approver whose own group is the unreadable one: their press
// is ended and takes no effect.
func TestAPermanentGroupFailureEndsThePressAtOnce(t *testing.T) {
	for name, user := range map[string]string{"a member in no group": "UALICE1", "an approver behind the bad group": "UBOB22"} {
		for action, act := range map[string]chat.Action{"approve all": chat.ActionApproveAll, "deny all": chat.ActionDenyAll} {
			t.Run(name+", "+action, func(t *testing.T) {
				r, out := brokenGroupRig(t, errNoScope, nil)
				if res, err := r.process(t, tap(user, act)); err != nil || res != interact.Done {
					t.Fatalf("Process = %v, %v; want Done with no error", res, err)
				}
				if got := r.eph.list(); !slices.Equal(got, []string{user + ": " + groupsBroken}) {
					t.Errorf("told %q; want the fixed words, once", got)
				}
				if row := r.row(t); row.State != store.InteractionPending || row.Decider != "" || len(r.sess.answers()) != 0 || len(r.counted()) != 0 || len(r.rep.replaced()) != 0 || len(r.aud.got) != 0 || r.waiting(t) != 0 {
					t.Errorf("state %s, decider %q, %d sends, counted %q, %d audit rows, %d waiting; want it pending, nothing sent, counted or audited, and the press gone",
						row.State, row.Decider, len(r.sess.answers()), r.counted(), len(r.aud.got), r.waiting(t))
				}
				var line map[string]any
				if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &line) != nil {
					t.Fatalf("log:\n%s\nwant one line", out.String())
				}
				if line["level"] != "ERROR" || line["binding"] != "ns/b" || line["interaction"] != "ix-1" || !strings.Contains(fmt.Sprint(line["err"]), "missing_scope") {
					t.Errorf("line = %v; want an Error naming the Binding and the interaction, with the platform's error", line)
				}
				if told := strings.Join(r.eph.list(), "|"); strings.Contains(told, "S1") || strings.Contains(told, "S2") || strings.Contains(told, "missing_scope") {
					t.Errorf("told %q; want no group ID and nothing from the platform", told)
				}
			})
		}
	}
}

// TestAPermanentGroupFailureDoesNotHoldTheLaneOfAnApprover is the review's probe, on the real Worker: a press by a member outside
// the earlier group hits the bad group, and the approver's press behind it, in the first group, is processed at once and
// decides. A failure that was held for retry would hold the approver's press behind it, and the approval would stay open.
func TestAPermanentGroupFailureDoesNotHoldTheLaneOfAnApprover(t *testing.T) {
	var w *interact.Worker
	r, _ := brokenGroupRig(t, errNoScope, func(p store.Press) { w.Submit(p) })
	w, _, _ = startWorker(t, r.pl, func(c *interact.WorkerConfig) { c.Backoff, c.MaxBackoff = 2*time.Millisecond, 2*time.Millisecond })
	if err := r.pl.Press(t.Context(), tap("UALICE1", chat.ActionDenyAll)); err != nil {
		t.Fatal(err)
	}
	if err := r.pl.Press(t.Context(), tap("UCAROL1", chat.ActionApproveAll)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.waiting(t) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if row := r.row(t); r.waiting(t) != 0 || row.State != store.InteractionAnswered || row.Decider != "UCAROL1" || len(r.aud.got) != 1 {
		t.Fatalf("state %s, decider %q, %d waiting, %d audit rows; want UCAROL1's approval decided without waiting for the first press", row.State, row.Decider, r.waiting(t), len(r.aud.got))
	}
	if got := r.az.groupAsked(); len(got) != 2 {
		t.Errorf("groups asked %d times; want once for each press: the first press was not tried again", len(got))
	}
	if got := r.eph.list(); !slices.Equal(got, []string{"UALICE1: " + groupsBroken}) {
		t.Errorf("told %q; want only the first person, in the fixed words", got)
	}
	if got, _ := approvalsSent(t, r.sess.answers()[0]); len(got) != 2 || !got[0].Approved || !got[1].Approved {
		t.Errorf("sent %+v; want the approver's approval, and not the denial of the press that was ended", got)
	}
}

// TestAGroupFailureThatMayPassIsKeptForRetry: any error that does not wrap the sentinel, a timeout and a 5xx among them, is the
// transient kind as before: Retry with the error, nothing told, audited, sent or deleted, one Error line unless the context ended,
// and once the platform answers the same press is decided. Neither kind is a yes.
func TestAGroupFailureThatMayPassIsKeptForRetry(t *testing.T) {
	for name, transient := range map[string]error{
		"a timeout":       fmt.Errorf("slack usergroups.users.list: %w", context.DeadlineExceeded),
		"a server error":  errors.New("slack usergroups.users.list: 502 Bad Gateway"),
		"a plain failure": errBoom,
	} {
		t.Run(name, func(t *testing.T) {
			r, out := brokenGroupRig(t, transient, nil)
			if res, err := r.process(t, tap("UBOB22", chat.ActionApproveAll)); !errors.Is(err, transient) || res != interact.Retry {
				t.Fatalf("Process = %v, %v; want Retry with the error", res, err)
			}
			if told := r.eph.list(); len(told) != 0 || len(r.aud.got) != 0 || r.waiting(t) != 1 || r.row(t).State != store.InteractionPending || len(r.sess.answers()) != 0 {
				t.Errorf("told %q, %d audit rows, %d waiting, state %s; want nothing told or audited, the press kept and the approval open", told, len(r.aud.got), r.waiting(t), r.row(t).State)
			}
			if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], `"level":"ERROR"`) || !strings.Contains(lines[0], "approver-group check failed") {
				t.Errorf("log:\n%s\nwant one Error line saying the check failed", out.String())
			}
			r.az.unreadable = nil
			presses, _ := r.st.ListPresses(t.Context())
			if res, err := r.pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done || r.row(t).State != store.InteractionAnswered || r.row(t).Decider != "UBOB22" {
				t.Errorf("Process again = %v, %v, state %s; want the same press decided once the groups can be read", res, err, r.row(t).State)
			}
		})
	}
}

// TestAPermanentGroupFailureOnAWrappedErrorStillEndsThePress: the platform's error may be wrapped on its way (the sentinel is found
// with errors.Is, not compared).
func TestAPermanentGroupFailureOnAWrappedErrorStillEndsThePress(t *testing.T) {
	r, _ := brokenGroupRig(t, fmt.Errorf("check: %w", fmt.Errorf("again: %w", errNoScope)), nil)
	if res, err := r.process(t, tap("UALICE1", chat.ActionApproveAll)); err != nil || res != interact.Done || r.waiting(t) != 0 {
		t.Errorf("Process = %v, %v, %d waiting; want Done", res, err, r.waiting(t))
	}
}
