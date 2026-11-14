package broker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/test/stubagent"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// killChan is the channel that makes the Run of token's client return an error. The caller holds w.mu.
func (w *connWorld) killChan(token string) chan error {
	if w.kill[token] == nil {
		w.kill[token] = make(chan error, 1)
	}
	return w.kill[token]
}

// killRun makes the Run of token's client return err, as Socket Mode does when the platform drops the connection for good.
func (w *connWorld) killRun(token string, err error) {
	w.mu.Lock()
	ch := w.killChan(token)
	w.mu.Unlock()
	ch <- err
}

// fail makes the call op ("Post", "Update", "StartStream") of token's client answer with err from now on.
func (w *connWorld) fail(token, op string, err error) {
	w.mu.Lock()
	w.faults[token+"/"+op] = err
	w.mu.Unlock()
}

func (w *connWorld) faultOf(token, op string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.faults[token+"/"+op]
}

func (l *liveSlack) Post(ctx context.Context, channel, thread string, c slack.Content, id string) (string, error) {
	if err := l.w.faultOf(l.token, "Post"); err != nil {
		return "", err
	}
	return l.idleSlack.Post(ctx, channel, thread, c, id)
}

func (l *liveSlack) Update(ctx context.Context, channel, ts string, c slack.Content) error {
	if err := l.w.faultOf(l.token, "Update"); err != nil {
		return err
	}
	return l.idleSlack.Update(ctx, channel, ts, c)
}

func (l *liveSlack) StartStream(ctx context.Context, channel, thread, to, markdown string, mode slack.StreamMode) (string, error) {
	if err := l.w.faultOf(l.token, "StartStream"); err != nil {
		return "", err
	}
	return l.idleSlack.StartStream(ctx, channel, thread, to, markdown, mode)
}

// connected is the line of /metrics that says whether the connection is up.
func connected(conn string, up int) string {
	return `agentkourier_chat_connected{connection="` + conn + `"} ` + string(rune('0'+up))
}

// waitNoMetric polls /metrics until no line starts with prefix, or fails after 40 s.
func (r *running) waitNoMetric(prefix string) {
	r.t.Helper()
	var body string
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, body = r.get("/metrics")
		if !slices.ContainsFunc(strings.Split(body, "\n"), func(l string) bool { return strings.HasPrefix(l, prefix) }) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("/metrics still holds a line that starts with %q:\n%s", prefix, grep(body, "agentkourier_chat_"))
		}
	}
}

// isolationWorld is a broker that serves the own connection and two team connections, each with a Binding that the verification probes quickly.
func isolationWorld(t *testing.T) (*connWorld, *statusCluster, *running) {
	t.Helper()
	w := newConnWorld(t, nil)
	runsAlongside(&w.c)
	cluster := newStatusClusterOn(w.dyn)
	w.c.StatusClient = cluster.dyn
	w.c.probeEvery, w.c.probeTick, w.c.probeGap = 50*time.Millisecond, 10*time.Millisecond, time.Millisecond
	w.team("team-a", "tok-a")
	w.binding("team-a", "on-a", "C0TEAMA0001")
	w.team("team-b", "tok-b")
	w.binding("team-b", "on-b", "C0TEAMB0001")
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	w.eventually("the three connections to run", func() bool { return w.up(ownToken) == 1 && w.up("tok-a") == 1 && w.up("tok-b") == 1 })
	for _, b := range []string{"team-a/on-a", "team-b/on-b"} {
		w.eventually("the Binding "+b+" to be joined", func() bool { return cluster.stored(t, b)["ChannelJoined"].Status == "True" })
	}
	return w, cluster, r
}

// requireIsolated checks what a team connection's rejection leaves alone: the broker is still running, the own connection and team-b are up and
// their Bindings joined, and team-a's metric says 0 while theirs say 1.
func requireIsolated(t *testing.T, w *connWorld, cluster *statusCluster, r *running) {
	t.Helper()
	r.waitMetric(connected("team-a/slack", 0))
	r.waitMetric(connected("team-b/slack", 1))
	r.waitMetric(connected("agent-kourier-system/slack-main", 1))
	select {
	case err := <-r.done:
		r.done <- err
		t.Fatalf("Run returned %v after a team connection was rejected; want the broker to go on\n%s", err, w.logs.String())
	default:
	}
	if w.up("tok-a") != 0 || w.up("tok-b") != 1 || w.up(ownToken) != 1 {
		t.Errorf("up: team-a %d, team-b %d, own %d; want only team-a down", w.up("tok-a"), w.up("tok-b"), w.up(ownToken))
	}
	if c := cluster.stored(t, "team-b/on-b")["ChannelJoined"]; c.Status != "True" {
		t.Errorf("team-b's Binding: ChannelJoined = %s/%s; want True, its connection was not touched", c.Status, c.Reason)
	}
	logged := w.logs.String()
	for _, want := range []string{"connection=team-a/slack", "code=invalid_auth"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log lacks %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "tok-a") {
		t.Errorf("the log holds a token:\n%s", logged)
	}
}

