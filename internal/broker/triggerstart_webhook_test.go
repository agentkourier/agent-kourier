package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/trigger"
)

const (
	hooksConn = "agent-kourier-system/hooks"
	hooksRepo = "acme/widgets"
	// hookTitle is text the sender wrote, which reaches the agent in the untrusted block alone, and no log line, note or audit entry.
	hookTitle = "Retry the token refresh"
)

// webhookMatch is the match of the event e of a pull request, #42 opened, of hooksRepo: what the Intake hands the starter for a
// webhookEvent trigger. Its Fields hold what a botMessage trigger's would, which a webhook start must not read.
func webhookMatch(e string) trigger.Match {
	ev := &chat.WebhookEvent{Delivery: "d-" + e, Actor: "octocat", Fields: []chat.WebhookField{
		{Name: "number", Value: "42", ID: true, Present: true},
		{Name: "sha", ID: true, Invalid: true},
		{Name: "title", Value: hookTitle, Present: true},
	}}
	return trigger.Match{
		Binding: startBinding, Index: 0, Webhook: true, PromptTemplate: "Review pull request #{{ .Fields.number }} of {{ .Channel }}.",
		MessageID: "webhook:" + hooksConn + ":" + hooksRepo + ":" + e,
		Fields:    map[string]string{"alertname": "KubePodCrashLooping", "state": "FIRING", "key": "k"},
		Received:  time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		Message: chat.Inbound{At: chat.Ref{Platform: "webhook", Conn: hooksConn, Channel: hooksRepo, Thread: "pr-42", ID: e}, User: "octocat",
			Webhook: ev},
	}
}

// newWebhookStarter is newStarter whose platform makes no permalink, as a webhook connection's does not, and fails the test if it is
// asked for one.
func newWebhookStarter(t *testing.T) *starter {
	t.Helper()
	s := newStarter(t)
	s.links = chattest.Same[chat.Permalinker](linkFunc(func(r chat.Ref) (string, bool) {
		t.Errorf("a permalink was asked for %+v; a webhook event has none", r)
		return "", false
	}))
	return s
}

// A webhook event's start is a turn of the thread the connection derived, under the session's key of that thread, named by the
// event's ID: nobody's (the system user's), on a platform with no replies, with the webhookEvent source and no subject, whatever
// the match's Fields say. Its text is the template over the channel and the event's id fields, then the event in the untrusted block.
func TestOnStartOfAWebhookEventIsATurnOfItsThreadWithNoReplies(t *testing.T) {
	s := newWebhookStarter(t)
	if err := s.OnStart(s.tel.Ctx, webhookMatch("e1")); err != nil {
		t.Fatalf("OnStart = %v", err)
	}
	if len(s.sub.got) != 1 {
		t.Fatalf("%d turns submitted; want 1", len(s.sub.got))
	}
	in := s.sub.got[0]
	want := session.Inbound{Key: session.Key{Connection: hooksConn, Channel: hooksRepo, Thread: "pr-42"}, Binding: startBinding,
		MessageID: "webhook:" + hooksConn + ":" + hooksRepo + ":e1", MessageRef: "e1", Create: true, NoReplies: true,
		Origin: store.Origin{Source: store.OriginWebhookEvent}, Text: in.Text}
	if in != want {
		t.Errorf("submitted %+v; want %+v", in, want)
	}
	if !strings.HasPrefix(in.Text, "Review pull request #42 of acme/widgets.\n\nThe webhook event that triggered this is below") {
		t.Errorf("the turn starts %q; want the template over the event, then its block", firstLine(in.Text))
	}
	tag := untrustedTag(t, in.Text)
	if before, block, _ := strings.Cut(in.Text, "<"+tag+">"); strings.Contains(before, hookTitle) || !strings.Contains(block, "<title>"+hookTitle+"</title>") ||
		!strings.Contains(block, "<actor>octocat</actor>") {
		t.Errorf("the event's actor and text field are not in the block alone:\n%s", in.Text)
	}
	if strings.Contains(s.logs.String(), hookTitle) {
		t.Errorf("a text field reached the log:\n%s", s.logs)
	}
}

