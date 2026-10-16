package chat_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// webhookBot is the incoming webhook's bot ID in the owner's alert channel: it posts Alertmanager's alerts,
// Grafana's and the Watchdog heartbeat.
const webhookBot = "B0123456789"

// The messages of the owner's alert channel, as the adapter flattens them. The kind ones are what the full
// tier's Alertmanager posts through the same webhook (the title tagged [kind], the receiver in the link);
// the production ones, Grafana's and the heartbeat share the channel.
const (
	kindLink = "http://alertmanager.kind.local/#/alerts?receiver=slack-critical-kind"
	prodLink = "https://alertmanager.example.com/#/alerts?receiver=slack-critical"
	beatLink = "https://alertmanager.example.com/#/alerts?receiver=slack-heartbeat"
)

var (
	kindFiring = chat.BotMessage{BotID: webhookBot, Title: "[kind] FIRING (1) - KubePodCrashLooping", TitleLink: kindLink,
		Fallback:       "[FIRING:1] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical) | " + kindLink,
		AttachmentText: "*KubePodCrashLooping* - critical\nPod is crash looping.\nPod shop/checkout-7d9f8b6c5-x2x4k (checkout) is in waiting state (reason: \"CrashLoopBackOff\").\n"}
	kindResolved = chat.BotMessage{BotID: webhookBot, Title: "[kind] RESOLVED - KubePodCrashLooping", TitleLink: kindLink,
		Fallback:       "[RESOLVED] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical) | " + kindLink,
		AttachmentText: "*KubePodCrashLooping* - critical\n"}
	kindGroup = chat.BotMessage{BotID: webhookBot, Title: "[kind] FIRING (3) - KubePodNotReady", TitleLink: kindLink,
		Fallback: "[FIRING:3] KubePodNotReady shop (critical) | " + kindLink}
	prodFiring = chat.BotMessage{BotID: webhookBot, Title: "FIRING (1) - KubeAPIErrorBudgetBurn", TitleLink: prodLink,
		Fallback:       "[FIRING:1] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | " + prodLink,
		AttachmentText: "*KubeAPIErrorBudgetBurn* - critical\nThe API server is burning too much error budget.\n"}
	prodResolved = chat.BotMessage{BotID: webhookBot, Title: "RESOLVED - KubeAPIErrorBudgetBurn", TitleLink: prodLink,
		Fallback: "[RESOLVED] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | " + prodLink}
	grafanaFiring = chat.BotMessage{BotID: webhookBot, Title: "FIRING: HighRequestLatency (payments)", TitleLink: "https://grafana.example.com/alerting/list",
		Fallback: "FIRING: HighRequestLatency (payments)", AttachmentText: "**Firing**\n\nValue: A=0.92\nLabels:\n - alertname = HighRequestLatency\n"}
	heartbeat = chat.BotMessage{BotID: webhookBot, Title: "Prometheus heartbeat", TitleLink: beatLink,
		Fallback:       "[FIRING:1] Watchdog  (none prometheus) | " + beatLink,
		AttachmentText: "Watchdog firing — Prometheus + Alertmanager are alive."}
)

// botInbound is a top-level bot message in channel at id (a ts), with no text of its own and the flattened
// attachment b, as an incoming webhook posts.
func botInbound(channel, id string, b chat.BotMessage) chat.Inbound {
	return chat.Inbound{At: chat.Ref{Platform: "slack", Conn: conn, Channel: channel, ID: id}, Bot: &b}
}

// botTrigger is a botMessage trigger on bot, as the Binding loader would hand it on after defaulting. Its limits are
// the ones every botMessage trigger must set, wide enough that no test reaches them and no daily cap.
func botTrigger(bot string, mutate func(*v1alpha1.Trigger)) v1alpha1.Trigger {
	t := v1alpha1.Trigger{Type: v1alpha1.TriggerBotMessage, From: &v1alpha1.BotSource{BotID: bot},
		PromptTemplate: "An alert.",
		Match:          map[v1alpha1.MessageField]string{},
		Limits: v1alpha1.TriggerLimits{
			RateLimit:     &v1alpha1.RateLimit{MaxRuns: 1000, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 1000},
			MaxRunsPerDay: new(int32(0)),
		},
	}
	if mutate != nil {
		mutate(&t)
	}
	return t
}

// trialTrigger is the owner's trial: the Alertmanager trigger written in full for the webhook bot, narrowed to
// the kind receiver (anchored, so that production's receiver of the same prefix stays out), with the heartbeat
// excluded.
func trialTrigger() v1alpha1.Trigger {
	tr := chattest.AlertmanagerTrigger(webhookBot)
	tr.Match[v1alpha1.FieldTitleLink] = `receiver=slack-critical-kind$`
	tr.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `^Prometheus heartbeat$`}
	return tr
}

