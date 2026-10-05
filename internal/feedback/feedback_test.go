package feedback_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

var (
	t0  = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	key = store.ConversationKey{Connection: "courier/slack", Channel: "C1", Thread: "100.1"}
)

const (
	turn      = "slack:courier/slack:C1:100.1"
	counter   = "courier_investigation_feedback_total"
	bindingID = "courier/sre"
	agentID   = "kagent/sre"
)

// authz says who is a member and who is a bot; a user in neither is a person outside the channel.
type authz struct {
	mu                sync.Mutex
	members, bots     map[string]bool
	memberErr, botErr error
	asked             int
}

func (a *authz) IsMember(_ context.Context, _, user string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	return a.members[user] && a.memberErr == nil, a.memberErr
}

func (a *authz) IsBot(_ context.Context, user string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	return a.bots[user] || a.botErr != nil, a.botErr
}

// told is one ephemeral: where, to whom, and what it said.
type told struct {
	target chat.Target
	user   string
	text   string
}

type ephemerals struct {
	mu  sync.Mutex
	got []told
	err error
}

func (e *ephemerals) Ephemeral(_ context.Context, t chat.Target, user string, m chat.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, told{t, user, m.Fallback})
	return e.err
}

func (e *ephemerals) list() []told {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.got)
}

// audits records the votes written to the audit log.
type audits struct {
	mu     sync.Mutex
	actors []audit.Actor
	votes  []audit.FeedbackVote
	err    error
}

func (a *audits) FeedbackVote(_ context.Context, actor audit.Actor, v audit.FeedbackVote) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actors, a.votes = append(a.actors, actor), append(a.votes, v)
	return a.err
}

