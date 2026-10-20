package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// hitlBackend returns the a2a dialect of a backend whose agent card lists kagent's HITL extension, as the broker gives it
// to that backend's Bindings, after connecting to it: a fake agent that serves the card alone.
func hitlBackend(t *testing.T) dialect.Dialect {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(a2a.AgentCard{Name: "sre", Version: "1",
			Capabilities:        a2a.AgentCapabilities{Streaming: true, Extensions: []a2a.AgentExtension{{URI: kagentv1.HITLExtension}}},
			SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("http://card.invalid/a2a", a2a.TransportProtocolJSONRPC)}})
	}))
	t.Cleanup(srv.Close)
	g := generic.New(time.Second)
	t.Cleanup(g.CloseIdleConnections)
	backend := &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{URL: srv.URL + "/a2a"}}
	if _, err := g.Connect(t.Context(), backend, v1alpha1.AgentRef{}, dialect.Credentials{UserID: "service"}); err != nil {
		t.Fatal(err)
	}
	return g.For(backend)
}

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
	r.pl, err = interact.NewPipeline(pressCfg(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{interact.Binding{Dialect: d, AskUser: true, Timeout: time.Minute}}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud, Now: func() time.Time { return r.now }}))
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
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyRetry || !errors.Is(err, errBoom) {
		t.Fatalf("failed delivery = %v, %v", done, err)
	}
	reply, err := r.st.NextReply(t.Context(), key)
	if err != nil || reply.InteractionID != id {
		t.Fatalf("claim = %+v, %v", reply, err)
	}
	if waiting, err := r.st.PendingPresses(t.Context(), id, deadline); !waiting || err != nil {
		t.Fatalf("sweeper lost queued answer: %v, %v", waiting, err)
	}
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyConsumed || err != nil {
		t.Fatalf("retry = %v, %v", done, err)
	}
	if len(r.sess.answers()) != 2 || len(r.aud.got) != 1 {
		t.Fatal("recorded answer was lost or audited twice")
	}
	later := task.Status.Timestamp.Add(time.Second)
	task.Status.Timestamp = &later
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyConsumed || err != nil {
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
			if done, err := r.textReply(t, key, reply, task); done != session.TextReplyRetry || !errors.Is(err, errBoom) {
				t.Fatalf("audit failure = %v, %v", done, err)
			}
			r.aud.err = nil
			if done, err := r.textReply(t, key, reply, task); done != session.TextReplyConsumed || err != nil {
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
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyConsumed || err != nil {
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
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyNotAnswer || err != nil {
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
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyConsumed || err != nil {
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
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyWait || err != nil {
		t.Fatalf("unposted prompt = %v, %v", done, err)
	}
	queued, err := r.st.NextReply(t.Context(), key)
	if err != nil || queued.InteractionID != "" || len(r.sess.answers()) != 0 {
		t.Fatalf("reply claimed before prompt: %+v, %v", queued, err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), id, "100.1"); err != nil {
		t.Fatal(err)
	}
	if done, err := r.textReply(t, key, queued, task); done != session.TextReplyConsumed || err != nil {
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
				done, err := r.textReply(t, key, reply, task)
				if claimed {
					if done != session.TextReplyConsumed || err != nil {
						t.Fatalf("closed claimed question = %v, %v", done, err)
					}
				} else if done != session.TextReplyNotAnswer || err != nil {
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
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyWait || err != nil {
		t.Fatalf("pending legacy claim = %v, %v", done, err)
	}
	r.now = reply.EnqueuedAt.Add(time.Hour)
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyConsumed || err != nil {
		t.Fatalf("aged legacy claim = %v, %v", done, err)
	}
}

func TestTextReplyGivesUpOnlyAfterFailedAttempt(t *testing.T) {
	r := newPressRig(t)
	task, reply, _ := textQuestion(t, r)
	r.now = reply.EnqueuedAt.Add(time.Hour)
	r.sess.errs = []error{errBoom}
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyConsumed || err != nil {
		t.Fatalf("failed aged reply = %v, %v", done, err)
	}
	if len(r.sess.answers()) != 1 {
		t.Fatal("aged reply was not attempted before giving up")
	}
}

// A reply pinned to its question when it was enqueued (a typed answer without a mention, D38) whose question closed
// before the runner reached it is consumed in the same call: the person is told why, nothing is decided, audited or
// sent, and nothing is left to retry. It is never an ordinary turn, and never waits for the give-up limit.
func TestTextReplyPinnedAtEnqueueToAClosedQuestionIsConsumedAndTold(t *testing.T) {
	closers := map[string]func(*testing.T, *pressRig, string){
		"timed out": func(t *testing.T, r *pressRig, id string) {
			r.now = deadline
			if err := r.st.ResolveInteraction(t.Context(), id, store.InteractionTimedOut, r.now); err != nil {
				t.Fatal(err)
			}
		},
		"cancelled": func(t *testing.T, r *pressRig, id string) {
			if err := r.st.ResolveInteraction(t.Context(), id, store.InteractionCancelled, r.now); err != nil {
				t.Fatal(err)
			}
		},
		"answered by another person": func(t *testing.T, r *pressRig, id string) {
			if err := r.st.SubmitInteraction(t.Context(), id, []byte(`[["x"]]`), "UBOBBY1", r.now); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, closeIt := range closers {
		t.Run(name, func(t *testing.T) {
			r := newPressRig(t)
			task, reply, id := textQuestion(t, r)
			reply.InteractionID = id // as EnqueueReply stores it
			closeIt(t, r, id)
			done, err := r.textReply(t, key, reply, task)
			if done != session.TextReplyConsumed || err != nil {
				t.Fatalf("TextReply = %v, %v; want it consumed, with no error", done, err)
			}
			if len(r.eph.got) != 1 || !strings.HasPrefix(r.eph.got[0], reply.UserID+":") {
				t.Errorf("told %v; want the person, once, why their answer did not count", r.eph.got)
			}
			if len(r.sess.answers()) != 0 || len(r.aud.got) != 0 {
				t.Error("a typed answer to a closed question was sent or audited as a decision")
			}
		})
	}
}

// A task that waits on a question that is no text question (a choice, or a tool's approval) is never decided by a
// typed reply: the shape is session.IsTextQuestion's, and the reply is neither claimed nor audited.
func TestTextReplyToAQuestionThatTakesNoTextStaysQueued(t *testing.T) {
	r := newPressRig(t) // the Binding's dialect is kagent's, whose questions offer choices
	task := askUser("req-1")
	reply := store.Reply{ID: "reply-1", Key: key, UserID: "UALICE1", MessageRef: "101.1", Text: "a", EnqueuedAt: r.now}
	if _, err := r.st.EnqueueReply(t.Context(), reply); err != nil {
		t.Fatal(err)
	}
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyNoTextQuestion || err != nil {
		t.Fatalf("TextReply = %v, %v; want no-text-question for a question that takes no text", done, err)
	}
	queued, err := r.st.NextReply(t.Context(), key)
	if err != nil || queued.InteractionID != "" || len(r.sess.answers()) != 0 || len(r.aud.got) != 0 {
		t.Fatalf("the reply was claimed or decided: %+v, %v", queued, err)
	}
}

// On the a2a dialect, a free-text question in kagent's HITL extension has the shape of a text question, but its prompt
// invites no typed reply, so the dialect does not mark it as taking one and a typed reply is never decided against it.
func TestTextReplyToAStructuredFreeTextQuestionOnA2AStaysQueued(t *testing.T) {
	r := newPressRig(t)
	d := hitlBackend(t)
	at := d0
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which service?"))
	msg.Extensions = []string{kagentv1.HITLExtension}
	msg.Metadata = map[string]any{kagentv1.HITLExtension: map[string]any{"type": "ask_user_request", "id": "req-1", "questions": []map[string]any{{"question": "Which service?", "choices": []string{}}}}}
	task := a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &at, Message: msg}}
	in, err := d.ParseInteraction(task)
	if err != nil || in.TextReplies || in.Kind != dialect.AskUser {
		t.Fatalf("pause = %+v, %v; want a structured question that takes no text replies", in, err)
	}
	id := interact.InteractionID(*in)
	req, _ := json.Marshal(in)
	if _, err := r.st.CreateInteraction(t.Context(), store.Interaction{ID: id, Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline}); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), id, "100.1"); err != nil {
		t.Fatal(err)
	}
	r.task(t, "task-1")
	r.pl, err = interact.NewPipeline(pressCfg(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{interact.Binding{Dialect: d, AskUser: true, Timeout: time.Minute}}, Authorizer: r.az, Ephemerals: r.eph, Decider: r.d, Auditor: r.aud, Now: func() time.Time { return r.now }}))
	if err != nil {
		t.Fatal(err)
	}
	reply := store.Reply{ID: "reply-1", Key: key, UserID: "UALICE1", MessageRef: "101.1", Text: "checkout", EnqueuedAt: r.now}
	if _, err := r.st.EnqueueReply(t.Context(), reply); err != nil {
		t.Fatal(err)
	}
	if done, err := r.textReply(t, key, reply, task); done != session.TextReplyNoTextQuestion || err != nil {
		t.Fatalf("TextReply = %v, %v; want no-text-question for a question that takes no text", done, err)
	}
	if queued, err := r.st.NextReply(t.Context(), key); err != nil || queued.InteractionID != "" || len(r.sess.answers()) != 0 || len(r.aud.got) != 0 {
		t.Fatalf("the reply was claimed or decided: %+v, %v", queued, err)
	}
}

// A reply pinned to its question names it: it is decided against that question whatever task it is handed, which is
// none when the runner reconciles a claim or a reply pinned at the intake (session.Queued).
func TestTextReplyPinnedToAQuestionNeedsNoTask(t *testing.T) {
	r := newPressRig(t)
	_, reply, id := textQuestion(t, r)
	reply.InteractionID = id // as EnqueueReply stores it
	if done, err := r.textReply(t, key, reply, a2a.Task{}); done != session.TextReplyConsumed || err != nil {
		t.Fatalf("TextReply = %v, %v; want the answer decided against its pinned question", done, err)
	}
	if row, _ := r.st.GetInteraction(t.Context(), id); row.State != store.InteractionAnswered || len(r.sess.answers()) != 1 {
		t.Fatalf("the pinned question was not answered: %+v", row)
	}
}

// An answer to a question whose prompt was seen is decided even though the question has closed since: the pipeline
// tells the person it came too late (Process), and it is not left to be sent to the agent as a turn.
func TestTextReplyToAClosedQuestionWithItsPromptSeenIsDecidedNotTurned(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestion(t, r)
	if err := r.st.SubmitInteraction(t.Context(), id, []byte(`[["x"]]`), "UBOBBY1", r.now); err != nil {
		t.Fatal(err)
	}
	done, err := r.textReply(t, key, reply, task)
	if done == session.TextReplyNotAnswer || err != nil {
		t.Fatalf("TextReply = %v, %v; want the answer decided, and not left as a turn", done, err)
	}
	queued, qerr := r.st.NextReply(t.Context(), key)
	if qerr != nil || queued.InteractionID != id {
		t.Fatalf("the reply was not claimed for its question: %+v, %v", queued, qerr)
	}
}

// noPause isolates answer decisions in these tests; coordinator tests exercise real take-up.
type noPause struct{}

func (noPause) Paused(context.Context, interact.Pause) error { return nil }

func (r *pressRig) textReply(t *testing.T, key store.ConversationKey, reply store.Reply, task a2a.Task) (session.TextReplyOutcome, error) {
	t.Helper()
	ready := make(chan struct{})
	close(ready)
	handler, err := interact.NewTextReplies(r.pl, noPause{}, ready)
	if err != nil {
		t.Fatal(err)
	}
	return handler.Decide(t.Context(), key, reply, task)
}
