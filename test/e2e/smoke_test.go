//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/sink"
)

// TestMain clears the sink before the run, so a request left by an earlier run is never mistaken
// for this run's. It is the runner's one use of Clear (hack/kind/e2e-run.sh runs this binary in a
// Job): a scenario never clears the shared log, it matches on Alertmanager.Name.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := NewSink(NewEnv(os.Getenv).SinkURL).Clear(ctx)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: clear the sink at the start of the run:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestSmoke proves the framework reaches every double from inside the cluster: an alert goes
// through Alertmanager's routing to the sink, a fake Slack user posts to a connected client and
// reads the thread back, and the sink pod restarts through the pod-control helpers.
func TestSmoke(t *testing.T) {
	env, ctx := NewEnv(os.Getenv), t.Context()
	snk := NewSink(env.SinkURL)

	am := NewAlertmanager(env.AlertmanagerURL)
	name := am.Name("Smoke")
	if err := am.Post(ctx, Alert{Labels: map[string]string{"alertname": name}}); err != nil {
		t.Fatal(err)
	}
	if _, err := snk.WaitFor(ctx, 60*time.Second, func(r sink.Request) bool { return strings.Contains(r.Body, name) }); err != nil {
		t.Fatalf("the alert never reached the sink through Alertmanager: %v", err)
	}

	sl := NewSlack(env.SlackURL)
	bot := slack.New("xoxb-fake-test-token", slack.OptionAPIURL(env.SlackURL+"/"), slack.OptionAppLevelToken("xapp-fake-test-token"))
	connectSocket(t, bot, true)
	mention, err := sl.Mention(ctx, "C1", "USMOKE", "<@UFAKEBOT> smoke "+name)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range mention.Envelopes {
		if _, err := sl.AckWait(ctx, 10*time.Second, id); err != nil {
			t.Fatalf("the client never acked the mention's event: %v", err)
		}
	}
	thread, err := sl.Thread(ctx, mention.Channel, mention.TS)
	if err != nil || len(thread) != 1 || !strings.Contains(thread[0].Text, name) {
		t.Fatalf("Thread = %+v, %v; want the mention read back", thread, err)
	}

	pods, err := kube.NewInCluster()
	if err != nil {
		t.Fatal(err)
	}
	restartCtx, cancel := context.WithTimeout(ctx, 2*time.Minute) // a stuck replacement fails here, with its reason
	defer cancel()
	if _, err := pods.Restart(restartCtx, kube.Namespace, "app=sink"); err != nil {
		t.Fatal(err)
	}
	// The new sink answers and no longer holds this run's alert. It is not empty in general: an
	// earlier run's alert resolves at Alertmanager's resolve timeout and is sent to whichever sink
	// pod is up, so the log is matched by this run's name, as a scenario's would be.
	err = Wait(ctx, 30*time.Second, "the restarted sink to answer without this run's alert", func(ctx context.Context) (bool, error) {
		reqs, err := snk.Requests(ctx)
		return !slices.ContainsFunc(reqs, func(r sink.Request) bool { return strings.Contains(r.Body, name) }), err
	})
	if err != nil {
		t.Fatal(err)
	}
}