type rig struct {
	st   *sqlite.Store
	az   *authz
	eph  *ephemerals
	aud  *audits
	tel  *telemetrytest.Harness
	h    *feedback.Handler
	now  time.Time
	seen []store.Press // what Notify was told
	logs bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &rig{st: st, az: &authz{members: map[string]bool{"UALICE1": true, "UBOB22": true}, bots: map[string]bool{"UBOT111": true}}, eph: &ephemerals{}, aud: &audits{}, tel: telemetrytest.New(t), now: t0}
	r.members("UBOT111") // a bot is in the channel: membership does not keep it out
	if err := st.UpsertSession(t.Context(), store.Session{Key: key, Binding: bindingID, Alert: "KubePodCrashLooping", Agent: agentID, LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	r.message(t, key, turn, "100.9")
	r.h, err = feedback.New(feedback.Config{
		Store: st, Sessions: st, Authorizer: r.az, Ephemerals: r.eph, Auditor: r.aud,
		Limits: store.PressLimits{Total: 10, PerUser: 3}, Logger: slog.New(slog.NewTextHandler(&r.logs, nil)),
		Now: func() time.Time { return r.now }, Notify: func(p store.Press) { r.seen = append(r.seen, p) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// message is the outbox row of the reply of turn in the thread of k, recorded as the message ref ref: the message the buttons are on.
func (r *rig) message(t *testing.T, k store.ConversationKey, turn, ref string) {
	t.Helper()
	id := outbox.ID(turn, store.OutboxReply)
	if _, err := r.st.CreateOutbox(t.Context(), store.OutboxEntry{ID: id, EventID: turn, Role: store.OutboxReply, Channel: k.Channel, Thread: k.Thread, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := r.st.RecordMessageRef(t.Context(), id, ref); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) members(users ...string) {
	for _, u := range users {
		r.az.members[u] = true
	}
}

// press is user's press of the button of verdict on the final message of turn, as the adapter hands it over.
func press(user string, kind store.FeedbackKind, verdict store.Verdict) chat.Interaction {
	return chat.Interaction{
		User: user, At: chat.Ref{Platform: "slack", Conn: key.Connection, Channel: key.Channel, Thread: key.Thread, ID: "100.9"},
		PromptID: feedback.Value{Kind: kind, Verdict: verdict, Thread: key.Thread, Turn: turn}.String(),
	}
}

// vote records the press and processes it, as the worker does.
func (r *rig) vote(t *testing.T, i chat.Interaction) (interact.Result, error) {
	t.Helper()
	if err := r.h.Press(r.tel.Ctx, i); err != nil {
		t.Fatalf("Press: %v", err)
	}
	presses, err := r.st.ListPresses(t.Context())
	if err != nil || len(presses) == 0 {
		t.Fatalf("ListPresses = %+v, %v; want the press recorded", presses, err)
	}
	return r.h.Process(r.tel.Ctx, presses[len(presses)-1])
}

func (r *rig) votes(t *testing.T) []store.FeedbackVote {
	t.Helper()
	got, err := r.st.ListVotes(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (r *rig) waiting(t *testing.T) int {
	t.Helper()
	p, err := r.st.ListPresses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return len(p)
}

func (r *rig) count(t *testing.T, alert string, verdict store.Verdict) float64 {
	t.Helper()
	return r.countFor(t, bindingID, alert, verdict)
}

func (r *rig) countFor(t *testing.T, binding, alert string, verdict store.Verdict) float64 {
	t.Helper()
	return r.tel.Counter(t, counter, map[string]string{"binding": binding, "agent": agentID, "alertname": alert, "verdict": string(verdict)})
}

// thread makes the session of thread under binding, started for alert, with a message in it, and returns a press of the useful
// button of that message, of the kind.
func (r *rig) thread(t *testing.T, binding, thread, alert string, kind store.FeedbackKind) chat.Interaction {
	t.Helper()
	k := store.ConversationKey{Connection: key.Connection, Channel: key.Channel, Thread: thread}
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: k, Binding: binding, Alert: alert, Agent: agentID, LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	own := turn + "@" + thread // a turn of its own, so that the row of its message is its own
	r.message(t, k, own, "100.9")
	in := press("UALICE1", kind, store.VerdictUseful)
	in.PromptID = feedback.Value{Kind: kind, Verdict: store.VerdictUseful, Thread: thread, Turn: own}.String()
	return in
}

func TestPartsAreTheQuestionAndTwoButtonsWhoseValuesNameTheKindVerdictAndTurn(t *testing.T) {
	parts := feedback.Parts(store.FeedbackInvestigation, key.Thread, turn)
	if err := (chat.Message{Fallback: "x", Parts: parts}).Validate(); err != nil {
		t.Fatalf("the parts are not a valid message: %v", err)
	}
	var btns chat.Buttons
	for _, p := range parts {
		if b, ok := p.(chat.Buttons); ok {
			btns = b
		}
	}
	if len(btns.Buttons) != 2 || btns.Buttons[0].Label != "Useful" || btns.Buttons[1].Label != "Not useful" || btns.Buttons[0].ID == btns.Buttons[1].ID {
		t.Fatalf("buttons = %+v; want Useful and Not useful, with IDs of their own", btns.Buttons)
	}
	for i, want := range []store.Verdict{store.VerdictUseful, store.VerdictNotUseful} {
		v, ok := feedback.ParseValue(btns.Buttons[i].Value)
		if !ok || v != (feedback.Value{Kind: store.FeedbackInvestigation, Verdict: want, Thread: key.Thread, Turn: turn}) {
			t.Errorf("button %d value %q parses to %+v, %v; want the investigation's %s on %s in %s", i, btns.Buttons[i].Value, v, ok, want, turn, key.Thread)
		}
	}
}

// The final message is stored by the outbox as the versioned neutral message, and posted from it after a restart: the buttons
// are in what is stored, with what they rate, and nothing in them depends on the process that made them.
func TestThePartsSurviveTheOutboxsStoredFormOfAMessage(t *testing.T) {
	out := chat.Say(chat.Message{Fallback: "the answer", Parts: append([]chat.Part{chat.Body{Markdown: "the answer"}}, feedback.Parts(store.FeedbackAnswer, key.Thread, turn)...)})
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var back chat.Outgoing
	if err := json.Unmarshal(raw, &back); err != nil || back.Validate() != nil || !reflect.DeepEqual(back, out) {
		t.Fatalf("the stored form %s read back as %+v, %v; want the message it was made from", raw, back, err)
	}
	if !strings.Contains(string(raw), `fb1|answer|useful|`+key.Thread+`|`+turn) {
		t.Errorf("the stored form %s does not hold what the button rates", raw)
	}
}

// A message that the platform refuses is lost whole, so a message whose thread and turn could not fit a button's value gets no buttons.
func TestPartsOfAMessageWhoseIDsDoNotFitAButtonAreNone(t *testing.T) {
	if parts := feedback.Parts(store.FeedbackAnswer, "100.1", strings.Repeat("t", 496)); len(parts) != 0 {
		t.Errorf("parts of a thread and a turn of 501 bytes = %+v; want none", parts)
	}
	if parts := feedback.Parts(store.FeedbackAnswer, "100.1", strings.Repeat("t", 495)); len(parts) == 0 {
		t.Error("no parts for a thread and a turn of 500 bytes")
	}
	if parts := feedback.Parts(store.FeedbackAnswer, "", turn); len(parts) != 0 {
		t.Errorf("parts of a message in no thread = %+v; want none", parts)
	}
}

func TestParseValueTakesNothingButAFeedbackValue(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"fb1|investigation|useful|100.1|" + turn, true},
		{"fb1|answer|not_useful|100.1|task:t|1", true}, // the turn is whatever follows the fourth separator, verbatim
		{"", false},
		{"ix-0123456789abcdef", false}, // an interaction's button
		{"feedback-up", false},
		{"fb2|investigation|useful|100.1|" + turn, false}, // another version
		{"fb1|digest|useful|100.1|" + turn, false},
		{"fb1|answer|great|100.1|" + turn, false},
		{"fb1|answer|useful||" + turn, false}, // no thread
		{"fb1|answer|useful|100.1|", false},   // no turn
		{"fb1|answer|useful|100.1", false},
		{"fb1|investigation|useful|" + turn, false}, // the value of a version with no thread
	} {
		if _, ok := feedback.ParseValue(tc.value); ok != tc.ok {
			t.Errorf("ParseValue(%q) ok = %v; want %v", tc.value, !tc.ok, tc.ok)
		}
	}
}

// The press is recorded and the person's checks wait for the worker: the press is on Slack's acknowledgement path, where a
// call to Slack is not allowed, and the check of who pressed is two.
func TestPressRecordsAtCouriersTimeAndDoesNoneOfTheWork(t *testing.T) {
	r := newRig(t)
	if err := r.h.Press(t.Context(), press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if len(presses) != 1 || !presses[0].PressedAt.Equal(t0) || len(r.seen) != 1 || r.seen[0].Seq != presses[0].Seq {
		t.Fatalf("presses %+v, announced %+v; want one, at courier's own time, announced to the worker", presses, r.seen)
	}
	if r.az.asked != 0 || len(r.eph.list()) != 0 || len(r.votes(t)) != 0 {
		t.Error("recording the press did the work of processing it")
	}
}

func TestPressOfAnotherButtonIsNotOurs(t *testing.T) {
	r := newRig(t)
	err := r.h.Press(t.Context(), chat.Interaction{User: "UALICE1", At: chat.Ref{Channel: "C1", Thread: "100.1", ID: "100.9"}, PromptID: "ix-1"})
	if !errors.Is(err, feedback.ErrNotFeedback) || r.waiting(t) != 0 {
		t.Errorf("Press = %v, %d recorded; want ErrNotFeedback and nothing recorded", err, r.waiting(t))
	}
}

func TestAPressThatNamesNoPersonOrMessageIsAcknowledgedAndDropped(t *testing.T) {
	r := newRig(t)
	for name, mutate := range map[string]func(*chat.Interaction){
		"no person":  func(i *chat.Interaction) { i.User = "" },
		"no message": func(i *chat.Interaction) { i.At.ID = "" },
		"no channel": func(i *chat.Interaction) { i.At.Channel = "" },
	} {
		i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
		mutate(&i)
		if err := r.h.Press(t.Context(), i); err != nil || r.waiting(t) != 0 {
			t.Errorf("Press with %s = %v, %d recorded; want it acknowledged and nothing recorded", name, err, r.waiting(t))
		}
	}
}

// Both buttons of a message are one lane of the worker, so that a person's two presses are processed in the order they
// were made and the later one wins.
func TestBothButtonsOfAMessageAreOneLaneAndAnotherMessageIsAnother(t *testing.T) {
	r := newRig(t)
	for _, v := range []store.Verdict{store.VerdictUseful, store.VerdictNotUseful} {
		if err := r.h.Press(t.Context(), press("UALICE1", store.FeedbackInvestigation, v)); err != nil {
			t.Fatal(err)
		}
	}
	other := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
	other.PromptID = feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: key.Thread, Turn: "another"}.String()
	if err := r.h.Press(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if len(presses) != 3 || presses[0].InteractionID != presses[1].InteractionID || presses[0].InteractionID == presses[2].InteractionID {
		t.Fatalf("lanes of the presses = %v; want the first two alike and the third apart", laneIDs(presses))
	}
	for _, p := range presses {
		if !feedback.IsPress(p) {
			t.Errorf("IsPress(%+v) = false; want true: the worker's router tells ours by it", p)
		}
	}
	if feedback.IsPress(store.Press{InteractionID: "ix-1"}) {
		t.Error("IsPress of an interaction's press = true")
	}
}

func laneIDs(ps []store.Press) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.InteractionID)
	}
	return out
}

func TestAMemberVotesAndTheVoteIsStoredAuditedCountedAndToldToThemAlone(t *testing.T) {
	r := newRig(t)
	res, err := r.vote(t, press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful))
	if err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want Done", res, err)
	}
	got := r.votes(t)
	want := store.FeedbackVote{Key: key, Turn: turn, User: "UALICE1", Binding: bindingID, Agent: agentID, Alert: "KubePodCrashLooping",
		Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, At: t0, First: t0}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("votes = %+v; want %+v: the Binding, the agent and the alert are the session's", got, want)
	}
	if n := r.count(t, "KubePodCrashLooping", store.VerdictUseful); n != 1 {
		t.Errorf("%s{useful} = %v; want 1", counter, n)
	}
	if len(r.aud.votes) != 1 || r.aud.votes[0] != (audit.FeedbackVote{Turn: turn, Channel: "C1", Thread: "100.1", Verdict: store.VerdictUseful}) ||
		r.aud.actors[0].User != "UALICE1" || r.aud.actors[0].Binding != bindingID {
		t.Errorf("audited %+v by %+v; want the vote by IDs and the verdict, by the voter", r.aud.votes, r.aud.actors)
	}
	eph := r.eph.list()
	if len(eph) != 1 || eph[0].user != "UALICE1" || eph[0].target != (chat.Target{Conn: key.Connection, Channel: "C1", Thread: "100.1"}) ||
		!strings.Contains(eph[0].text, "useful") || !strings.Contains(eph[0].text, "change") {
		t.Errorf("told %+v; want the voter alone, in the thread, that the vote is recorded and how to change it", eph)
	}
	if r.waiting(t) != 0 {
		t.Error("the press is still recorded after it was processed")
	}
}

func TestAPersonChangingTheirVoteReplacesItAndIsCountedUnderTheNewVerdict(t *testing.T) {
	r := newRig(t)
	for i, v := range []store.Verdict{store.VerdictUseful, store.VerdictNotUseful} {
		r.now = t0.Add(time.Duration(i) * time.Minute)
		if res, err := r.vote(t, press("UALICE1", store.FeedbackInvestigation, v)); err != nil || res != interact.Done {
			t.Fatalf("Process %d = %v, %v", i, res, err)
		}
	}
	got := r.votes(t)
	if len(got) != 1 || got[0].Verdict != store.VerdictNotUseful || !got[0].First.Equal(t0) || !got[0].At.Equal(t0.Add(time.Minute)) {
		t.Fatalf("votes = %+v; want the one vote, now not useful, first cast at %v and changed a minute later", got, t0)
	}
	// A counter of votes cast: the change is one more vote, under its new verdict; the first stays counted.
	if u, n := r.count(t, "KubePodCrashLooping", store.VerdictUseful), r.count(t, "KubePodCrashLooping", store.VerdictNotUseful); u != 1 || n != 1 {
		t.Errorf("%s: useful %v, not_useful %v; want 1 and 1", counter, u, n)
	}
	if len(r.aud.votes) != 2 || r.aud.votes[0].Changed || !r.aud.votes[1].Changed || r.aud.votes[1].Verdict != store.VerdictNotUseful {
		t.Errorf("audited %+v; want the vote, then the change", r.aud.votes)
	}
}

func TestThePressOfTheVerdictAlreadyRecordedChangesNothingAndCountsNothing(t *testing.T) {
	r := newRig(t)
	for range 2 {
		if _, err := r.vote(t, press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.count(t, "KubePodCrashLooping", store.VerdictUseful); n != 1 || len(r.aud.votes) != 1 || len(r.votes(t)) != 1 {
		t.Errorf("counted %v, audited %d, stored %d; want one of each: pressing the same button again is no new vote", n, len(r.aud.votes), len(r.votes(t)))
	}
	if eph := r.eph.list(); len(eph) != 2 || !strings.Contains(eph[1].text, "already") {
		t.Errorf("told %+v; want the second press answered that the vote is recorded already", eph)
	}
}

func TestAnyMemberMayVoteAndEachHasAVoteOfTheirOwn(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct {
		user string
		v    store.Verdict
	}{{"UALICE1", store.VerdictUseful}, {"UBOB22", store.VerdictNotUseful}} {
		if _, err := r.vote(t, press(tc.user, store.FeedbackInvestigation, tc.v)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.votes(t); len(got) != 2 {
		t.Fatalf("votes = %+v; want one for each member", got)
	}
}

func TestABotsPressIsIgnoredWithoutAWord(t *testing.T) {
	r := newRig(t)
	res, err := r.vote(t, press("UBOT111", store.FeedbackInvestigation, store.VerdictUseful))
	if err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want Done", res, err)
	}
	if len(r.votes(t)) != 0 || len(r.eph.list()) != 0 || len(r.aud.votes) != 0 || r.waiting(t) != 0 {
		t.Errorf("a bot's press left votes %+v, told %+v, audited %+v, %d pressed; want nothing but the press consumed", r.votes(t), r.eph.list(), r.aud.votes, r.waiting(t))
	}
	if n := r.count(t, "KubePodCrashLooping", store.VerdictUseful); n != 0 {
		t.Errorf("a bot's vote was counted: %v", n)
	}
}

// Slack refuses an ephemeral to a person who is not in the channel, so there is no word to tell them: the press is dropped.
func TestAPersonOutsideTheChannelIsIgnoredAndNotCounted(t *testing.T) {
	r := newRig(t)
	res, err := r.vote(t, press("UOUTSIDE", store.FeedbackInvestigation, store.VerdictUseful))
	if err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want Done", res, err)
	}
	if len(r.votes(t)) != 0 || len(r.aud.votes) != 0 || len(r.eph.list()) != 0 || r.waiting(t) != 0 {
		t.Errorf("votes %+v, audited %+v, told %+v, %d pressed; want nothing but the press consumed", r.votes(t), r.aud.votes, r.eph.list(), r.waiting(t))
	}
	if n := r.count(t, "KubePodCrashLooping", store.VerdictUseful); n != 0 {
		t.Errorf("the vote of a person outside the channel was counted: %v", n)
	}
}

// A check that cannot be made is no decision: the press is kept for the worker to try again, and nothing is recorded.
func TestAFailedCheckKeepsThePressForAnotherTry(t *testing.T) {
	for name, set := range map[string]func(*authz){
		"membership": func(a *authz) { a.memberErr = errors.New("slack down") },
		"is-bot":     func(a *authz) { a.botErr = errors.New("slack down") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			set(r.az)
			res, err := r.vote(t, press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful))
			if err == nil || res != interact.Retry {
				t.Fatalf("Process = %v, %v; want Retry with the error", res, err)
			}
			if len(r.votes(t)) != 0 || len(r.eph.list()) != 0 || r.waiting(t) != 1 {
				t.Errorf("votes %+v, told %+v, %d waiting; want nothing but the press kept", r.votes(t), r.eph.list(), r.waiting(t))
			}
			r.az.memberErr, r.az.botErr = nil, nil // Slack is back: the same press, tried again, counts
			presses, _ := r.st.ListPresses(t.Context())
			if res, err := r.h.Process(r.tel.Ctx, presses[0]); err != nil || res != interact.Done || len(r.votes(t)) != 1 {
				t.Errorf("the retry = %v, %v, %d votes; want Done and the vote", res, err, len(r.votes(t)))
			}
		})
	}
}

func TestAPressOnAThreadThatIsGoneIsToldSoAndRecordsNothing(t *testing.T) {
	r := newRig(t)
	i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
	i.PromptID = feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: "999.9", Turn: turn}.String()
	if res, err := r.vote(t, i); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want Done", res, err)
	}
	if eph := r.eph.list(); len(r.votes(t)) != 0 || len(eph) != 1 || !strings.Contains(eph[0].text, "no longer") || r.waiting(t) != 0 {
		t.Errorf("votes %+v, told %+v; want the person told it can no longer be rated", r.votes(t), eph)
	}
}

// The Binding and the agent of a vote are the session's, as they were when the thread was made: a Binding that has left the
// config, or has another agent now, neither blocks nor relabels a vote on an earlier investigation.
func TestAVoteOnAThreadWhoseBindingIsGoneIsRecordedUnderWhatTheSessionHolds(t *testing.T) {
	r := newRig(t)
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "courier/removed", Alert: "KubePodCrashLooping", Agent: "kagent/old-agent", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	if res, err := r.vote(t, press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want Done", res, err)
	}
	got := r.votes(t)
	if len(got) != 1 || got[0].Binding != "courier/removed" || got[0].Agent != "kagent/old-agent" {
		t.Fatalf("votes = %+v; want one under the session's Binding and agent", got)
	}
	if n := r.tel.Counter(t, counter, map[string]string{"binding": "courier/removed", "agent": "kagent/old-agent", "alertname": "KubePodCrashLooping", "verdict": "useful"}); n != 1 {
		t.Errorf("%s under the session's Binding and agent = %v; want 1", counter, n)
	}
}

// A session made before the agent was kept has none: the vote is counted under "unknown", and not lost.
func TestAVoteOnASessionWithNoAgentIsRecordedUnderUnknown(t *testing.T) {
	r := newRig(t)
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: bindingID, LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	if res, err := r.vote(t, press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want Done", res, err)
	}
	if got := r.votes(t); len(got) != 1 || got[0].Agent != "unknown" {
		t.Errorf("votes = %+v; want one under the agent \"unknown\"", got)
	}
}

// The button's thread and turn are the platform's to confirm (as a prompt's interaction is): a vote is recorded only for the
// message that the turn's outbox row says it is, in the channel of the press. Anything else records nothing and tells no one.
func TestAPressIsTiedToTheMessageItWasMadeOn(t *testing.T) {
	t.Run("a press from another channel that names a thread of this one", func(t *testing.T) {
		r := newRig(t)
		r.members("UALICE1")
		i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
		i.At.Channel = "C2"
		if res, err := r.vote(t, i); err != nil || res != interact.Done {
			t.Fatalf("Process = %v, %v; want Done", res, err)
		}
		if len(r.votes(t)) != 0 || r.waiting(t) != 0 || r.count(t, "KubePodCrashLooping", store.VerdictUseful) != 0 {
			t.Errorf("votes %+v; want nothing recorded or counted for a press from C2 naming the thread of C1", r.votes(t))
		}
	})
	t.Run("a session of the other channel does not make it", func(t *testing.T) {
		r := newRig(t)
		other := store.ConversationKey{Connection: key.Connection, Channel: "C2", Thread: key.Thread}
		if err := r.st.UpsertSession(t.Context(), store.Session{Key: other, Binding: bindingID, Agent: agentID, LastActivity: t0}); err != nil {
			t.Fatal(err)
		}
		i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
		i.At.Channel = "C2" // the message of the turn is in C1: its row says so
		if _, err := r.vote(t, i); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.st.ListVotes(t.Context(), other); len(got) != 0 || len(r.votes(t)) != 0 {
			t.Errorf("votes %+v and %+v; want none: the turn's message is not in C2", got, r.votes(t))
		}
	})
	t.Run("a value whose turn is another message than the one pressed", func(t *testing.T) {
		r := newRig(t)
		i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
		i.At.ID = "200.2"
		if _, err := r.vote(t, i); err != nil {
			t.Fatal(err)
		}
		if len(r.votes(t)) != 0 || len(r.eph.list()) != 0 || r.waiting(t) != 0 {
			t.Errorf("votes %+v, told %+v; want nothing recorded and no one told", r.votes(t), r.eph.list())
		}
	})
	t.Run("a turn with no message recorded", func(t *testing.T) {
		r := newRig(t)
		i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
		i.PromptID = feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: key.Thread, Turn: "another-turn"}.String()
		if _, err := r.vote(t, i); err != nil {
			t.Fatal(err)
		}
		if len(r.votes(t)) != 0 {
			t.Errorf("votes %+v; want none for a turn that has no message", r.votes(t))
		}
	})
	t.Run("a row of another thread", func(t *testing.T) {
		r := newRig(t)
		const t2 = "other-turn"
		r.message(t, store.ConversationKey{Connection: key.Connection, Channel: "C1", Thread: "300.3"}, t2, "100.9") // the same message ref, a thread of another session
		i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
		i.PromptID = feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: key.Thread, Turn: t2}.String()
		if _, err := r.vote(t, i); err != nil {
			t.Fatal(err)
		}
		if len(r.votes(t)) != 0 {
			t.Errorf("votes %+v; want none: the turn's message is in another thread", r.votes(t))
		}
	})
	t.Run("a reply posted again under a row of its own is found by it", func(t *testing.T) {
		r := newRig(t)
		const t2 = "reposted-turn"
		r.message(t, key, outbox.RepostedSource(t2), "100.9") // the first attempt's row (turn/reply) never recorded a message
		if err := r.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: bindingID, Agent: agentID, LastActivity: t0}); err != nil {
			t.Fatal(err)
		}
		i := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
		i.PromptID = feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: key.Thread, Turn: t2}.String()
		if _, err := r.vote(t, i); err != nil {
			t.Fatal(err)
		}
		if got := r.votes(t); len(got) != 1 || got[0].Turn != t2 {
			t.Errorf("votes %+v; want the vote on the first source's turn", got)
		}
	})
}

