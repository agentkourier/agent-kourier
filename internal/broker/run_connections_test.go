package broker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// ownToken is the bot token of the own connection's Secret in the test cluster.
const ownToken = "fake-value"

// connWorld is a broker on the custom resources that serves the own connection and the team connections of the namespaces team-a and team-b,
// whose Slack clients are doubles: each holds its connection until its context ends, and the world sees which are up.
type connWorld struct {
	t    *testing.T
	c    RunConfig
	dyn  *dynamicfake.FakeDynamicClient
	kube *fake.Clientset

	mu     sync.Mutex
	live   map[string]int         // the connections up, by bot token
	probed map[string][]string    // the channels each bot token was asked for its access to, by bot token
	sinks  map[string]slack.Sink  // what each token's Run was given last
	events []string               // runs that ended, shutdown steps and transports that closed, in order
	ids    map[string]appIdentity // by bot token; a token with none is its own app
	ob     *outbox.Outbox
	logs   syncBuffer

	// kill is, by bot token, the channel a test sends to to make that token's Run return the error (a connection the platform drops for good).
	kill map[string]chan error
	// faults is the error each call of a token's client answers with, by token and call ("Post", "StartStream", "Update").
	faults map[string]error
	// idErr is the error asking whose bot a token is answers with, by token; idHold holds that call back until its channel is closed.
	idErr  map[string]error
	idHold map[string]chan struct{}
}

// syncBuffer is a buffer a logger writes to while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newConnWorld(t *testing.T, edit func([]*unstructured.Unstructured)) *connWorld {
	t.Helper()
	w := &connWorld{t: t, live: map[string]int{}, probed: map[string][]string{}, sinks: map[string]slack.Sink{}, ids: map[string]appIdentity{},
		kill: map[string]chan error{}, faults: map[string]error{}, idErr: map[string]error{}, idHold: map[string]chan struct{}{}}
	w.c = crStartConfig(t, func(objs []*unstructured.Unstructured) {
		_ = unstructured.SetNestedStringSlice(objs[0].Object, []string{"sre", "payments", "team-a", "team-b"}, "spec", "allowedNamespaces")
		if edit != nil {
			edit(objs)
		}
	})
	w.dyn, w.kube = w.c.Dynamic.(*dynamicfake.FakeDynamicClient), w.c.Client.(*fake.Clientset)
	w.ids[ownToken] = appIdentity{App: "AOWN", Team: "T1", User: "UOWN", Bot: "BOWN"}
	w.c.ConnectionNamespaces = []string{"team-a", "team-b"}
	w.c.Logger = slog.New(slog.NewTextHandler(&w.logs, nil))
	w.c.superviseEvery, w.c.reloadPoll = 10*time.Millisecond, 20*time.Millisecond
	w.c.newSlack = func(sc slack.Config) slackAPI {
		return &liveSlack{w: w, token: sc.BotToken.Reveal(), onConn: sc.OnConnection}
	}
	w.c.identify = func(ctx context.Context, c Connection) (appIdentity, error) {
		tok := c.Credentials["botToken"].Reveal()
		w.mu.Lock()
		hold := w.idHold[tok]
		w.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return appIdentity{}, ctx.Err()
			}
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if err := w.idErr[tok]; err != nil {
			return appIdentity{}, err
		}
		if id, ok := w.ids[tok]; ok {
			return id, nil
		}
		return appIdentity{App: "A" + tok, Team: "T1", User: "U" + tok, Bot: "B" + tok}, nil
	}
	w.c.transportClosed = func(conn string) { w.event("closed:" + conn) }
	w.c.shutdownTrace = func(step string) { w.event("step:" + step) }
	w.c.outboxBuilt = func(ob *outbox.Outbox) { w.mu.Lock(); w.ob = ob; w.mu.Unlock() }
	return w
}

func (w *connWorld) event(e string) { w.mu.Lock(); w.events = append(w.events, e); w.mu.Unlock() }

func (w *connWorld) log() []string { w.mu.Lock(); defer w.mu.Unlock(); return slices.Clone(w.events) }

func (w *connWorld) up(token string) int { w.mu.Lock(); defer w.mu.Unlock(); return w.live[token] }

func (w *connWorld) sink(token string) slack.Sink {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sinks[token]
}