// kindOnly is the trial's trigger with only that narrowing: the webhook bot's messages whose link names the kind
// receiver, and not the heartbeat. Tests of the intake's own mechanics use it, so that they do not depend on
// what the Alertmanager trigger says.
func kindOnly() v1alpha1.Trigger {
	return botTrigger(webhookBot, func(t *v1alpha1.Trigger) {
		t.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldTitleLink: `receiver=slack-critical-kind$`}
		t.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `^Prometheus heartbeat$`}
	})
}

// productionTrigger is what the production Binding would say: the receiver anchored at its end.
func productionTrigger() v1alpha1.Trigger {
	tr := trialTrigger()
	tr.Match[v1alpha1.FieldTitleLink] = `receiver=slack-critical$`
	return tr
}

// startsEveryMatch is tr with a close rule that no message satisfies, so that every match of it, a resolve too,
// starts a turn and the matcher's tests see its fields through the start hook.
func startsEveryMatch(tr v1alpha1.Trigger) v1alpha1.Trigger {
	tr.Thread.CloseWhen = map[string]string{"state": "NEVER"}
	return tr
}

// lockedBuffer is a bytes.Buffer that the logger and the test may use at once.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// clock is a test's time: it stands still until the test moves it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// noon is when a test's clock starts.
var noon = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// triggered is an Intake over a config in which C1 is bound to sre/sre-alerts with the given triggers, and G1
// to payments/pay with none. It keeps its threads in a real store, and records what the start hook is given
// and the notices the Intake asks for; its metrics and log are readable.
type triggered struct {
	in      *chat.Intake
	cfg     chat.IntakeConfig // what in was made from, for a test that wants another Intake over the same table
	snap    atomic.Pointer[config.Snapshot]
	sub     *submitter
	ctx     context.Context
	reg     *prometheus.Registry
	logs    *lockedBuffer
	st      *sqlite.Store
	dbPath  string
	clk     *clock
	notes   *notices
	mu      sync.Mutex
	matches []chat.TriggerMatch // what the start hook was given: the matches that start a turn
	hookErr error
}

func (h *triggered) hook(_ context.Context, m chat.TriggerMatch) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.matches = append(h.matches, m)
	return h.hookErr
}

func (h *triggered) got() []chat.TriggerMatch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]chat.TriggerMatch(nil), h.matches...)
}

// counter returns the value of the counter name for the Binding, 0 if it has no series.
func (h *triggered) counter(t *testing.T, name, binding string) float64 {
	t.Helper()
	families, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			for _, l := range m.Label {
				if l.GetName() == "binding" && l.GetValue() == binding {
					return m.Counter.GetValue()
				}
			}
		}
	}
	return 0
}

// reopenStore opens the test's database, closing the one that was open before: a restart. The Intake of a
// config made before it holds the old store, so a test that restarts makes a new Intake (see restart).
func (h *triggered) reopenStore(t *testing.T) {
	t.Helper()
	if h.st != nil {
		if err := h.st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	st, err := sqlite.Open(t.Context(), h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.st = st
}

// snapshotOf is a routing table in which C1 is bound to sre/sre-alerts with the triggers, defaulted and checked as
// the loader would, and G1 to payments/pay with none.
func snapshotOf(t *testing.T, triggers []v1alpha1.Trigger) *config.Snapshot {
	t.Helper()
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	bind := func(ns, name, channel string, triggers []v1alpha1.Trigger) v1alpha1.Binding {
		b := v1alpha1.Binding{ObjectMeta: meta(ns, name), Spec: v1alpha1.BindingSpec{
			Agent:    v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "kagent"}, Namespace: "kagent", Name: "sre-agent"},
			Identity: v1alpha1.Identity{UserID: "agent-kourier-sre"},
			Chat:     v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "slack-main"}, Channel: channel, Triggers: triggers}}}
		b.Default() // as the loader does: no triggers means the mention trigger
		return b
	}
	allow := []string{"sre", "payments"}
	snap, err := config.Resolve(t.Context(), &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: meta("agent-kourier-system", "kagent"), Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, AllowedNamespaces: allow}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: meta("agent-kourier-system", "slack-main"), Spec: v1alpha1.ChatConnectionSpec{AllowedNamespaces: allow}}},
		Bindings:        []v1alpha1.Binding{bind("sre", "sre-alerts", "C1", triggers), bind("payments", "pay", "G1", nil)},
	}, allKeys{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rb := range []string{"sre/sre-alerts"} { // the triggers must be ones the loader would accept
		ns, name, _ := strings.Cut(rb, "/")
		b, ok := snap.Binding(ns, name)
		if !ok {
			t.Fatalf("no Binding %s", rb)
		}
		if err := b.Binding.Validate(); err != nil {
			t.Fatalf("the test's triggers are invalid: %v", err)
		}
	}
	return snap
}

