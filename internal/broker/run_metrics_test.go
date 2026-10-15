package broker

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// enqueuer is an Outbox over st that only enqueues, for the tests that seed a store before Run opens it.
func enqueuer(t testing.TB, st store.Outbox) *outbox.Outbox {
	t.Helper()
	o, err := outbox.New(outbox.Config{Store: st, Platform: slack.NewPlatform(idleSlack{}, "ns/conn"), OnAbandon: outbox.NoAbandon})
	if err != nil {
		t.Fatalf("outbox.New = %v", err)
	}
	return o
}

// seedPost leaves one unsent post in the store, as an earlier run that died would, for the outbox's
// first reconcile to find.
func seedPost(t *testing.T, c RunConfig) {
	t.Helper()
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	post := outbox.Post{Source: "ev1", Role: store.OutboxNotice, Channel: "C0123456789", Out: chat.Say(chat.Message{Fallback: "hi"})}
	if _, err := enqueuer(t, seed).Enqueue(t.Context(), post); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
}

// seedReaction leaves one unmade reaction in the store, as seedPost leaves a post.
func seedReaction(t *testing.T, c RunConfig) {
	t.Helper()
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	react := outbox.Reaction{Source: "ev1/queued", Channel: "C0123456789", TS: "1.1", Name: "hourglass_flowing_sand"}
	if _, err := enqueuer(t, seed).EnqueueReaction(t.Context(), react); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
}

// waitMetric polls /metrics until it holds line, or fails after 10 s.
func (r *running) waitMetric(line string) {
	r.t.Helper()
	var body string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, body = r.get("/metrics")
		if strings.Contains(body, line+"\n") {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("/metrics never held %q:\n%s", line, body)
		}
	}
}

// channelGone is a Slack whose posts fail the way a post to a deleted channel does, which is for
// good: the outbox gives the post up and says why.
type channelGone struct{ idleSlack }

func (channelGone) Post(context.Context, string, string, slack.Content, string) (string, error) {
	return "", slackErr("channel_not_found")
}

func (channelGone) Find(context.Context, string, string, string, time.Time) (string, bool, error) {
	return "", false, nil
}

func TestRunCountsAnAbandonedPost(t *testing.T) {
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return channelGone{} }
	seedPost(t, c)
	r := start(t, c)

	r.waitMetric(`agentkourier_outbox_abandoned_total{reason="channel_gone"} 1`)
}