func (w *connWorld) eventually(what string, ok func() bool) {
	w.t.Helper()
	for deadline := time.Now().Add(40 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// liveSlack is a Slack client that is up from Run to the end of its context.
type liveSlack struct {
	idleSlack
	w     *connWorld
	token string
	// onConn is the adapter's OnConnection, which the client tells when its connection is up and when it is not.
	onConn func(bool)
}

func (l *liveSlack) Run(ctx context.Context, sink slack.Sink) error {
	l.w.mu.Lock()
	l.w.live[l.token]++
	l.w.sinks[l.token] = sink
	kill := l.w.killChan(l.token)
	l.w.mu.Unlock()
	if l.onConn != nil {
		l.onConn(true)
		defer l.onConn(false)
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-kill:
	}
	l.w.mu.Lock()
	l.w.live[l.token]--
	l.w.events = append(l.w.events, "run-ended:"+l.token)
	l.w.mu.Unlock()
	return err
}

// ChannelAccess records who was asked for which channel: the verification of a Binding reads its channel as the bot of its connection.
func (l *liveSlack) ChannelAccess(_ context.Context, channel string) (chat.Access, error) {
	l.w.mu.Lock()
	l.w.probed[l.token] = append(l.w.probed[l.token], channel)
	l.w.mu.Unlock()
	return chat.Access{Member: true}, nil
}

func (w *connWorld) probedBy(token string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.probed[token])
}

// secret puts the credentials Secret of a team connection in the cluster, or changes it.
func (w *connWorld) secret(ns, bot string) { w.t.Helper(); w.secretOf(ns, bot, "app-"+bot) }

// secretOf is secret for a Secret whose app token is app.
func (w *connWorld) secretOf(ns, bot, app string) {
	w.t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "creds"}, Data: map[string][]byte{"botToken": []byte(bot), "appToken": []byte(app)}}
	_, err := w.kube.CoreV1().Secrets(ns).Update(w.t.Context(), s, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		_, err = w.kube.CoreV1().Secrets(ns).Create(w.t.Context(), s, metav1.CreateOptions{})
	}
	if err != nil {
		w.t.Fatal(err)
	}
}

// connection puts the ChatConnection ns/slack, whose Secret is creds, in the cluster.
func (w *connWorld) connection(ns string) {
	w.t.Helper()
	conn := crObject(w.t, "chatconnection.yaml")
	conn.SetNamespace(ns)
	conn.SetName("slack")
	conn.SetUID(types.UID("uid-" + ns))
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "credentialsSecretRef", "name")
	if err := w.dyn.Tracker().Create(crConnections, conn, ns); err != nil {
		w.t.Fatal(err)
	}
}

// team puts a team connection with its Secret, whose bot token is bot.
func (w *connWorld) team(ns, bot string) { w.t.Helper(); w.secret(ns, bot); w.connection(ns) }

// binding puts a Binding ns/name on the ChatConnection ns/slack, in channel.
func (w *connWorld) binding(ns, name, channel string) {
	w.t.Helper()
	b := crObject(w.t, "binding-minimal.yaml")
	b.SetNamespace(ns)
	b.SetName(name)
	b.SetUID(types.UID("uid-" + ns + "-" + name))
	for field, value := range map[string]string{"namespace": ns, "name": "slack"} {
		_ = unstructured.SetNestedField(b.Object, value, "spec", "chat", "connectionRef", field)
	}
	_ = unstructured.SetNestedField(b.Object, channel, "spec", "chat", "channel")
	if err := w.dyn.Tracker().Create(crBindings, b, ns); err != nil {
		w.t.Fatal(err)
	}
}

func (w *connWorld) lanes() int { w.mu.Lock(); defer w.mu.Unlock(); return w.ob.Lanes() }

// A takeover with the Secret of a team connection not synced leads and serves the own connection, and does not exit: the team connection
// starts once its Secret is there.
func TestARunWhoseTeamSecretIsNotThereServesTheOwnConnectionAndStartsTheTeamOnceItIs(t *testing.T) {
	w := newConnWorld(t, nil)
	w.connection("team-a") // its Secret is not in the cluster
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	w.eventually("the own connection to run", func() bool { return w.up(ownToken) == 1 })
	w.eventually("the supervisor to have seen the connection whose Secret is missing", func() bool {
		return strings.Contains(w.logs.String(), "is not started yet")
	})
	select {
	case err := <-r.done:
		t.Fatalf("Run returned %v with a team Secret missing; want it to lead", err)
	default:
	}

	w.secret("team-a", "tok-a")
	w.eventually("the team connection to run once its Secret is there", func() bool { return w.up("tok-a") == 1 })
	if w.up(ownToken) != 1 {
		t.Errorf("the own connection is up %d times; want it to run on", w.up(ownToken))
	}
}