// reload swaps in a routing table with the given triggers, as a config reload does.
func (h *triggered) reload(t *testing.T, triggers ...v1alpha1.Trigger) {
	t.Helper()
	h.snap.Store(snapshotOf(t, triggers))
}

func newTriggered(t *testing.T, triggers ...v1alpha1.Trigger) *triggered {
	t.Helper()
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &triggered{sub: &submitter{}, reg: reg, logs: &lockedBuffer{}, ctx: telemetry.WithObserver(t.Context(), obs),
		clk: &clock{now: noon}, notes: &notices{}, dbPath: sqlitetest.Path(t)}
	h.snap.Store(snapshotOf(t, triggers))
	h.reopenStore(t)
	h.cfg = chat.IntakeConfig{
		Connection: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "slack-main"},
		Snapshot:   h.snap.Load, Submitter: h.sub, OnStart: h.hook,
		TriggerThreads: h.st, Notice: h.notes.take, Now: h.clk.Now,
		Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	h.in = chat.NewIntake(h.cfg)
	return h
}

// The matcher, on explicit regexes: every match entry must match, no exclude entry may, every
// extract must find its group, and a trigger takes only the bot it names.
func TestABotMessageMatchesATriggerOnItsRegexes(t *testing.T) {
	const (
		other = "B0OTHERBOT1"
		title = v1alpha1.FieldTitle
	)
	msg := func(mutate func(*chat.BotMessage)) chat.BotMessage {
		b := chat.BotMessage{BotID: webhookBot, Title: "FIRING (1) - Disk", TitleLink: "http://am/#/alerts?receiver=a", Fallback: "[FIRING:1] Disk full | http://am", Footer: "key: disk/ns1", AttachmentText: "disk is at 97%"}
		if mutate != nil {
			mutate(&b)
		}
		return b
	}
	tests := []struct {
		name    string
		trigger v1alpha1.Trigger
		msg     chat.BotMessage
		match   bool
		fields  map[string]string
	}{
		{"a trigger with no regexes takes every message of its bot", botTrigger(webhookBot, nil), msg(nil), true, map[string]string{}},
		{"another bot's message is not taken, whatever it says", botTrigger(webhookBot, nil), msg(func(b *chat.BotMessage) { b.BotID = other }), false, nil},
		{"match on every named field",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Match = map[v1alpha1.MessageField]string{
					v1alpha1.FieldTitle: `^FIRING`, v1alpha1.FieldTitleLink: `receiver=a$`, v1alpha1.FieldFallback: `Disk full`,
					v1alpha1.FieldFooter: `^key: `, v1alpha1.FieldAttachmentText: `\d+%`}
			}), msg(nil), true, map[string]string{}},
		{"every match entry must match: one that does not is no match",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Match = map[v1alpha1.MessageField]string{title: `^FIRING`, v1alpha1.FieldFooter: `^nope`}
			}), msg(nil), false, nil},
		{"match is a search, not anchored, unless it says so",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) { tr.Match = map[v1alpha1.MessageField]string{title: `Disk`} }), msg(nil), true, map[string]string{}},
		{"an anchor is respected",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) { tr.Match = map[v1alpha1.MessageField]string{title: `^Disk`} }), msg(nil), false, nil},
		{"a field the message lacks is empty, so a regex that needs text fails",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) { tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldFooter: `.`} }),
			msg(func(b *chat.BotMessage) { b.Footer = "" }), false, nil},
		{"text is the message's own text, not the attachment's",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) { tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldText: `97%`} }), msg(nil), false, nil},
		{"exclude turns a message away that match accepts",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldFallback: `^\[FIRING`}
				tr.Exclude = map[v1alpha1.MessageField]string{title: `^Prometheus heartbeat$`}
			}), msg(func(b *chat.BotMessage) { b.Title = "Prometheus heartbeat" }), false, nil},
		{"any one exclude entry is enough",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Exclude = map[v1alpha1.MessageField]string{title: `^nope$`, v1alpha1.FieldAttachmentText: `97%`}
			}), msg(nil), false, nil},
		{"a message no exclude entry matches is kept",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Exclude = map[v1alpha1.MessageField]string{title: `^Prometheus heartbeat$`}
			}), msg(nil), true, map[string]string{}},
		{"extract takes the named group",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Extract = map[string]v1alpha1.Extract{
					"state": {Field: title, Regex: `^(?P<state>FIRING|RESOLVED)`},
					"key":   {Field: v1alpha1.FieldFooter, Regex: `key: (?P<key>\S+)`},
				}
			}), msg(nil), true, map[string]string{"state": "FIRING", "key": "disk/ns1"}},
		{"extract takes the group of its own name when the regex has several",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Extract = map[string]v1alpha1.Extract{"pct": {Field: v1alpha1.FieldAttachmentText, Regex: `(?P<what>\w+) is at (?P<pct>\d+)%`}}
			}), msg(nil), true, map[string]string{"pct": "97"}},
		{"a group that took part and matched empty text is a match, with an empty value",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Extract = map[string]v1alpha1.Extract{"note": {Field: v1alpha1.FieldFooter, Regex: `^(?P<note>x*)key`}}
			}), msg(func(b *chat.BotMessage) { b.Footer = "key: disk" }), true, map[string]string{"note": ""}},
		{"an optional group that took no part is a miss, so the trigger does not match",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Extract = map[string]v1alpha1.Extract{"key": {Field: v1alpha1.FieldFooter, Regex: `^key: (?:(?P<key>\S+)/)?ns1$`}}
			}), msg(func(b *chat.BotMessage) { b.Footer = "key: ns1" }), false, nil},
		{"the same optional group, taking part, is a match",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Extract = map[string]v1alpha1.Extract{"key": {Field: v1alpha1.FieldFooter, Regex: `^key: (?:(?P<key>\S+)/)?ns1$`}}
			}), msg(func(b *chat.BotMessage) { b.Footer = "key: disk/ns1" }), true, map[string]string{"key": "disk"}},
		{"an extract that does not match is no match",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Extract = map[string]v1alpha1.Extract{"key": {Field: v1alpha1.FieldFooter, Regex: `agent-kourier-key: (?P<key>\S+)`}}
			}), msg(nil), false, nil},
		{"one extract that misses spoils the rest",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Extract = map[string]v1alpha1.Extract{
					"state": {Field: title, Regex: `^(?P<state>FIRING)`},
					"key":   {Field: v1alpha1.FieldFooter, Regex: `agent-kourier-key: (?P<key>\S+)`},
				}
			}), msg(nil), false, nil},
		{"extract and match work together",
			botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
				tr.Match = map[v1alpha1.MessageField]string{title: `^FIRING`}
				tr.Extract = map[string]v1alpha1.Extract{"n": {Field: title, Regex: `\((?P<n>\d+)\)`}}
			}), msg(nil), true, map[string]string{"n": "1"}},
		{"a mention trigger is no bot trigger",
			v1alpha1.Trigger{Type: v1alpha1.TriggerMention}, msg(nil), false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, tt.trigger)
			if err := h.in.Message(h.ctx, botInbound("C1", "9.9", tt.msg)); err != nil {
				t.Fatal(err)
			}
			got := h.got()
			if (len(got) == 1) != tt.match || len(got) > 1 {
				t.Fatalf("matched %d times, want a match: %v", len(got), tt.match)
			}
			if tt.match && !maps.Equal(got[0].Fields, tt.fields) {
				t.Errorf("fields = %v, want %v", got[0].Fields, tt.fields)
			}
			unmatched := h.counter(t, "agentkourier_trigger_unmatched_total", "sre/sre-alerts")
			if tt.match != (unmatched == 0) {
				t.Errorf("unmatched count = %v for a message that matched: %v", unmatched, tt.match)
			}
		})
	}
}

