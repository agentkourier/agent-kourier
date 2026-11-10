package broker

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// noOwn makes the broker of w one with no ChatConnection of its own: it serves the connections of the namespaces it admits (team-a and team-b)
// and the ChatConnection of the example config, in a namespace it does not admit, is none of its business. RunConfig still requires the own
// reference, so the seam lets the test leave it out.
func (w *connWorld) noOwn() {
	w.c.ChatConnection, w.c.allowNoOwn = v1alpha1.ObjectRef{}, true
}

// teamOnlyWorld is a broker with no own connection that serves the two team connections team-a and team-b, each with a Binding that the
// verification probes quickly. idErr is what asking whose bot each token is answers with, by token.
func teamOnlyWorld(t *testing.T, idErr map[string]error) (*connWorld, *statusCluster, *running) {
	t.Helper()
	w := newConnWorld(t, nil)
	w.noOwn()
	cluster := newStatusClusterOn(w.dyn)
	w.c.StatusClient = cluster.dyn
	w.c.probeEvery, w.c.probeTick, w.c.probeGap = 50*time.Millisecond, 10*time.Millisecond, time.Millisecond
	for tok, err := range idErr {
		w.idErr[tok] = err
	}
	w.team("team-a", "tok-a")
	w.binding("team-a", "on-a", "C0TEAMA0001")
	w.team("team-b", "tok-b")
	w.binding("team-b", "on-b", "C0TEAMB0001")
	return w, cluster, start(t, w.c)
}

// A broker with no own connection serves the team connections of the namespaces it admits, is ready, runs no connection of the example config's
// namespace, and stops cleanly: the leader half ends with the shutdown, with no connection of its own to end it.
func TestARunWithNoOwnConnectionServesTheTeamConnections(t *testing.T) {
	w, cluster, r := teamOnlyWorld(t, nil)
	r.until("/readyz", http.StatusOK)

	w.eventually("both team connections to run", func() bool { return w.up("tok-a") == 1 && w.up("tok-b") == 1 })
	r.waitMetric(connected("team-a/slack", 1))
	r.waitMetric(connected("team-b/slack", 1))
	for _, b := range []string{"team-a/on-a", "team-b/on-b"} {
		w.eventually("the Binding "+b+" to be joined", func() bool { return cluster.stored(t, b)["ChannelJoined"].Status == "True" })
	}
	if w.up(ownToken) != 0 {
		t.Errorf("the ChatConnection of a namespace the broker does not admit runs %d times; want it not to", w.up(ownToken))
	}
	_, body := r.get("/metrics")
	if strings.Contains(body, `connection="agent-kourier-system/slack-main"`) {
		t.Errorf("/metrics has a series for a connection that is not served:\n%s", grep(body, "slack-main"))
	}
	select {
	case err := <-r.done:
		t.Fatalf("Run returned %v; want the leader half to run until the shutdown", err)
	default:
	}

	r.cancel()
	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after a signal; want nil", err)
	}
}

// With no own connection and every token rejected the broker stays up and Ready: no chat failure ends the process. Each connection says 0 on
// agentkourier_chat_connected and its Bindings say ChatCredentialsRejected. This is the case where Slack rejects the token as the connection is
// started, and the one where the token is revoked while it runs.
func TestARunWithNoOwnConnectionAndEveryTokenRejectedStaysUpAndReady(t *testing.T) {
	rejected := &credentialsRejectedError{method: "auth.test", code: "invalid_auth"}
	for name, tc := range map[string]struct {
		idErr  map[string]error
		revoke bool
	}{
		"at the start":   {idErr: map[string]error{"tok-a": rejected, "tok-b": rejected}},
		"while it is up": {revoke: true},
	} {
		t.Run(name, func(t *testing.T) {
			w, cluster, r := teamOnlyWorld(t, tc.idErr)
			r.until("/readyz", http.StatusOK)
			if tc.revoke {
				w.eventually("both team connections to run", func() bool { return w.up("tok-a") == 1 && w.up("tok-b") == 1 })
				w.killRun("tok-a", slackErr("invalid_auth"))
				w.killRun("tok-b", slackErr("invalid_auth"))
			}

			r.waitMetric(connected("team-a/slack", 0))
			r.waitMetric(connected("team-b/slack", 0))
			for _, b := range []string{"team-a/on-a", "team-b/on-b"} {
				w.eventually("the Binding "+b+" to say its credentials were rejected", func() bool {
					c := cluster.stored(t, b)["ChannelJoined"]
					return c.Status == "False" && c.Reason == "ChatCredentialsRejected"
				})
			}
			// Settled: the pass that wrote it has run, so nothing that was going to end the process has been let go of since.
			time.Sleep(200 * time.Millisecond)
			select {
			case err := <-r.done:
				t.Fatalf("Run returned %v with every token rejected; want the broker to stay up", err)
			default:
			}
			if code, _ := r.get("/readyz"); code != http.StatusOK {
				t.Errorf("/readyz = %d with every token rejected; want 200: readiness does not depend on a connection", code)
			}
			if w.up("tok-a") != 0 || w.up("tok-b") != 0 {
				t.Errorf("up: team-a %d, team-b %d; want no connection to run", w.up("tok-a"), w.up("tok-b"))
			}
			r.cancel()
			if err := r.wait(); err != nil {
				t.Errorf("Run = %v after a signal; want nil", err)
			}
		})
	}
}

// With no own connection to give them to, the outbox rows that have no connection stop the leader's start with a wiring error, which exits 2,
// and say what to do. The row is not read or changed.
func TestARunWithNoOwnConnectionAndRowsWithNoConnectionRefusesToLead(t *testing.T) {
	w := newConnWorld(t, nil)
	w.noOwn()
	w.team("team-a", "tok-a")
	seed, err := sqlite.Open(t.Context(), w.c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateOutbox(t.Context(), store.OutboxEntry{ID: "old/reply", EventID: "old", Role: store.OutboxReply, Channel: "C0123456789", Thread: "1.1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	err = start(t, w.c).wait()

	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "1 outbox rows") || !strings.Contains(err.Error(), "config.chatConnection") {
		t.Fatalf("Run = %v; want ErrWiring that counts the row and says to set config.chatConnection", err)
	}
	if w.up("tok-a") != 0 {
		t.Error("a team connection ran though the leader half did not start")
	}
	st, err := sqlite.Open(t.Context(), w.c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if rows, err := st.UnattributedRows(t.Context()); err != nil || rows.Outbox != 1 {
		t.Errorf("UnattributedRows = %+v, %v; want the row left as it was", rows, err)
	}
}
