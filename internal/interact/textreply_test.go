package interact_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func textQuestion(t *testing.T, r *pressRig) (a2a.Task, store.Reply, string) {
	t.Helper()
	return textQuestionWithPrompt(t, r, true)
}

func textQuestionWithPrompt(t *testing.T, r *pressRig, posted bool) (a2a.Task, store.Reply, string) {
	t.Helper()
	d := generic.New(time.Second)
	at := d0
	task := a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &at, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which service?"))}}
	in, err := d.ParseInteraction(task)
	if err != nil {
		t.Fatal(err)
	}
	id := interact.InteractionID(*in)
	req, _ := json.Marshal(in)
	_, err = r.st.CreateInteraction(t.Context(), store.Interaction{TextReplies: true, ID: id, Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	if posted {
		if err := r.st.SetInteractionMessageRef(t.Context(), id, "100.1"); err != nil {
			t.Fatal(err)
		}
	}
	r.task(t, "task-1")
	r.pl, err = interact.NewPipeline(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{interact.Binding{Dialect: d, AskUser: true, Timeout: time.Minute}}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud, Now: func() time.Time { return r.now }})
	if err != nil {
		t.Fatal(err)
	}
	reply := store.Reply{ID: "reply-1", Key: key, UserID: "UALICE1", MessageRef: "101.1", Text: "checkout", EnqueuedAt: r.now}
	if _, err := r.st.EnqueueReply(t.Context(), reply); err != nil {
		t.Fatal(err)
	}
	return task, reply, id
}

func TestTextReplyRetriesRecordedAnswerAndPinsQuestion(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestion(t, r)
	if waiting, err := r.st.PendingPresses(t.Context(), id, deadline); !waiting || err != nil {
		t.Fatalf("sweeper missed in-time reply before claim: %v, %v", waiting, err)
	}
	r.sess.errs = []error{errBoom}
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); done || !errors.Is(err, errBoom) {
		t.Fatalf("failed delivery = %v, %v", done, err)
	}
	reply, err := r.st.NextReply(t.Context(), key)
	if err != nil || reply.InteractionID != id {
		t.Fatalf("claim = %+v, %v", reply, err)
	}
	if waiting, err := r.st.PendingPresses(t.Context(), id, deadline); !waiting || err != nil {
		t.Fatalf("sweeper lost queued answer: %v, %v", waiting, err)
	}
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); !done || err != nil {
		t.Fatalf("retry = %v, %v", done, err)
	}
	if len(r.sess.answers()) != 2 || len(r.aud.got) != 1 {
		t.Fatal("recorded answer was lost or audited twice")
	}
	later := task.Status.Timestamp.Add(time.Second)
	task.Status.Timestamp = &later
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); !done || err != nil {
		t.Fatalf("replay = %v, %v", done, err)
	}
	if len(r.sess.answers()) != 2 {
		t.Fatal("replay answered a later question")
	}
	if err := r.st.ClaimReplyInteraction(t.Context(), reply.ID, "another-question"); err == nil {
		t.Fatal("claim changed questions")
	}
}

func TestTextReplyAuthorizationAndAuditFailure(t *testing.T) {
	for _, user := range []string{"UOUTSIDE", "UBOT"} {
		t.Run(user, func(t *testing.T) {
			r := newPressRig(t)
			task, reply, id := textQuestion(t, r)
			r.az.members["UBOT"] = true
			r.az.bots = map[string]bool{"UBOT": true}
			reply.UserID = user
			r.aud.err = errBoom
			if done, err := r.pl.TextReply(t.Context(), key, reply, task); done || !errors.Is(err, errBoom) {
				t.Fatalf("audit failure = %v, %v", done, err)
			}
			r.aud.err = nil
			if done, err := r.pl.TextReply(t.Context(), key, reply, task); !done || err != nil {
				t.Fatalf("denial = %v, %v", done, err)
			}
			row, _ := r.st.GetInteraction(t.Context(), id)
			if row.State != store.InteractionPending || len(r.sess.answers()) != 0 || r.aud.got[len(r.aud.got)-1].Result != audit.DecisionDenied {
				t.Fatal("unauthorized answer took effect")
			}
		})
	}
}

func TestTextReplyAfterDeadlineDoesNotAnswer(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestion(t, r)
	reply.EnqueuedAt = deadline
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); !done || err != nil {
		t.Fatalf("late answer = %v, %v", done, err)
	}
	row, _ := r.st.GetInteraction(t.Context(), id)
	if row.State != store.InteractionPending || len(r.sess.answers()) != 0 {
		t.Fatal("late answer accepted")
	}
}