// A team connection is added, changed (its Secret) and removed on a running broker. Each has its own run; the one that is replaced or removed
// has no run left, its outbox lane is gone, and the others are not touched.
func TestARunAddsChangesAndRemovesATeamConnection(t *testing.T) {
	w := newConnWorld(t, nil)
	w.c.ConnectionNamespaces = append(w.c.ConnectionNamespaces, "team-c")
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	w.eventually("the own connection to run", func() bool { return w.up(ownToken) == 1 && w.ob != nil })
	lanes := w.lanes()

	w.team("team-a", "tok-a1")
	w.team("team-b", "tok-b")
	w.team("team-c", "tok-c")
	w.eventually("the team connections to run", func() bool { return w.up("tok-a1") == 1 && w.up("tok-b") == 1 && w.up("tok-c") == 1 })
	// The outbox gauges name each connection the broker serves, the ones the supervisor started included (they say 0 with nothing unposted).
	for _, conn := range []string{"agent-kourier-system/slack-main", "team-b/slack", "team-c/slack"} {
		r.waitMetric(`agentkourier_outbox_pending{connection="` + conn + `"} 0`)
	}
	r.waitMetric(`agentkourier_chat_connected{connection="team-a/slack"} 1`)
	// A post on the connection makes its lane; the double's Post fails, which leaves the row pending and the lane kept.
	_, _ = w.ob.Send(t.Context(), outbox.Post{Source: "x", Role: store.OutboxReply, Connection: "team-a/slack", Channel: "C1", Thread: "1.1", Out: chat.Say(chat.Message{Fallback: "hi"})})
	if got := w.lanes(); got != lanes+1 {
		t.Fatalf("lanes = %d after a post on team-a; want %d", got, lanes+1)
	}

	w.secret("team-a", "tok-a2")
	w.eventually("team-a to restart on its new token", func() bool { return w.up("tok-a1") == 0 && w.up("tok-a2") == 1 })
	if w.up("tok-b") != 1 || w.up(ownToken) != 1 {
		t.Errorf("up: team-b %d, own %d; want the connections that did not change left running", w.up("tok-b"), w.up(ownToken))
	}

	// A deletion takes effect at the first read that settles it: some 10 to 20 seconds.
	for _, ns := range []string{"team-a", "team-c"} {
		if err := w.dyn.Tracker().Delete(crConnections, ns, "slack"); err != nil {
			t.Fatal(err)
		}
	}
	w.eventually("team-a and team-c to stop and team-a's lane to go", func() bool { return w.up("tok-a2") == 0 && w.up("tok-c") == 0 && w.lanes() == lanes })
	// The series of a connection that is away is deleted, not left at 0; the others' stay.
	r.waitNoMetric(`agentkourier_chat_connected{connection="team-a/slack"}`)
	r.waitMetric(`agentkourier_chat_connected{connection="team-b/slack"} 1`)
	if w.up("tok-b") != 1 || w.up(ownToken) != 1 {
		t.Errorf("up: team-b %d, own %d; want them running", w.up("tok-b"), w.up(ownToken))
	}
	// A connection that is no longer served has an outbox series only while a row of it is unposted: team-a's post is still pending, team-c has none.
	r.waitMetric(`agentkourier_outbox_pending{connection="team-a/slack"} 1`)
	w.eventually("team-c's outbox series to go with it", func() bool {
		_, body := r.get("/metrics")
		return !strings.Contains(body, `agentkourier_outbox_pending{connection="team-c/slack"}`)
	})
	r.waitMetric(`agentkourier_outbox_pending{connection="team-b/slack"} 0`)
	r.cancel()
	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after a signal; want nil", err)
	}
}