// Triggers are tried in order and the first that matches takes the message. One that does not match
// (a filter, an exclude, an extract that misses, another bot) lets the next try, which is what allows two
// triggers that differ only in what they extract.
func TestTheFirstTriggerThatMatchesTakesTheMessage(t *testing.T) {
	// Load refuses two triggers that match the same messages, so the triggers differ in a match entry that
	// every message of the test passes.
	always := func(name string) v1alpha1.Trigger {
		return botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
			tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: "^(?:" + name + ")?"}
		})
	}
	needsKey := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldFooter: "^"}
		tr.Extract = map[string]v1alpha1.Extract{"key": {Field: v1alpha1.FieldFooter, Regex: `agent-kourier-key: (?P<key>\S+)`}}
	})
	optionalKey := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldAttachmentText: "^"}
		tr.Extract = map[string]v1alpha1.Extract{"key": {Field: v1alpha1.FieldFooter, Regex: `^(?:no (?P<key>key)|no key here)$`}}
	})
	hasState := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldTitleLink: "^"}
		tr.Extract = map[string]v1alpha1.Extract{"state": {Field: v1alpha1.FieldTitle, Regex: `^(?P<state>FIRING|RESOLVED)`}}
	})
	excluded := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `FIRING`}
	})
	otherBot := botTrigger("B0OTHERBOT1", nil)
	msg := chat.BotMessage{BotID: webhookBot, Title: "FIRING (1) - Disk", Footer: "no key here"}
	tests := []struct {
		name     string
		triggers []v1alpha1.Trigger
		index    int
		fields   map[string]string
	}{
		{"both match, the first wins", []v1alpha1.Trigger{always("first"), always("second")}, 0, map[string]string{}},
		{"both match, in the other order", []v1alpha1.Trigger{hasState, always("second")}, 0, map[string]string{"state": "FIRING"}},
		{"the first misses an extract, the second takes it", []v1alpha1.Trigger{needsKey, hasState}, 1, map[string]string{"state": "FIRING"}},
		{"the first has an optional group that took no part, so it misses and the second takes it", []v1alpha1.Trigger{optionalKey, hasState}, 1, map[string]string{"state": "FIRING"}},
		{"the first is excluded, the second takes it", []v1alpha1.Trigger{excluded, always("second")}, 1, map[string]string{}},
		{"the first is for another bot, the second takes it", []v1alpha1.Trigger{otherBot, always("second")}, 1, map[string]string{}},
		{"a mention trigger first still counts its place in the list", []v1alpha1.Trigger{{Type: v1alpha1.TriggerMention}, always("second")}, 1, map[string]string{}},
		{"a later trigger that would match is not reached", []v1alpha1.Trigger{always("first"), hasState}, 0, map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, tt.triggers...)
			if err := h.in.Message(h.ctx, botInbound("C1", "9.9", msg)); err != nil {
				t.Fatal(err)
			}
			got := h.got()
			if len(got) != 1 || got[0].Index != tt.index || !maps.Equal(got[0].Fields, tt.fields) {
				t.Fatalf("matches %+v, want one at index %d with fields %v", got, tt.index, tt.fields)
			}
		})
	}
}