// requireTold checks that team-a's Binding is told the credentials were rejected, makes no call of Slack for it from then on, and that a changed
// Secret starts the connection again and clears it.
func requireTold(t *testing.T, w *connWorld, cluster *statusCluster, r *running) {
	t.Helper()
	w.eventually("team-a's Binding to be told the credentials were rejected", func() bool {
		c := cluster.stored(t, "team-a/on-a")["ChannelJoined"]
		return c.Status == "False" && c.Reason == "ChatCredentialsRejected"
	})
	asked, others := len(w.probedBy("tok-a")), len(w.probedBy("tok-b"))
	// Probe passes go on for the connection that runs: wait for several of them, and the rejected one must have had its chance to be asked.
	w.eventually("more probe passes", func() bool { return len(w.probedBy("tok-b")) >= others+5 })
	if n := len(w.probedBy("tok-a")); n != asked {
		t.Errorf("team-a's bot was asked %d more times after its Binding was told the credentials were rejected; want no call", n-asked)
	}

	w.secret("team-a", "tok-a2")
	w.eventually("team-a to run again on its new token", func() bool { return w.up("tok-a2") == 1 })
	r.waitMetric(connected("team-a/slack", 1))
	w.eventually("team-a's Binding to be joined again", func() bool {
		c := cluster.stored(t, "team-a/on-a")["ChannelJoined"]
		return c.Status == "True" && slices.Contains(w.probedBy("tok-a2"), "C0TEAMA0001")
	})
	if w.up("tok-b") != 1 || w.up(ownToken) != 1 {
		t.Errorf("up: team-b %d, own %d; want them running", w.up("tok-b"), w.up(ownToken))
	}
}

// A team connection whose Slack drops the connection for good (the token is rejected) stops alone: the broker does not exit, the own connection
// and the other team connection serve on, the metric of the stopped one says 0, one WARN names it and the code, its Binding is told the
// credentials were rejected and Slack is not asked for it again, and a new Secret starts it again.
func TestATeamConnectionTheSlackRunRejectsStopsThatConnectionAlone(t *testing.T) {
	t.Parallel()
	w, cluster, r := isolationWorld(t)
	w.killRun("tok-a", slackErr("invalid_auth"))
	requireIsolated(t, w, cluster, r)
	requireTold(t, w, cluster, r)
}

// The same when it is a post that finds the token rejected, which is what a token revoked while the connection is up looks like: the outbox's
// post through the table's platform ends that connection's run, and the process goes on.
func TestATeamConnectionWhosePostSlackRejectsStopsThatConnectionAlone(t *testing.T) {
	t.Parallel()
	w, cluster, r := isolationWorld(t)
	w.fail("tok-a", "Post", slackErr("invalid_auth"))
	_, _ = w.ob.Send(t.Context(), outbox.Post{Source: "x", Role: store.OutboxReply, Connection: "team-a/slack", Channel: "C0TEAMA0001", Thread: "1.1", Out: chat.Say(chat.Message{Fallback: "hi"})})
	requireIsolated(t, w, cluster, r)
	requireTold(t, w, cluster, r)
}

// The own connection's rejection is still the broker's: Run returns ErrSlackAuth, which cmd/agent-kourier exits 1 on, whatever the team
// connections are doing.
func TestTheOwnConnectionsRejectionStillEndsTheBrokerWithTeamConnections(t *testing.T) {
	t.Parallel()
	w, _, r := isolationWorld(t)
	w.killRun(ownToken, slackErr("invalid_auth"))
	err := r.waitWithin(30 * time.Second)
	if !isSlackAuth(err) || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("Run = %v; want ErrSlackAuth naming invalid_auth", err)
	}
}

// The turns a team connection left while it did not run are taken up when it does, and not before: its conversation's recorded task is asked
// after at the agent only once the connection runs (the others' were taken up with Recover at the start).
func TestTheTurnsOfATeamConnectionAreTakenUpWhenItRuns(t *testing.T) {
	t.Parallel()
	stub := stubagent.NewHandler()
	t.Cleanup(stub.Close)
	stub.Agent("kagent", "k8s-agent")
	log := &rpcLog{next: stub}
	srv := httptest.NewServer(log)
	t.Cleanup(srv.Close)

	w := newConnWorld(t, func(objs []*unstructured.Unstructured) {
		_ = unstructured.SetNestedField(objs[0].Object, srv.URL, "spec", "url")
	})
	runsAlongside(&w.c)
	hold := make(chan struct{})
	w.idHold["tok-a"] = hold // the bot of team-a is not known: the connection does not start
	w.team("team-a", "tok-a")
	w.binding("team-a", "on-a", "C0TEAMA0001")
	seed, err := sqlite.Open(t.Context(), w.c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	key := store.ConversationKey{Connection: "team-a/slack", Channel: "C0TEAMA0001", Thread: "1.1"}
	if err := seed.UpsertSession(t.Context(), store.Session{Key: key, Binding: "team-a/on-a", ContextID: "ctx-1", TaskID: "task-1", LastActivity: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	w.eventually("the own connection to run", func() bool { return w.up(ownToken) == 1 })

	// A negative: nothing the test can wait on says "the agent was not asked". Recover's goroutines ask within milliseconds of the start (the
	// control for it is the files-route test above), so this wait is long for them; the supervisor cannot be waited for, since it is held in
	// this connection's identity call, which is the point.
	time.Sleep(500 * time.Millisecond)
	if log.saw("GetTask") {
		t.Fatal("the agent was asked about the task of a connection that does not run")
	}
	close(hold)
	w.eventually("team-a to run", func() bool { return w.up("tok-a") == 1 })
	w.eventually("the agent to be asked about the recorded task", func() bool { return log.saw("GetTask") })
}

func isSlackAuth(err error) bool { return errors.Is(err, ErrSlackAuth) }