// A vote is not lost for a record that cannot be written, nor an unacknowledged person for a told that fails.
func TestAFailedAuditOrTellingDoesNotCostTheVote(t *testing.T) {
	r := newRig(t)
	r.aud.err, r.eph.err = errors.New("audit down"), errors.New("slack down")
	res, err := r.vote(t, press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful))
	if err != nil || res != interact.Done || len(r.votes(t)) != 1 || r.waiting(t) != 0 {
		t.Errorf("Process = %v, %v, %d votes, %d waiting; want Done and the vote: a record of something done and a word to the person never block", res, err, len(r.votes(t)), r.waiting(t))
	}
	if !strings.Contains(r.logs.String(), "level=WARN") || !strings.Contains(r.logs.String(), "not audited") {
		t.Errorf("logged %q; want a warning that the vote was not audited", r.logs.String())
	}
}

func TestAPressRecordedBeforeARestartIsProcessedAfterIt(t *testing.T) {
	r := newRig(t)
	if err := r.h.Press(t.Context(), press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)); err != nil {
		t.Fatal(err)
	}
	// A new Handler over the same store, as a restart makes, finds the press in the store and processes it.
	h, err := feedback.New(feedback.Config{Store: r.st, Sessions: r.st, Authorizer: r.az, Ephemerals: r.eph, Auditor: r.aud, Now: func() time.Time { return t0.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if res, err := h.Process(r.tel.Ctx, presses[0]); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v", res, err)
	}
	if got := r.votes(t); len(got) != 1 || !got[0].At.Equal(t0) {
		t.Errorf("votes = %+v; want the vote, at the time of the press", got)
	}
}

func TestGiveUpTellsThePersonToPressAgainAndEndsThePress(t *testing.T) {
	r := newRig(t)
	if err := r.h.Press(t.Context(), press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	if err := r.h.GiveUp(t.Context(), presses[0]); err != nil {
		t.Fatal(err)
	}
	if eph := r.eph.list(); len(eph) != 1 || eph[0].user != "UALICE1" || !strings.Contains(eph[0].text, "again") || r.waiting(t) != 0 || len(r.votes(t)) != 0 {
		t.Errorf("told %+v, %d waiting, %d votes; want the person told to press again, the press gone and no vote", eph, r.waiting(t), len(r.votes(t)))
	}
}

func TestThePressesAPersonMayHaveWaitingAreBounded(t *testing.T) {
	r := newRig(t)
	var err error
	for range 4 { // the rig's limit is 3 for one person
		err = r.h.Press(t.Context(), press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful))
	}
	if !errors.Is(err, store.ErrLimit) {
		t.Errorf("the fourth press = %v; want store.ErrLimit", err)
	}
}

// The alertname label comes from text in an alert: it is kept to the name shape, and a Binding that sees more distinct
// names than the bound shows the rest as "other", so that no alert can make the series grow without end.
func TestTheAlertnameLabelIsBounded(t *testing.T) {
	label := func(alert string, kind store.FeedbackKind) string {
		r := newRig(t)
		if _, err := r.vote(t, r.thread(t, bindingID, key.Thread, alert, kind)); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{alert, "none", "unknown"} {
			if r.count(t, want, store.VerdictUseful) == 1 {
				return want
			}
		}
		return "other"
	}
	if got := label("KubePodCrashLooping", store.FeedbackInvestigation); got != "KubePodCrashLooping" {
		t.Errorf("label of a name = %q", got)
	}
	if got := label("", store.FeedbackAnswer); got != "none" {
		t.Errorf("label of an answer in a thread no alert started = %q; want none", got)
	}
	if got := label("", store.FeedbackInvestigation); got != "unknown" {
		t.Errorf("label of an investigation whose alert has no name = %q; want unknown", got)
	}

	// The bound is each Binding's: one that has used its names up leaves another its own.
	const second = "courier/second"
	r := newRig(t)
	seen := map[string]bool{}
	for i := range feedback.MaxAlertLabels + 5 {
		alert := fmt.Sprintf("Alert%d", i)
		if _, err := r.vote(t, r.thread(t, bindingID, fmt.Sprintf("%d.1", 200+i), alert, store.FeedbackInvestigation)); err != nil {
			t.Fatal(err)
		}
		seen[alert] = r.count(t, alert, store.VerdictUseful) == 1
	}
	named := 0
	for _, counted := range seen {
		if counted {
			named++
		}
	}
	if named != feedback.MaxAlertLabels || r.count(t, "other", store.VerdictUseful) != 5 {
		t.Errorf("%d names have a series of their own, %v votes under other; want %d and 5", named, r.count(t, "other", store.VerdictUseful), feedback.MaxAlertLabels)
	}
	if _, err := r.vote(t, r.thread(t, second, "900.1", "FirstOfTheSecond", store.FeedbackInvestigation)); err != nil {
		t.Fatal(err)
	}
	if n := r.countFor(t, second, "FirstOfTheSecond", store.VerdictUseful); n != 1 {
		t.Errorf("a second Binding's first name has %v votes under its own name; want 1: the cap is each Binding's", n)
	}
}

func TestAnAlertNameOutsideTheNameShapeIsNotKept(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"KubePodCrashLooping", "KubePodCrashLooping"},
		{"node:cpu_high-1.x", "node:cpu_high-1.x"},
		{"", ""},
		{"Alert Name", ""},             // a space: it is a sentence, not a name
		{"Alert\nIgnore previous", ""}, // a line break
		{"Älert", ""},                  // not ASCII
		{"Alert\x00", ""},              // a NUL, which Postgres text cannot hold
		{strings.Repeat("a", 128), strings.Repeat("a", 128)},
		{strings.Repeat("a", 129), ""},
	} {
		if got := feedback.AlertName(tc.in); got != tc.want {
			t.Errorf("AlertName(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