// At shutdown every connection's run stops in the intake's step, before anything it feeds, and every connection's transport closes after the
// final drain has begun, whichever way each connection came to run.
func TestShutdownStopsEveryIntakeFirstAndClosesEveryTransportAfterTheDrain(t *testing.T) {
	w := newConnWorld(t, nil)
	w.team("team-a", "tok-a")
	w.team("team-b", "tok-b")
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	w.eventually("three connections to run", func() bool { return w.up(ownToken) == 1 && w.up("tok-a") == 1 && w.up("tok-b") == 1 })

	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after a signal; want nil", err)
	}

	log := w.log()
	at := func(e string) int { return slices.Index(log, e) }
	for _, e := range []string{"step:intake", "step:interactions", "step:drain", "run-ended:" + ownToken, "run-ended:tok-a", "run-ended:tok-b",
		"closed:agent-kourier-system/slack-main", "closed:team-a/slack", "closed:team-b/slack"} {
		if at(e) < 0 {
			t.Fatalf("%q is not in the shutdown's log %v", e, log)
		}
	}
	for _, tok := range []string{ownToken, "tok-a", "tok-b"} {
		if at("step:intake") >= at("run-ended:"+tok) || at("run-ended:"+tok) >= at("step:interactions") {
			t.Errorf("the run of %s did not end in the intake's step: %v", tok, log)
		}
	}
	for _, conn := range []string{"agent-kourier-system/slack-main", "team-a/slack", "team-b/slack"} {
		if at("closed:"+conn) < at("step:drain") {
			t.Errorf("the transport of %s closed before the drain began: %v", conn, log)
		}
	}
}

// The own connection's start failure is the broker's, whatever the team connections are: a Secret that lacks a token stops the start with
// a wiring error, though a team connection is healthy.
func TestTheOwnConnectionsStartFailureIsUnchangedWithTeamConnections(t *testing.T) {
	w := newConnWorld(t, nil)
	w.team("team-a", "tok-a")
	if err := w.kube.CoreV1().Secrets("agent-kourier-system").Delete(t.Context(), "slack-main", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.kube.CoreV1().Secrets("agent-kourier-system").Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agent-kourier-system", Name: "slack-main"}, Data: map[string][]byte{"botToken": []byte(ownToken)}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err := start(t, w.c).waitWithin(30 * time.Second)
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "appToken") {
		t.Errorf("Run = %v; want ErrWiring for the own connection's Secret", err)
	}
}

// Every connection's intake drops a mention by the bot of another connection this broker runs, the own connection's too, and still takes a
// bot that is none of theirs: through the sink each Slack client was given (the adapter asks it before it forwards a bot's mention).
func TestEveryConnectionsIntakeDropsTheMentionsOfTheOtherConnectionsBots(t *testing.T) {
	w := newConnWorld(t, nil)
	w.team("team-a", "tok-a")
	w.binding("team-a", "on-a", "C0TEAMA0001")
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	w.eventually("both connections to run", func() bool { return w.up(ownToken) == 1 && w.up("tok-a") == 1 })
	for _, tc := range []struct {
		sinkOf, channel, bot string
		want                 bool
	}{
		{"tok-a", "C0TEAMA0001", "BOWN", false},      // team-a drops a mention by the own connection's bot
		{"tok-a", "C0TEAMA0001", "BSTRANGER", true},  // and takes one by a bot that is none of this broker's
		{ownToken, "G0123456789", "Btok-a", false},   // the own connection drops a mention by team-a's bot
		{ownToken, "G0123456789", "BSTRANGER", true}, // and takes a stranger's
	} {
		f, ok := w.sink(tc.sinkOf).(slack.BotMentionFilter)
		if !ok {
			t.Fatalf("the sink of %s is no BotMentionFilter", tc.sinkOf)
		}
		w.eventually("the Binding of the channel to be served", func() bool { return f.AcceptsBotMention(tc.channel, "BSTRANGER") })
		if got := f.AcceptsBotMention(tc.channel, tc.bot); got != tc.want {
			t.Errorf("sink of %s: AcceptsBotMention(%s, %s) = %t; want %t", tc.sinkOf, tc.channel, tc.bot, got, tc.want)
		}
	}
}

// Two ChatConnections of one Slack app are not both served: the older runs, and the Bindings of the younger are told it is not served.
func TestARunServesTheOlderOfTwoConnectionsOfOneSlackAppAndTellsTheOthersBindings(t *testing.T) {
	w := newConnWorld(t, nil)
	cluster := newStatusClusterOn(w.dyn)
	w.c.StatusClient = cluster.dyn
	w.ids["tok-a"] = w.ids[ownToken] // team-a's bot is the own connection's app
	w.team("team-a", "tok-a")
	w.team("team-b", "tok-b")
	w.binding("team-a", "on-a", "C0TEAMA0001")
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	w.eventually("team-b to run", func() bool { return w.up("tok-b") == 1 })
	w.eventually("the Binding of the refused connection to be told", func() bool {
		return cluster.stored(t, "team-a/on-a")["Accepted"].Reason == "ConnectionNotServed"
	})
	if w.up("tok-a") != 0 || w.up(ownToken) != 1 {
		t.Errorf("up: team-a %d, own %d; want the refused connection not to run and the own to", w.up("tok-a"), w.up(ownToken))
	}
	if a := cluster.stored(t, "team-a/on-a")["Accepted"]; a.Status != metav1.ConditionFalse || !strings.Contains(a.Message, "an app is served once") {
		t.Errorf("Accepted = %+v; want False, with why", a)
	}
}