// What a match hands onward: the Binding, the trigger's index in the Binding's list, the extracted values
// and the message as it came, whole.
func TestAMatchCarriesTheBindingTheIndexTheFieldsAndTheMessage(t *testing.T) {
	h := newTriggered(t, v1alpha1.Trigger{Type: v1alpha1.TriggerMention}, trialTrigger())
	m := botInbound("C1", "9.9", kindFiring)
	if err := h.in.Message(h.ctx, m); err != nil {
		t.Fatal(err)
	}
	got := h.got()
	if len(got) != 1 {
		t.Fatalf("matched %d times, want 1", len(got))
	}
	g := got[0]
	if g.Binding != "sre/sre-alerts" || g.Index != 1 || g.Message.At != m.At || g.Message.Bot == nil || *g.Message.Bot != kindFiring {
		t.Errorf("match = %+v; want Binding sre/sre-alerts, index 1 and the message whole", g)
	}
	if g.Fields["state"] != "FIRING" || g.Fields["key"] != "KubePodCrashLooping shop" || g.Fields["alertname"] != "KubePodCrashLooping" {
		t.Errorf("fields = %v; want state, key and alertname of the alert", g.Fields)
	}
	if n := h.counter(t, "agentkourier_trigger_matched_total", "sre/sre-alerts"); n != 1 {
		t.Errorf("agentkourier_trigger_matched_total = %v, want 1", n)
	}
	if len(h.sub.submitted()) != 0 {
		t.Errorf("a match started a session: %+v (the Intake hands the start on, and CT-4 starts the session)", h.sub.submitted())
	}
}

// The owner's alert channel, through the Alertmanager trigger with the trial's narrowing: the kind alerts
// match and carry their state and key; production's, Grafana's and the heartbeat do not.
func TestTheTrialTriggerTakesKindAlertsAndNothingElse(t *testing.T) {
	tests := []struct {
		name   string
		msg    chat.BotMessage
		fields map[string]string // nil: no match
	}{
		{"kind alert firing", kindFiring, map[string]string{"state": "FIRING", "key": "KubePodCrashLooping shop", "alertname": "KubePodCrashLooping"}},
		{"kind alert resolved", kindResolved, map[string]string{"state": "RESOLVED", "key": "KubePodCrashLooping shop", "alertname": "KubePodCrashLooping"}},
		{"kind alerts grouped", kindGroup, map[string]string{"state": "FIRING", "key": "KubePodNotReady shop", "alertname": "KubePodNotReady"}},
		{"production alert firing, receiver slack-critical", prodFiring, nil},
		{"production alert resolved", prodResolved, nil},
		{"Grafana alerting", grafanaFiring, nil},
		{"the Watchdog heartbeat", heartbeat, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, startsEveryMatch(trialTrigger()))
			if err := h.in.Message(h.ctx, botInbound("C1", "9.9", tt.msg)); err != nil {
				t.Fatal(err)
			}
			got := h.got()
			if tt.fields == nil {
				if len(got) != 0 {
					t.Fatalf("matched %+v; want it ignored", got)
				}
				return
			}
			if len(got) != 1 || !maps.Equal(got[0].Fields, tt.fields) {
				t.Fatalf("matches %+v; want one with fields %v", got, tt.fields)
			}
		})
	}
}

