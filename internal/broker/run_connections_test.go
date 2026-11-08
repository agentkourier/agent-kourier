package broker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
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
	w := &connWorld{t: t, live: map[string]int{}, probed: map[string][]string{}, sinks: map[string]slack.Sink{}, ids: map[string]appIdentity{}}
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
	w.c.newSlack = func(sc slack.Config) slackAPI { return &liveSlack{w: w, token: sc.BotToken.Reveal()} }
	w.c.identify = func(_ context.Context, c Connection) (appIdentity, error) {
		tok := c.Credentials["botToken"].Reveal()
		w.mu.Lock()
		defer w.mu.Unlock()
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
}

func (l *liveSlack) Run(ctx context.Context, sink slack.Sink) error {
	l.w.mu.Lock()
	l.w.live[l.token]++
	l.w.sinks[l.token] = sink
	l.w.mu.Unlock()
	<-ctx.Done()
	l.w.mu.Lock()
	l.w.live[l.token]--
	l.w.events = append(l.w.events, "run-ended:"+l.token)
	l.w.mu.Unlock()
	return ctx.Err()
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
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	w.eventually("the own connection to run", func() bool { return w.up(ownToken) == 1 && w.ob != nil })
	lanes := w.lanes()

	w.team("team-a", "tok-a1")
	w.team("team-b", "tok-b")
	w.eventually("both team connections to run", func() bool { return w.up("tok-a1") == 1 && w.up("tok-b") == 1 })
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
	if err := w.dyn.Tracker().Delete(crConnections, "team-a", "slack"); err != nil {
		t.Fatal(err)
	}
	w.eventually("team-a to stop and its lane to go", func() bool { return w.up("tok-a2") == 0 && w.lanes() == lanes })
	if w.up("tok-b") != 1 || w.up(ownToken) != 1 {
		t.Errorf("up: team-b %d, own %d; want them running", w.up("tok-b"), w.up(ownToken))
	}
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