// Two events of one pull request are two turns of one session: the same key, each its own message.
func TestOnStartOfTwoWebhookEventsOfOneThreadIsOneSession(t *testing.T) {
	s := newWebhookStarter(t)
	for _, e := range []string{"e1", "e2"} {
		if err := s.OnStart(s.tel.Ctx, webhookMatch(e)); err != nil {
			t.Fatalf("OnStart %s = %v", e, err)
		}
	}
	if len(s.sub.got) != 2 || s.sub.got[0].Key != s.sub.got[1].Key || s.sub.got[0].MessageID == s.sub.got[1].MessageID {
		t.Errorf("submitted %+v; want two turns of one key", s.sub.got)
	}
}

// A webhook event in a thread whose session has expired, or whose Binding is gone, replaces the session, on a platform with no
// replies, and the start says so. When the session manager withholds the replacement (the old session may still be at work, or
// the body was queued before), nothing would change if it were asked again: the start is not made and the event is dropped,
// rather than answered 503 and sent again for ever. Nothing is noted in the thread.
func TestOnStartOfAWebhookEventInAnEndedThread(t *testing.T) {
	for _, tc := range []struct {
		name    string
		res     session.Result
		started bool
		log     string
	}{
		{"replaced", session.Result{Disposition: session.Created, Admission: session.Admission{Kind: session.AdmitTurn, Replaces: true}}, true,
			"chat: a webhook event replaced the ended session of its thread"},
		{"expired, withheld", session.Result{Disposition: session.Expired, Admission: session.Admission{Kind: session.AdmitNotice, Notice: session.NoticeExpired}}, false,
			"chat: a webhook event's thread cannot be continued yet, so the event was dropped"},
		{"Binding gone, withheld", session.Result{Disposition: session.BindingGone, Admission: session.Admission{Kind: session.AdmitNotice, Notice: session.NoticeBindingGone}}, false,
			"chat: a webhook event's thread cannot be continued yet, so the event was dropped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newWebhookStarter(t)
			s.sub.res = tc.res
			err := s.OnStart(s.tel.Ctx, webhookMatch("e1"))
			if tc.started && err != nil || !tc.started && !errors.Is(err, trigger.ErrWithheld) {
				t.Fatalf("OnStart = %v; want started %v, or ErrWithheld", err, tc.started)
			}
			if len(s.notes.got) != 0 {
				t.Errorf("notes %+v; want none", s.notes.got)
			}
			logs := s.logs.String()
			for _, want := range []string{tc.log, "thread=pr-42", "delivery=d-e1", "disposition=" + string(tc.res.Disposition)} {
				if !strings.Contains(logs, want) {
					t.Errorf("the log lacks %q:\n%s", want, logs)
				}
			}
		})
	}
}

// A template that reads an id field that is Invalid refuses the start (D2), as a value that fails its check does: no turn, the
// refusal audited and counted with the reason invalid, and nothing of the event in the log, the audit or the error.
func TestOnStartRefusesAWebhookTemplateThatReadsAnInvalidField(t *testing.T) {
	s := newWebhookStarter(t)
	m := webhookMatch("e1")
	m.PromptTemplate = "Review {{ .Fields.sha }} of {{ .Channel }}."
	err := s.OnStart(s.tel.Ctx, m)
	if !errors.Is(err, trigger.ErrNotStarted) || errors.Is(err, trigger.ErrWithheld) || len(s.sub.got) != 0 {
		t.Fatalf("OnStart = %v after %d turns; want ErrNotStarted and none", err, len(s.sub.got))
	}
	want := audit.TriggerRefusal{MessageID: m.MessageID, Trigger: 0, Ref: "Fields.sha", Reason: "invalid"}
	if len(s.aud.got) != 1 || s.aud.got[0] != want {
		t.Errorf("audited %+v; want %+v", s.aud.got, want)
	}
	labels := map[string]string{"binding": startBinding, "trigger": "0", "ref": "Fields.sha", "reason": "invalid"}
	if got := s.tel.Counter(t, "agentkourier_trigger_refused_total", labels); got != 1 {
		t.Errorf("agentkourier_trigger_refused_total%v = %v; want 1", labels, got)
	}
	for name, text := range map[string]string{"the log": s.logs.String(), "the error": err.Error(), "the audit": fmt.Sprintf("%+v", s.aud.got)} {
		if strings.Contains(text, hookTitle) || strings.Contains(text, "octocat") {
			t.Errorf("%s holds a value of the event:\n%s", name, text)
		}
	}
}