// The production Binding's trigger (receiver=slack-critical, anchored) takes production's alerts and turns
// the kind ones away, which share the channel while the trial runs.
func TestTheProductionTriggerTakesProductionAlertsNotKindOnes(t *testing.T) {
	for _, tt := range []struct {
		name  string
		msg   chat.BotMessage
		match bool
	}{
		{"production firing", prodFiring, true},
		{"production resolved", prodResolved, true},
		{"kind firing", kindFiring, false},
		{"kind resolved", kindResolved, false},
		{"Grafana", grafanaFiring, false},
		{"heartbeat", heartbeat, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, startsEveryMatch(productionTrigger()))
			if err := h.in.Message(h.ctx, botInbound("C1", "9.9", tt.msg)); err != nil {
				t.Fatal(err)
			}
			if got := h.got(); (len(got) == 1) != tt.match {
				t.Fatalf("matches %+v; want a match: %v", got, tt.match)
			}
		})
	}
}

// Why the production Binding must anchor its receiver: "receiver=slack-critical" is a prefix of
// "receiver=slack-critical-kind", so an unanchored match takes the trial's alerts too. This test pins that
// hazard, and the anchored regexes that avoid it.
func TestAnUnanchoredReceiverMatchWronglyTakesKindAlerts(t *testing.T) {
	bare := func(receiver string) v1alpha1.Trigger {
		return botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
			tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldTitleLink: receiver}
		})
	}
	withAlertmanager := func(receiver string) v1alpha1.Trigger {
		tr := productionTrigger()
		tr.Match[v1alpha1.FieldTitleLink] = receiver
		return tr
	}
	for name, mk := range map[string]func(string) v1alpha1.Trigger{"on the link alone": bare, "with the Alertmanager trigger": withAlertmanager} {
		t.Run(name, func(t *testing.T) {
			takes := func(receiver string, m chat.BotMessage) bool {
				h := newTriggered(t, mk(receiver))
				if err := h.in.Message(h.ctx, botInbound("C1", "9.9", m)); err != nil {
					t.Fatal(err)
				}
				return len(h.got()) == 1
			}
			if !takes(`receiver=slack-critical`, kindFiring) {
				t.Error("an unanchored receiver=slack-critical no longer takes a -kind alert: the guard is stale, and so may be the advice to anchor")
			}
			if takes(`receiver=slack-critical$`, kindFiring) {
				t.Error("receiver=slack-critical$ takes a -kind alert; anchored, it must not")
			}
			if !takes(`receiver=slack-critical$`, prodFiring) {
				t.Error("receiver=slack-critical$ does not take production's own alert")
			}
		})
	}
}

// A bot message no trigger takes is ignored and counted per Binding, with a DEBUG line that says where it
// was and which bot, and nothing of what it said.
func TestAnUnmatchedBotMessageIsCountedAndLoggedWithoutItsText(t *testing.T) {
	const canary = "SECRET-CANARY-TEXT"
	h := newTriggered(t, kindOnly())
	leaky := chat.BotMessage{BotID: webhookBot, Title: canary, TitleLink: canary, Fallback: canary, Footer: canary, AttachmentText: canary}
	m := botInbound("C1", "9.9", leaky)
	m.Text = canary
	if err := h.in.Message(h.ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := h.in.Message(h.ctx, botInbound("C1", "9.10", prodFiring)); err != nil {
		t.Fatal(err)
	}
	if n := h.counter(t, "agentkourier_trigger_unmatched_total", "sre/sre-alerts"); n != 2 {
		t.Errorf("agentkourier_trigger_unmatched_total = %v, want 2", n)
	}
	if n := h.counter(t, "agentkourier_trigger_matched_total", "sre/sre-alerts"); n != 0 {
		t.Errorf("agentkourier_trigger_matched_total = %v for messages that matched nothing", n)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, `"level":"DEBUG"`) || !strings.Contains(logs, webhookBot) || !strings.Contains(logs, "9.9") {
		t.Errorf("log %q; want a DEBUG line with the bot and the message", logs)
	}
	for _, text := range []string{canary, "KubeAPIErrorBudgetBurn", "receiver="} {
		if strings.Contains(logs, text) {
			t.Errorf("the log holds message text %q: %s", text, logs)
		}
	}
	if len(h.got()) != 0 || len(h.sub.submitted()) != 0 {
		t.Error("an unmatched message was handed on")
	}
}

// A message of a bot no trigger of the Binding names is the same: unmatched, and counted for that Binding
// (its trigger list can be only the mention trigger).
func TestABotMessageInAChannelWithOnlyMentionTriggersIsUnmatched(t *testing.T) {
	h := newTriggered(t) // C1 has the mention trigger alone
	if err := h.in.Message(h.ctx, botInbound("C1", "9.9", kindFiring)); err != nil {
		t.Fatal(err)
	}
	if n := h.counter(t, "agentkourier_trigger_unmatched_total", "sre/sre-alerts"); n != 1 || len(h.got()) != 0 {
		t.Errorf("unmatched = %v, matches %+v; want one unmatched message", n, h.got())
	}
}

// A channel with no Binding has no trigger, and no Binding to count against: the message is ignored, and no
// series is made for it.
func TestABotMessageInAnUnboundChannelIsIgnored(t *testing.T) {
	h := newTriggered(t, kindOnly())
	if err := h.in.Message(h.ctx, botInbound("C9", "9.9", kindFiring)); err != nil {
		t.Fatal(err)
	}
	if got := h.got(); len(got) != 0 {
		t.Fatalf("matched %+v in an unbound channel", got)
	}
	families, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "agentkourier_trigger_unmatched_total" || f.GetName() == "agentkourier_trigger_matched_total" {
			t.Errorf("a series of %s exists for a message in an unbound channel", f.GetName())
		}
	}
}

