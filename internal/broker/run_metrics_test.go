package broker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// seedPost leaves one unsent post in the store, as an earlier run that died would, for the outbox's
// first reconcile to find.
func seedPost(t *testing.T, c RunConfig) {
	t.Helper()
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	post := outbox.Post{Source: "ev1", Role: store.OutboxNotice, Channel: "C0123456789", Content: slack.Content{Text: "hi"}}
	if _, err := outbox.New(outbox.Config{Store: seed, Poster: idleSlack{}}).Enqueue(t.Context(), post); err != nil {
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

	r.waitMetric(`courier_outbox_abandoned_total{reason="channel_not_found"} 1`)
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

	r.waitMetric("courier_config_reload_errors_total 1")
	r.waitMetric("courier_config_stale 1") // and the config is stale while it fails
}

// The hooks that the interactions (WP-33) call exist on Metrics with the signatures of the fields they
// fill, and each moves its counter. 35h connects them when the interactions are built.
var (
	_ = interact.Config{OnApprovalOutcome: (*Metrics)(nil).ApprovalOutcome}
	_ = interact.SweepConfig{OnTimeout: (*Metrics)(nil).InteractionTimeout}
	_ = interact.DecideConfig{OnDecision: (*Metrics)(nil).InteractionDecision}
)

// The results of a decision are a closed set, so no text from a dialect or a person can grow the series.
func TestInteractionDecisionCountsByResultAndFoldsTheRest(t *testing.T) {
	m := mustMetrics(t, nil)
	for _, r := range []string{"answered", "answered", "superseded", "late", "invalid", "a result nobody defined", ""} {
		m.InteractionDecision(r)
	}

	got := strings.Join(samples(t, m), "\n")
	for _, want := range []string{
		`courier_interaction_decisions_total{result=answered} 2`,
		`courier_interaction_decisions_total{result=superseded} 1`,
		`courier_interaction_decisions_total{result=late} 1`,
		`courier_interaction_decisions_total{result=invalid} 1`,
		`courier_interaction_decisions_total{result=other} 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the series lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "nobody defined") {
		t.Error("a result text became a label")
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
		`courier_approval_outcomes_total{binding=sre/sre-alerts,outcome=auto_denied} 2`,
		`courier_approval_outcomes_total{binding=sre/sre-alerts,outcome=ask_user_cancelled} 1`,
		`courier_approval_outcomes_total{binding=sre/sre-alerts,outcome=other} 1`, // an unknown outcome cannot grow the series
		`courier_interaction_timeouts_total{} 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the series lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ia-1") || strings.Contains(got, "nobody defined") {
		t.Error("an interaction ID or an outcome text became a label")
	}
}