// webhookWorld is the Intake of the webhook connection hooks as the broker builds it (newIntakeConfig), over the starter s and a
// database of the test's, in which hooksRepo is bound to startBinding with one webhookEvent trigger of the limits given.
func webhookWorld(t *testing.T, s *starter, limits v1alpha1.TriggerLimits) *trigger.Intake {
	t.Helper()
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	conn := v1alpha1.ChatConnection{ObjectMeta: meta("agent-kourier-system", "hooks"), Spec: webhookConnectionSpec()}
	conn.Spec.AllowedNamespaces = []string{"sre"}
	conn.Spec.Webhook.Thread = &v1alpha1.WebhookThread{Field: "number", Prefix: "pr-"}
	conn.Spec.Webhook.Fields = []v1alpha1.WebhookField{
		{Name: "number", From: []string{"/pull_request/number"}, Kind: v1alpha1.WebhookKindID},
		{Name: "sha", From: []string{"/pull_request/head/sha"}, Kind: v1alpha1.WebhookKindID},
		{Name: "title", From: []string{"/pull_request/title"}},
	}
	b := v1alpha1.Binding{ObjectMeta: meta("sre", "sre-alerts"), Spec: v1alpha1.BindingSpec{
		Agent:    v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "kagent"}, Namespace: "kagent", Name: "sre-agent"},
		Identity: v1alpha1.Identity{UserID: "agent-kourier-sre"},
		Chat: v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "hooks"}, Channel: hooksRepo, Output: v1alpha1.OutputNone,
			Triggers: []v1alpha1.Trigger{{Type: v1alpha1.TriggerWebhookEvent, PromptTemplate: "Review pull request #{{ .Fields.number }}.", Limits: limits}}}}}
	b.Default()
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Resolve(t.Context(), &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: meta("agent-kourier-system", "kagent"), Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, AllowedNamespaces: []string{"sre"}}}},
		ChatConnections: []v1alpha1.ChatConnection{conn},
		Bindings:        []v1alpha1.Binding{b},
	}, keysOfEveryPlatform{})
	if err != nil {
		t.Fatal(err)
	}
	st := sqlitetest.Open(t)
	cfg, err := newIntakeConfig(intakeParts{
		conn: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "hooks"}, snapshot: func() *config.Snapshot { return snap },
		submitter: s.sub, interactions: func(context.Context, chat.Interaction) error { return nil }, notice: s.notes.take,
		decidePress: func(context.Context, chat.Interaction) (chat.PressOutcome, error) { return chat.PressOutcome{}, nil }, starter: s.triggerStarter,
		threads: st, runs: st, replies: st, log: s.log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return trigger.NewIntake(cfg)
}