// A bot's reply inside a thread is never a trigger: a bot must not continue, or hijack, a thread of people
// or of Agent Kourier's. It is ignored and not counted, since no template is to blame.
func TestABotReplyInAThreadIsIgnored(t *testing.T) {
	h := newTriggered(t, kindOnly())
	m := botInbound("C1", "9.9", kindFiring)
	m.At.Thread = "4.4"
	if err := h.in.Message(h.ctx, m); err != nil {
		t.Fatal(err)
	}
	if len(h.got()) != 0 || len(h.sub.submitted()) != 0 {
		t.Fatalf("a bot's thread reply was handled: matches %+v, submits %+v", h.got(), h.sub.submitted())
	}
	if n := h.counter(t, "agentkourier_trigger_unmatched_total", "sre/sre-alerts"); n != 0 {
		t.Errorf("a thread reply counted as unmatched: %v", n)
	}
	// A message that is the parent of its own thread (thread equal to its ID) is top-level.
	root := botInbound("C1", "9.11", kindFiring)
	root.At.Thread = "9.11"
	if err := h.in.Message(h.ctx, root); err != nil || len(h.got()) != 1 {
		t.Fatalf("a thread parent: %v, matches %+v; want it taken", err, h.got())
	}
}

// A bot that mentions Agent Kourier starts nothing: a mention is a person's. Without a matching trigger the
// message is unmatched; with one it is taken as a bot message and no session is asked for.
func TestABotMessageNeverStartsAMentionTurn(t *testing.T) {
	for name, triggers := range map[string][]v1alpha1.Trigger{"no trigger takes it": nil, "a trigger takes it": {botTrigger(webhookBot, nil)}} {
		t.Run(name, func(t *testing.T) {
			h := newTriggered(t, triggers...)
			m := botInbound("C1", "9.9", kindFiring)
			m.Mentioned, m.Text = true, "<@UAGENTKOURIER> look at this"
			if err := h.in.Message(h.ctx, m); err != nil {
				t.Fatal(err)
			}
			if got := h.sub.submitted(); len(got) != 0 {
				t.Fatalf("a bot's mention was submitted as a turn: %+v", got)
			}
		})
	}
}

// What Slack sends twice is handled once: a redelivered bot message neither matches again nor counts again.
func TestARedeliveredBotMessageIsHandledOnce(t *testing.T) {
	h := newTriggered(t, kindOnly())
	for range 3 {
		if err := h.in.Message(h.ctx, botInbound("C1", "9.9", kindFiring)); err != nil {
			t.Fatal(err)
		}
		if err := h.in.Message(h.ctx, botInbound("C1", "9.10", heartbeat)); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.got()); n != 1 {
		t.Errorf("matched %d times for one message delivered three times", n)
	}
	if n := h.counter(t, "agentkourier_trigger_unmatched_total", "sre/sre-alerts"); n != 1 {
		t.Errorf("unmatched counted %v times for one message delivered three times", n)
	}
}

// The hook's error is the Intake's, so the platform leaves the event unacknowledged and Slack sends it again;
// the message is not remembered, so the redelivery is tried.
func TestAFailingHookLeavesTheEventToBeRedelivered(t *testing.T) {
	h := newTriggered(t, kindOnly())
	h.hookErr = errors.New("store unavailable")
	m := botInbound("C1", "9.9", kindFiring)
	if err := h.in.Message(h.ctx, m); !errors.Is(err, h.hookErr) {
		t.Fatalf("Message = %v; want the hook's error", err)
	}
	if n := h.counter(t, "agentkourier_trigger_matched_total", "sre/sre-alerts"); n != 0 {
		t.Fatalf("agentkourier_trigger_matched_total = %v after the hook failed; want 0, the match was not taken", n)
	}
	h.hookErr = nil
	if err := h.in.Message(h.ctx, m); err != nil || len(h.got()) != 2 {
		t.Fatalf("redelivery: %v, hook called %d times; want it tried again and taken", err, len(h.got()))
	}
	if n := h.counter(t, "agentkourier_trigger_matched_total", "sre/sre-alerts"); n != 1 {
		t.Fatalf("agentkourier_trigger_matched_total = %v after the redelivery; want 1", n)
	}
	if err := h.in.Message(h.ctx, m); err != nil || len(h.got()) != 2 {
		t.Fatalf("third delivery: %v, hook called %d times; want it remembered", err, len(h.got()))
	}
}