func TestRunCountsAFailedConfigReload(t *testing.T) {
	c := startConfig(t)
	c.reloadPoll = 5 * time.Millisecond
	r := start(t, c)
	r.until("/readyz", 200)

	staging := filepath.Join(c.ConfigDir, ".bad.yaml.tmp")
	if err := os.WriteFile(staging, []byte("kind: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(c.ConfigDir, "bad.yaml")); err != nil {
		t.Fatal(err)
	}

	r.waitMetric("agentkourier_config_reload_errors_total 1")
	r.waitMetric("agentkourier_config_stale 1") // and the config is stale while it fails
}

// The hooks that the interactions (WP-33) call exist on Metrics with the signatures of the fields they
// fill, and each moves its counter. 35h connects them when the interactions are built.
var (
	_ = interact.Config{OnApprovalOutcome: (*Metrics)(nil).ApprovalOutcome}
	_ = interact.SweepConfig{OnTimeout: (*Metrics)(nil).InteractionTimeout, OnApprovalOutcome: (*Metrics)(nil).ApprovalOutcome}
	_ = interact.DecideConfig{OnDecision: (*Metrics)(nil).InteractionDecision, OnApprovalOutcome: (*Metrics)(nil).ApprovalOutcome}
)

// The results of a decision are a closed set, so no text from a dialect or a person can grow the series.
func TestInteractionDecisionCountsByResultAndFoldsTheRest(t *testing.T) {
	m := mustMetrics(t, nil)
	for _, r := range []string{"answered", "answered", "superseded", "late", "invalid", "unrecorded", "a result nobody defined", ""} {
		m.InteractionDecision(r)
	}

	got := strings.Join(samples(t, m), "\n")
	for _, want := range []string{
		`agentkourier_interaction_decisions_total{result=answered} 2`,
		`agentkourier_interaction_decisions_total{result=superseded} 1`,
		`agentkourier_interaction_decisions_total{result=late} 1`,
		`agentkourier_interaction_decisions_total{result=invalid} 1`,
		`agentkourier_interaction_decisions_total{result=unrecorded} 1`,
		`agentkourier_interaction_decisions_total{result=other} 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the series lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "nobody defined") {
		t.Error("a result text became a label")
	}
}

// The outcomes of a decided approval, and a timed-out one, are in the closed set: each is a series of its own, per
// Binding, and not "other". The set is closed, so a new constant that is not listed here and in ApprovalOutcome folds into "other".
func TestApprovalOutcomesOfADecidedApprovalAreSeriesOfTheirOwn(t *testing.T) {
	m := mustMetrics(t, nil)
	for _, o := range []interact.Outcome{interact.OutcomeApproved, interact.OutcomeApproved, interact.OutcomeDenied, interact.OutcomePartiallyApproved, interact.OutcomeTimedOut} {
		m.ApprovalOutcome("sre/sre-alerts", o)
	}
	m.ApprovalOutcome("sre/other", interact.OutcomeApproved)
	got := strings.Join(samples(t, m), "\n")
	for _, want := range []string{
		`agentkourier_approval_outcomes_total{binding=sre/sre-alerts,outcome=approved} 2`,
		`agentkourier_approval_outcomes_total{binding=sre/sre-alerts,outcome=denied} 1`,
		`agentkourier_approval_outcomes_total{binding=sre/sre-alerts,outcome=partially_approved} 1`,
		`agentkourier_approval_outcomes_total{binding=sre/sre-alerts,outcome=timed_out} 1`,
		`agentkourier_approval_outcomes_total{binding=sre/other,outcome=approved} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the series lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "outcome=other") {
		t.Errorf("a defined outcome was folded into other:\n%s", got)
	}
}

func TestApprovalOutcomeAndTimeoutHooksMoveTheirCounters(t *testing.T) {
	m := mustMetrics(t, nil)
	m.ApprovalOutcome("sre/sre-alerts", interact.OutcomeAutoDenied)
	m.ApprovalOutcome("sre/sre-alerts", interact.OutcomeAutoDenied)
	m.ApprovalOutcome("sre/sre-alerts", interact.OutcomeAskUserCancelled)
	m.ApprovalOutcome("sre/sre-alerts", interact.Outcome("an outcome nobody defined"))
	m.InteractionTimeout("ia-1")
	m.InteractionTimeout("ia-2")

	got := strings.Join(samples(t, m), "\n")
	for _, want := range []string{
		`agentkourier_approval_outcomes_total{binding=sre/sre-alerts,outcome=auto_denied} 2`,
		`agentkourier_approval_outcomes_total{binding=sre/sre-alerts,outcome=ask_user_cancelled} 1`,
		`agentkourier_approval_outcomes_total{binding=sre/sre-alerts,outcome=other} 1`, // an unknown outcome cannot grow the series
		`agentkourier_interaction_timeouts_total{} 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the series lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ia-1") || strings.Contains(got, "nobody defined") {
		t.Error("an interaction ID or an outcome text became a label")
	}
}

// upSlack is idleSlack that tells its config's OnConnection what the Slack client would: the connection is up, and
// then, when it is told to, down.
type upSlack struct {
	idleSlack
	onConn func(bool)
	drop   chan struct{}
}

func (s upSlack) Run(ctx context.Context, _ slack.Sink) error {
	s.onConn(true)
	select {
	case <-s.drop:
		s.onConn(false)
	case <-ctx.Done():
	}
	<-ctx.Done()
	return ctx.Err()
}

// agentkourier_chat_connected is 0 from the moment the connection is chosen, since a connection that never came up must
// say so, and then follows the adapter: 1 when it is up, 0 when it drops. The label is the ChatConnection's name.
func TestRunShowsWhetherTheChatConnectionIsUp(t *testing.T) {
	const name = "agent-kourier-system/slack-main"
	up, down := `agentkourier_chat_connected{connection="`+name+`"} 1`, `agentkourier_chat_connected{connection="`+name+`"} 0`
	t.Run("never connected", func(t *testing.T) {
		r := start(t, startConfig(t)) // idleSlack calls nothing
		r.until("/readyz", http.StatusOK)

		r.waitMetric(down)
	})
	t.Run("connects and drops", func(t *testing.T) {
		c := startConfig(t)
		drop := make(chan struct{})
		c.newSlack = func(sc slack.Config) slackAPI { return upSlack{onConn: sc.OnConnection, drop: drop} }
		r := start(t, c)

		r.waitMetric(up)
		close(drop)
		r.waitMetric(down)
	})
}

// Slack being down does not make Agent Kourier unready: /readyz holds what Agent Kourier needs to serve, and the gauge says the rest.
func TestReadyzDoesNotWaitForTheChatConnection(t *testing.T) {
	r := start(t, startConfig(t)) // idleSlack never connects

	r.until("/readyz", http.StatusOK)

	r.waitMetric(`agentkourier_chat_connected{connection="agent-kourier-system/slack-main"} 0`)
}