func TestTextReplyBeforeQuestionRemainsUnclaimed(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestion(t, r)
	reply.EnqueuedAt = d0.Add(-time.Second)
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); done || !errors.Is(err, store.ErrReplyNotAnswer) {
		t.Fatalf("earlier reply = %v, %v", done, err)
	}
	queued, err := r.st.NextReply(t.Context(), key)
	if err != nil || queued.InteractionID != "" {
		t.Fatalf("earlier reply was claimed: %+v, %v", queued, err)
	}
	row, _ := r.st.GetInteraction(t.Context(), id)
	if row.State != store.InteractionPending || len(r.sess.answers()) != 0 || len(r.aud.got) != 0 {
		t.Fatal("earlier reply became an interaction decision")
	}
}

func TestTextReplyTriesTimelyAnswerAfterLongOutage(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestion(t, r)
	r.now = reply.EnqueuedAt.Add(time.Hour)
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); !done || err != nil {
		t.Fatalf("aged reply = %v, %v", done, err)
	}
	row, _ := r.st.GetInteraction(t.Context(), id)
	if row.State != store.InteractionAnswered || len(r.sess.answers()) != 1 {
		t.Fatal("timely answer was discarded after an outage")
	}
}

func TestTextReplyWaitsForPromptBeforeClaiming(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestionWithPrompt(t, r, false)
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); done || err != nil {
		t.Fatalf("unposted prompt = %v, %v", done, err)
	}
	queued, err := r.st.NextReply(t.Context(), key)
	if err != nil || queued.InteractionID != "" || len(r.sess.answers()) != 0 {
		t.Fatalf("reply claimed before prompt: %+v, %v", queued, err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), id, "100.1"); err != nil {
		t.Fatal(err)
	}
	if done, err := r.pl.TextReply(t.Context(), key, queued, task); !done || err != nil {
		t.Fatalf("posted prompt = %v, %v", done, err)
	}
	queued, err = r.st.NextReply(t.Context(), key)
	if err != nil || queued.InteractionID != id || len(r.sess.answers()) != 1 {
		t.Fatalf("posted question was not answered: %+v, %v", queued, err)
	}
}

func TestTextReplyClosedUnpostedQuestionDoesNotBlockQueue(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		for _, state := range []store.InteractionState{store.InteractionCancelled, store.InteractionTimedOut} {
			t.Run(string(state)+"_"+strconv.FormatBool(claimed), func(t *testing.T) {
				r := newPressRig(t)
				task, reply, id := textQuestionWithPrompt(t, r, false)
				r.now = deadline
				if err := r.st.ResolveInteraction(t.Context(), id, state, r.now); err != nil {
					t.Fatal(err)
				}
				if claimed { // recovery of a claim persisted by the previous implementation
					if err := r.st.ClaimReplyInteraction(t.Context(), reply.ID, id); err != nil {
						t.Fatal(err)
					}
					reply.InteractionID = id
					reply.EnqueuedAt = d0.Add(-time.Second) // legacy claims can predate the question
				}
				done, err := r.pl.TextReply(t.Context(), key, reply, task)
				if claimed {
					if !done || err != nil {
						t.Fatalf("closed claimed question = %v, %v", done, err)
					}
				} else if done || !errors.Is(err, store.ErrReplyNotAnswer) {
					t.Fatalf("closed unclaimed question = %v, %v", done, err)
				}
				if len(r.sess.answers()) != 0 || len(r.eph.got) != 0 || len(r.aud.got) != 0 {
					t.Fatal("unposted question produced an answer, decision or Submit notice")
				}
				queued, err := r.st.NextReply(t.Context(), key)
				if err != nil || (!claimed && queued.InteractionID != "") {
					t.Fatalf("ordinary reply was claimed: %+v, %v", queued, err)
				}
			})
		}
	}
}

func TestTextReplyLegacyUnpostedClaimRetainsRetryLimit(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestionWithPrompt(t, r, false)
	if err := r.st.ClaimReplyInteraction(t.Context(), reply.ID, id); err != nil {
		t.Fatal(err)
	}
	reply.InteractionID = id
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); done || err != nil {
		t.Fatalf("pending legacy claim = %v, %v", done, err)
	}
	r.now = reply.EnqueuedAt.Add(time.Hour)
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); !done || err != nil {
		t.Fatalf("aged legacy claim = %v, %v", done, err)
	}
}

func TestTextReplyGivesUpOnlyAfterFailedAttempt(t *testing.T) {
	r := newPressRig(t)
	task, reply, _ := textQuestion(t, r)
	r.now = reply.EnqueuedAt.Add(time.Hour)
	r.sess.errs = []error{errBoom}
	if done, err := r.pl.TextReply(t.Context(), key, reply, task); !done || err != nil {
		t.Fatalf("failed aged reply = %v, %v", done, err)
	}
	if len(r.sess.answers()) != 1 {
		t.Fatal("aged reply was not attempted before giving up")
	}
}