// With no start hook, a match is logged, counted and decided, and that is all.
func TestAMatchWithNoHookIsLoggedAndCounted(t *testing.T) {
	h := newTriggered(t, kindOnly())
	cfg := h.cfg
	cfg.OnStart = nil
	in := chat.NewIntake(cfg)
	if err := in.Message(h.ctx, botInbound("C1", "9.9", kindFiring)); err != nil {
		t.Fatal(err)
	}
	if n := h.counter(t, "agentkourier_trigger_matched_total", "sre/sre-alerts"); n != 1 {
		t.Errorf("agentkourier_trigger_matched_total = %v, want 1", n)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, `"level":"INFO"`) || !strings.Contains(logs, "matched a trigger") || strings.Contains(logs, "KubePodCrashLooping") {
		t.Errorf("log %q; want an INFO line that a trigger matched, with none of the message", logs)
	}
	if len(h.sub.submitted()) != 0 {
		t.Errorf("a match started a session: %+v", h.sub.submitted())
	}
}

// With no routing table yet, a bot message is ignored, as a mention is.
func TestABotMessageWithNoRoutingTableIsIgnored(t *testing.T) {
	h := newTriggered(t, kindOnly())
	cfg := h.cfg
	cfg.Snapshot = func() *config.Snapshot { return nil }
	in := chat.NewIntake(cfg)
	if err := in.Message(h.ctx, botInbound("C1", "9.9", kindFiring)); err != nil || len(h.got()) != 0 {
		t.Fatalf("Message = %v, matches %+v; want it ignored", err, h.got())
	}
	if in.AcceptsBot("C1", webhookBot) {
		t.Error("AcceptsBot with no routing table = true; want false")
	}
}

// AcceptsBot is what the adapter asks before it forwards a bot's message: only a bot that a botMessage
// trigger of the channel's Binding names.
func TestAcceptsBotOnlyForABotAnotherTriggerNamesInThatChannel(t *testing.T) {
	h := newTriggered(t, v1alpha1.Trigger{Type: v1alpha1.TriggerMention}, kindOnly())
	for _, tt := range []struct {
		name, channel, bot string
		want               bool
	}{
		{"the bot of a trigger, in the Binding's channel", "C1", webhookBot, true},
		{"another bot", "C1", "B0OTHERBOT1", false},
		{"no bot", "C1", "", false},
		{"the bot of a trigger, in a channel of another Binding", "G1", webhookBot, false},
		{"the bot of a trigger, in a channel with no Binding", "C9", webhookBot, false},
	} {
		if got := h.in.AcceptsBot(tt.channel, tt.bot); got != tt.want {
			t.Errorf("%s: AcceptsBot(%q, %q) = %v, want %v", tt.name, tt.channel, tt.bot, got, tt.want)
		}
	}
}

// Mentions are what they were. The table of spec 8.2 holds for a Binding that also has a botMessage trigger,
// and a bot message between them changes nothing.
func TestMentionsAreUnchangedByBotTriggers(t *testing.T) {
	tests := []struct {
		name   string
		m      chat.Inbound
		submit bool
		create bool
		thread string
	}{
		{"mention outside a thread starts one", inbound("C1", "5.5", "", true), true, true, "5.5"},
		{"mention in a thread, owned or not", inbound("C1", "5.5", "4.4", true), true, true, "4.4"},
		{"reply in a thread", inbound("C1", "5.5", "4.4", false), true, false, "4.4"},
		{"message outside any thread", inbound("C1", "5.5", "", false), false, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, v1alpha1.Trigger{Type: v1alpha1.TriggerMention}, kindOnly())
			if err := h.in.Message(h.ctx, botInbound("C1", "3.3", kindFiring)); err != nil {
				t.Fatal(err)
			}
			if err := h.in.Message(h.ctx, tt.m); err != nil {
				t.Fatal(err)
			}
			got := h.sub.submitted()
			if (len(got) == 1) != tt.submit || len(got) > 1 {
				t.Fatalf("submitted %+v; want a submit: %v", got, tt.submit)
			}
			if tt.submit && (got[0].Create != tt.create || got[0].Key.Thread != tt.thread || got[0].Binding != map[bool]string{true: "sre/sre-alerts", false: ""}[tt.create]) {
				t.Errorf("submitted %+v; want create=%v in thread %s", got[0], tt.create, tt.thread)
			}
		})
	}
}