// dueQuestion is a question that waits on a thread, past its deadline, with a task that its session records.
type dueQuestion struct{ id, conn, channel, binding string }

// seedDueQuestions puts the questions in the store at path before the broker starts.
func seedDueQuestions(t *testing.T, path string, qs ...dueQuestion) {
	t.Helper()
	seed, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		k := store.ConversationKey{Connection: q.conn, Channel: q.channel, Thread: "1.1"}
		if err := seed.UpsertSession(t.Context(), store.Session{Key: k, Binding: q.binding, ContextID: "ctx", LastActivity: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := seed.SetSessionTask(t.Context(), k, "", "task-"+q.id, store.TaskTurn{ID: "turn"}); err != nil {
			t.Fatal(err)
		}
		req, _ := json.Marshal(dialect.Interaction{Kind: dialect.AskUser, TaskID: a2a.TaskID("task-" + q.id), RequestID: "req-" + q.id, Questions: []dialect.Question{{Text: "Which?", Choices: []string{"a", "b"}}}})
		due := store.Interaction{ID: q.id, Key: k, TaskID: "task-" + q.id, Kind: store.InteractionAskUser, Request: req, CreatedAt: time.Now().Add(-2 * time.Hour), Deadline: time.Now().Add(-time.Hour)}
		if _, err := seed.CreateInteraction(t.Context(), due); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
}

// A question that waits on the thread of a refused connection (a second connection of the own connection's Slack app) is ended by the leader's
// sweep as an orphan's, though the connection's Binding is in the config: nothing runs the connection, and unlike a connection that is down for now it
// never will. This holds the leader's wiring of the sweep to the supervisor's refusals (Served).
func TestARunEndsTheDueQuestionOfARefusedConnectionAsAnOrphans(t *testing.T) {
	w := newConnWorld(t, nil)
	w.c.sweepEvery, w.c.orphanGrace = 10*time.Millisecond, 50*time.Millisecond
	w.ids["tok-twin"] = w.ids[ownToken]
	w.team("team-b", "tok-twin")
	w.binding("team-b", "on-b", "C0TEAMB0001")
	seedDueQuestions(t, w.c.DBPath, dueQuestion{id: "ix-twin", conn: "team-b/slack", channel: "C0TEAMB0001", binding: "team-b/on-b"})
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	r.waitMetric(`agentkourier_approval_outcomes_total{binding="team-b/on-b",outcome="orphaned"} 1`)
	if w.up("tok-twin") != 0 {
		t.Error("the refused connection runs")
	}
}

// The Bindings of a team connection are verified as the bot of their own connection, and not as the own connection's: the channel of a team
// Binding is read through the team's platform and the own Binding's through the own, and a refused connection's Binding is not probed at all.
func TestATeamBindingIsProbedWithItsOwnBot(t *testing.T) {
	w := newConnWorld(t, nil)
	w.c.probeEvery, w.c.probeTick, w.c.probeGap = 50*time.Millisecond, 10*time.Millisecond, time.Millisecond
	app := w.ids[ownToken]
	w.ids["tok-twin"] = app // a second connection of the own connection's app is refused
	w.team("team-a", "tok-a")
	w.binding("team-a", "on-a", "C0TEAMA0001")
	w.team("team-b", "tok-twin")
	w.binding("team-b", "on-b", "C0TEAMB0001")
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	w.eventually("team-a's channel to be read by team-a's bot", func() bool { return slices.Contains(w.probedBy("tok-a"), "C0TEAMA0001") })
	w.eventually("the own Binding's channel to be read by the own bot", func() bool { return slices.Contains(w.probedBy(ownToken), "G0123456789") })
	for tok, other := range map[string]string{ownToken: "C0TEAMA0001", "tok-a": "G0123456789"} {
		if slices.Contains(w.probedBy(tok), other) {
			t.Errorf("the bot of %s was asked about the channel %s, which is another connection's", tok, other)
		}
	}
	if w.up("tok-twin") != 0 || len(w.probedBy("tok-twin")) != 0 || slices.Contains(w.probedBy(ownToken), "C0TEAMB0001") {
		t.Errorf("the refused connection's Binding was probed (its bot: %v, the own bot: %v)", w.probedBy("tok-twin"), w.probedBy(ownToken))
	}
}

// A rotated Secret of a team connection is told to its Bindings as changed while the connection runs on the old tokens, and the restart that follows
// is verified: the pass after it clears what the first said, within a tick, and not at the next interval. The supervisor looks slowly here, so
// the pass the rotation calls for comes first; the interval is an hour, so only the change of the published connections can call for another.
func TestARotatedTeamSecretIsVerifiedAgainAfterTheRestart(t *testing.T) {
	w := newConnWorld(t, nil)
	w.c.probeEvery, w.c.probeTick, w.c.probeGap = time.Hour, 10*time.Millisecond, time.Millisecond
	w.c.superviseEvery = 300 * time.Millisecond
	w.team("team-a", "tok-a")
	w.binding("team-a", "on-a", "C0TEAMA0001")
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	w.eventually("team-a's channel to be read by its bot", func() bool { return slices.Contains(w.probedBy("tok-a"), "C0TEAMA0001") })

	w.secret("team-a", "tok-a2")

	w.eventually("team-a to restart on its new token", func() bool { return w.up("tok-a") == 0 && w.up("tok-a2") == 1 })
	w.eventually("the Binding to be verified again as the bot of the restarted connection", func() bool {
		return slices.Contains(w.probedBy("tok-a2"), "C0TEAMA0001")
	})
}

// A question that waits on a thread of a ChatConnection that is not in the config is ended by the leader's sweep as an orphan's, though the
// Binding of its session is in the config (team-b/on-b, which was pointed at team-b's connection after the question was asked on the connection
// team-z, and team-z's ChatConnection was deleted): nothing will run team-z again. A question on a connection that is in the config and down for
// rejected credentials (team-b's) is served and waits for it, not ended. This holds the leader's Served to the config's connections.
func TestARunEndsTheDueQuestionOfAConnectionThatIsGoneAndKeepsOneThatIsDown(t *testing.T) {
	w := newConnWorld(t, nil)
	w.c.sweepEvery, w.c.orphanGrace = 10*time.Millisecond, 50*time.Millisecond
	w.idErr["tok-b"] = &credentialsRejectedError{method: "auth.test", code: "invalid_auth"}
	w.team("team-b", "tok-b")
	w.binding("team-b", "on-b", "C0TEAMB0001")
	seedDueQuestions(t, w.c.DBPath,
		dueQuestion{id: "ix-gone", conn: "team-z/slack", channel: "C0TEAMZ0001", binding: "team-b/on-b"},
		dueQuestion{id: "ix-down", conn: "team-b/slack", channel: "C0TEAMB0001", binding: "team-b/on-b"})
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	r.waitMetric(`agentkourier_approval_outcomes_total{binding="team-b/on-b",outcome="orphaned"} 1`)
	// The sweep that looks at ix-down says it waits for its connection, and the sweep that does not serve it would count it down instead.
	logged := func(msg string) bool { // a line that has msg and is about ix-down
		for _, l := range strings.Split(w.logs.String(), "\n") {
			if strings.Contains(l, msg) && strings.Contains(l, "interaction=ix-down") {
				return true
			}
		}
		return false
	}
	w.eventually("the sweep to say that it waits for team-b's connection", func() bool {
		return logged("interact: the chat connection of interactions that are due is not running")
	})
	if logged("cannot be ended through its Binding") {
		t.Errorf("the sweep counted the grace for ix-down, whose connection is in the config:\n%s", grep(w.logs.String(), "cannot be ended through its Binding"))
	}
	st, err := sqlite.Open(t.Context(), w.c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for id, want := range map[string]store.InteractionState{"ix-gone": store.InteractionTimedOut, "ix-down": store.InteractionPending} {
		if row, err := st.GetInteraction(t.Context(), id); err != nil || row.State != want {
			t.Errorf("%s = %+v, %v; want %s: the connection that is gone ends its question and the one that is down keeps it", id, row.State, err, want)
		}
	}
	_, body := r.get("/metrics")
	if strings.Contains(body, "agentkourier_interaction_timeouts_total{} 1") {
		t.Errorf("/metrics:\n%s\nwant no timeout", grep(body, "agentkourier_interaction_timeouts_total"))
	}
}