// A webhook event whose thread's ended session the session manager will not replace yet (past its threadTTL, or its Binding gone,
// with the old session still at work or the body queued before) is asked about before the limits, as the start would be made: on a
// platform with no replies, by the event's message ID. It is taken, so that the platform answers 200, decided not_started and logged
// with its thread and delivery, and it spends no place in the limits: with one run an hour and one a day, the next event starts.
func TestAWithheldWebhookReplacementSpendsNoPlaceInTheLimits(t *testing.T) {
	s := newWebhookStarter(t)
	withhold := true
	var asked []session.Inbound
	s.takes = func(_ context.Context, in session.Inbound) (bool, error) {
		asked = append(asked, in)
		return !withhold, nil
	}
	in := webhookWorld(t, s, v1alpha1.TriggerLimits{RateLimit: &v1alpha1.RateLimit{MaxRuns: 1, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 1}, MaxRunsPerDay: new(int32(1))})
	if err := in.Message(s.tel.Ctx, webhookMatch("e1").Message); err != nil {
		t.Fatalf("Message e1 = %v; want the event taken", err)
	}
	if len(s.sub.got) != 0 || len(s.notes.got) != 0 {
		t.Fatalf("submitted %+v, noted %+v; want nothing for a withheld replacement", s.sub.got, s.notes.got)
	}
	withhold = false
	if err := in.Message(s.tel.Ctx, webhookMatch("e2").Message); err != nil {
		t.Fatalf("Message e2 = %v", err)
	}
	if len(s.sub.got) != 1 {
		t.Fatalf("%d turns submitted; want e2's, in the place e1 did not take", len(s.sub.got))
	}
	for decision, want := range map[string]float64{"not_started": 1, "started": 1, "limited": 0} {
		if got := s.tel.Counter(t, "agentkourier_trigger_decisions_total", map[string]string{"binding": startBinding, "decision": decision}); got != want {
			t.Errorf("decisions %s = %v; want %v", decision, got, want)
		}
	}
	key := session.Key{Connection: hooksConn, Channel: hooksRepo, Thread: "pr-42"}
	if len(asked) != 2 || asked[0].Key != key || !asked[0].NoReplies || !asked[0].Create || asked[0].WholeThread ||
		!strings.HasSuffix(asked[0].MessageID, ":e1") || asked[1].MessageID != s.sub.got[0].MessageID {
		t.Errorf("asked %+v; want each event asked about in thread %+v, with no replies, by its message ID", asked, key)
	}
	for _, want := range []string{"chat: a webhook event's thread cannot be continued yet, so the event was dropped", "thread=pr-42", "delivery=d-e1"} {
		if !strings.Contains(s.logs.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, s.logs)
		}
	}
}

// From the Intake through the starter: a webhook event whose ended session's replacement is withheld is taken, so that the platform
// answers 200, decided not_started, and not remembered: the withholding is transient (the old session has a task, or a reply not yet
// delivered), so the sender's redelivery of the same body, once the session has settled, is decided again and starts. That holds
// whether the session said so before the limits (Takes) or only as the start was submitted (the old session took a turn meanwhile);
// in the second the first delivery's place is its redelivery's, so one run an hour does not stop it. Nothing is noted in the thread.
func TestAWithheldWebhookEventStartsWhenItIsDeliveredAgainOnceTheSessionSettles(t *testing.T) {
	withheld := session.Result{Disposition: session.Expired, Admission: session.Admission{Kind: session.AdmitNotice, Notice: session.NoticeExpired}}
	for _, tc := range []struct {
		name     string
		withhold func(s *starter, on bool)
	}{
		{"before the limits", func(s *starter, on bool) {
			s.takes = func(context.Context, session.Inbound) (bool, error) { return !on, nil }
		}},
		{"as the start is submitted", func(s *starter, on bool) {
			s.sub.res = session.Result{}
			if on {
				s.sub.res = withheld
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newWebhookStarter(t)
			in := webhookWorld(t, s, v1alpha1.TriggerLimits{RateLimit: &v1alpha1.RateLimit{MaxRuns: 1, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 1}, MaxRunsPerDay: new(int32(1))})
			m := webhookMatch("e1").Message
			tc.withhold(s, true)
			if err := in.Message(s.tel.Ctx, m); err != nil {
				t.Fatalf("Message = %v; want the withheld event taken", err)
			}
			tc.withhold(s, false) // the session settles
			if err := in.Message(s.tel.Ctx, m); err != nil {
				t.Fatalf("Message of the redelivery = %v", err)
			}
			if got := s.sub.got; len(got) == 0 || got[len(got)-1].MessageID == "" || len(s.notes.got) != 0 {
				t.Fatalf("submitted %+v, noted %+v; want the redelivery's turn, and no note", got, s.notes.got)
			}
			for decision, want := range map[string]float64{"not_started": 1, "started": 1, "dedup": 0, "limited": 0} {
				if got := s.tel.Counter(t, "agentkourier_trigger_decisions_total", map[string]string{"binding": startBinding, "decision": decision}); got != want {
					t.Errorf("decisions %s = %v; want %v", decision, got, want)
				}
			}
		})
	}
}
