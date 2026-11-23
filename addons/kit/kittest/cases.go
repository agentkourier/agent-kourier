package kittest

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/addons/kit/cluster"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// metric is the series of the subject's add-on: agentkourier_<Name>_<name>.
func metric[S runtime.Store](s Subject[S], name string) string {
	return "agentkourier_" + s.Addon.Name + "_" + name
}

func turns[S runtime.Store](s Subject[S], outcome string) string {
	return metric(s, `turns_total{outcome="`+outcome+`"}`)
}

// text is a user message of text.
func text(text string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
}

// answer is a user message of parts for task.
func answer(task *a2a.Task, parts ...*a2a.Part) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleUser, parts...)
	m.TaskID, m.ContextID = task.ID, task.ContextID
	return m
}

// goodAnswer is the subject's answer to paused, which completes it.
func goodAnswer[S runtime.Store](s Subject[S], paused *a2a.Task) *a2a.Message {
	m := answer(paused, a2a.NewTextPart(s.Answer))
	m.Metadata = s.AnswerMetadata
	return m
}

// send sends msg and returns its task as the send answers it.
func send(t *testing.T, c *a2aclient.Client, msg *a2a.Message) (*a2a.Task, error) {
	t.Helper()
	res, err := c.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		return nil, err
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		return nil, errors.New("the send answered a message, not a task")
	}
	return task, nil
}

// want sends msg and fails the test unless its task ends in state.
func want(t *testing.T, c *a2aclient.Client, msg *a2a.Message, state a2a.TaskState) *a2a.Task {
	t.Helper()
	task, err := send(t, c, msg)
	if err != nil || task.Status.State != state {
		t.Fatalf("the send = %+v, %v; want %s", task, err, state)
	}
	return task
}

// holding starts a turn of msg and returns its task's ID once the turn has begun, and hangUp, which ends the request
// (the turn runs on: a2a-go runs it detached from its request).
func holding(t *testing.T, c *a2aclient.Client, msg *a2a.Message) (id a2a.TaskID, hangUp func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ids := make(chan a2a.TaskID, 1)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for ev, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}) {
			if err != nil {
				return
			}
			var id a2a.TaskID
			switch v := ev.(type) {
			case *a2a.Task:
				id = v.ID
			case *a2a.TaskStatusUpdateEvent:
				if v.Status.State == a2a.TaskStateWorking {
					id = v.TaskID
				}
			}
			if id != "" {
				select {
				case ids <- id:
				default:
				}
			}
		}
	}()
	select {
	case id = <-ids:
	case <-ended:
		t.Fatal("the held turn's stream ended before its task began")
	case <-time.After(10 * time.Second):
		t.Fatal("the held turn did not begin within 10s")
	}
	hangUp = func() { cancel(); <-ended }
	t.Cleanup(hangUp)
	return id, hangUp
}

// openStore opens the subject's store on env, as a replica would, and closes it when the test ends.
func openStore[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) S {
	t.Helper()
	all := map[string]string{"AUTH_TOKEN_FILE": "/unused", "ADVERTISE_URL": "http://127.0.0.1:1", "POD_NAME": "kittest-seed"}
	maps.Copy(all, env)
	maps.Copy(all, s.Env)
	cfg, _, err := runtime.Parse(s.Addon, func(k string) string { return all[k] })
	if err != nil {
		t.Fatalf("the settings: %v", err)
	}
	st, err := s.Addon.Open(t.Context(), cfg, runtime.StoreOptions{Log: discard()})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// cutOff leaves a working task in the store, as a process that stopped mid-turn would. On Postgres it writes under a
// lease of its own, which it releases.
func cutOff[S runtime.Store](t *testing.T, s Subject[S], env map[string]string, id string) {
	t.Helper()
	st := openStore(t, s, env)
	if env["STORE"] == "postgres" {
		if _, ok, err := st.TryLead(t.Context(), "kittest-seed", "http://127.0.0.1:1", time.Hour); err != nil || !ok {
			t.Fatalf("TryLead: %v, %v", ok, err)
		}
	}
	if _, err := st.Create(AsCaller(), &a2a.Task{ID: a2a.TaskID(id), ContextID: "c-" + id, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
		t.Fatal(err)
	}
	if env["STORE"] == "postgres" {
		if err := st.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// wantInterrupted fails the test unless the task id has failed as interrupted by the subject's restart.
func wantInterrupted[S runtime.Store](t *testing.T, s Subject[S], c *a2aclient.Client, id a2a.TaskID) {
	t.Helper()
	got, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: id})
	if err != nil || got.Status.State != a2a.TaskStateFailed || got.Status.Message == nil || len(got.Status.Message.Parts) == 0 ||
		got.Status.Message.Parts[0].Text() != runtime.InterruptedMessage(s.Addon.Noun) {
		t.Fatalf("the cut-off task = %+v, %v; want failed with %q", got, err, runtime.InterruptedMessage(s.Addon.Noun))
	}
}

// The add-on reads no setting it does not declare, at its start or in a turn (Start's cleanup checks it).
func everySettingItReadsIsDeclared[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	in := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the replica to be ready", in.Ready)
	want(t, in.Client(t), text(s.Complete), a2a.TaskStateCompleted)
	if err := in.Stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// /metrics, /healthz and /readyz are open; the card and the endpoint are behind the caller check, whose refusal is
// counted; the replica leads.
func metricsAreServed[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	in := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the replica to be ready", in.Ready)
	card := in.Base + runtime.A2APath + runtime.CardPath
	for _, probe := range []struct {
		url, token string
		want       int
	}{
		{in.Base + "/healthz", "", http.StatusOK}, {in.Base + "/metrics", "", http.StatusOK},
		{card, "", http.StatusUnauthorized}, {card, Token, http.StatusOK}, {card, "fake-wrong-token", http.StatusUnauthorized},
	} {
		if got, _ := Probe(http.MethodGet, probe.url, probe.token); got != probe.want {
			t.Errorf("GET %s = %d, want %d", probe.url, got, probe.want)
		}
	}
	for series, want := range map[string]string{
		metric(s, "leader"): "1", metric(s, `auth_refused_total{reason="missing"}`): "1", metric(s, `auth_refused_total{reason="invalid"}`): "1",
	} {
		in.WantMetric(t, series, want)
	}
}

// A message sent again with its ID joins the task its first send started, and runs no turn of its own.
func aResentMessageJoinsItsTask[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	in := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the replica to be ready", in.Ready)
	c := in.Client(t)
	msg := text(s.Complete)
	first := want(t, c, msg, a2a.TaskStateCompleted)
	again := want(t, c, msg, a2a.TaskStateCompleted)
	if again.ID != first.ID {
		t.Fatalf("the resend's task %s, want the first's %s", again.ID, first.ID)
	}
	in.WantMetric(t, metric(s, "message_replays_total"), "1")
	in.WantMetric(t, turns(s, "completed"), "1")
}

// An answer the backend refuses is refused with invalid params and leaves the task paused for a good one.
func aRefusedAnswerKeepsTheTaskPaused[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	in := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the replica to be ready", in.Ready)
	c := in.Client(t)
	paused := want(t, c, text(s.Pause), a2a.TaskStateInputRequired)
	if _, err := send(t, c, answer(paused, s.BadAnswer...)); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("the bad answer = %v, want invalid params", err)
	}
	if got, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: paused.ID}); err != nil || got.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("the task after the bad answer = %+v, %v; want it still paused", got, err)
	}
	if done := want(t, c, goodAnswer(s, paused), a2a.TaskStateCompleted); done.ID != paused.ID {
		t.Fatalf("the answer completed %s, want %s", done.ID, paused.ID)
	}
	in.WantMetric(t, turns(s, "input_required"), "1")
	in.WantMetric(t, turns(s, "completed"), "1")
	in.WantMetric(t, turns(s, "failed"), "")
}

// A new message the backend refuses is refused with invalid params before any turn: none is counted, and a resend of
// it is refused again rather than joined to a task.
func aRefusedMessageStartsNoTurn[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	if s.BadMessage == nil {
		t.Skip("the add-on accepts every new message")
	}
	in := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the replica to be ready", in.Ready)
	c := in.Client(t)
	bad := a2a.NewMessage(a2a.MessageRoleUser, s.BadMessage...)
	for _, what := range []string{"the bad message", "its resend"} {
		if _, err := send(t, c, bad); !errors.Is(err, a2a.ErrInvalidParams) {
			t.Fatalf("%s = %v, want invalid params", what, err)
		}
	}
	want(t, c, text(s.Complete), a2a.TaskStateCompleted)
	in.WantMetric(t, turns(s, "completed"), "1")
	in.WantMetric(t, turns(s, "failed"), "")
}

// A cancel ends a running turn canceled, and the turn counts as canceled.
func aCancelEndsTheTurnCanceled[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	in := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the replica to be ready", in.Ready)
	c := in.Client(t)
	id, _ := holding(t, c, text(s.Hold))
	got, err := c.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: id})
	if err != nil || got.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("CancelTask = %+v, %v; want canceled", got, err)
	}
	in.WantMetric(t, turns(s, "canceled"), "1")
	in.WantMetric(t, turns(s, "failed"), "")
}

// A replica that leads fails the turns a restart cut off before it serves, and counts them.
func aStartFailsTheTurnsARestartCutOff[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	cutOff(t, s, env, "kittest-cut")
	in := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the replica to be ready", in.Ready)
	wantInterrupted(t, s, in.Client(t), "kittest-cut")
	in.WantMetric(t, metric(s, "tasks_interrupted_total"), "1")
	in.WantMetric(t, turns(s, "interrupted"), "1")
}

// A stop cuts a running turn short and leaves its task as it was, uncounted; the next start fails it as interrupted and
// counts it.
func aStopLeavesARunningTurnForTheNextStart[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	first := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the replica to be ready", first.Ready)
	id, hangUp := holding(t, first.Client(t), text(s.Hold))
	hangUp()
	if err := first.Stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	for _, outcome := range []string{"canceled", "failed", "interrupted"} {
		if got := first.Metric(turns(s, outcome)); got != "" {
			t.Errorf("the stopped replica counted %q turns %s: the cut turn is the next start's to count", got, outcome)
		}
	}
	next := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the restart to be ready", next.Ready)
	wantInterrupted(t, s, next.Client(t), id)
	next.WantMetric(t, metric(s, "tasks_interrupted_total"), "1")
}

// A context idle past SESSION_IDLE_TTL is released with its tasks.
func anIdleContextIsReleased[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	idle := maps.Clone(env)
	idle["SESSION_IDLE_TTL"] = "1h"
	in := Start(t, s, "kittest-a", idle, runtime.Options{UpkeepInterval: 20 * time.Millisecond,
		Now: func() time.Time { return time.Now().Add(2 * time.Hour) }})
	WaitFor(t, "the replica to be ready", in.Ready)
	c := in.Client(t)
	task := want(t, c, text(s.Complete), a2a.TaskStateCompleted)
	WaitFor(t, "the idle context's task to be released", func() bool {
		_, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
		return errors.Is(err, a2a.ErrTaskNotFound)
	})
}

// Two replicas: the first leads, fails the turn a restart cut off and serves; the second is a standby, ready at once,
// which settles nothing and forwards the endpoint and the card to the leader, where the caller check runs. When the
// leader stops it releases the lease, the standby takes over at once, and a task paused on the old leader takes its
// answer on the new one.
func aStandbyForwardsAndTakesOver[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	cutOff(t, s, env, "kittest-cut")
	a := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "a to be ready", a.Ready)
	b := Start(t, s, "kittest-b", env, runtime.Options{})
	if !b.Ready() {
		t.Fatal("a standby is not ready: it forwards, so it should be")
	}
	for series, want := range map[string][2]string{
		metric(s, "leader"): {"1", "0"}, metric(s, "leader_takeovers_total"): {"1", "0"}, metric(s, "tasks_interrupted_total"): {"1", "0"},
	} {
		if got := [2]string{a.Metric(series), b.Metric(series)}; got != want {
			t.Errorf("%s on a, b = %q, want %q", series, got, want)
		}
	}
	task := want(t, b.Client(t), text(s.Complete), a2a.TaskStateCompleted)
	events := 0
	for _, err := range b.Client(t).SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: text(s.Complete)}) {
		if err != nil {
			t.Fatalf("a streamed turn through the standby: %v", err)
		}
		events++
	}
	if events < 2 {
		t.Fatalf("a streamed turn through the standby gave %d events, want the leader's stream", events)
	}
	if got, err := a.Client(t).GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID}); err != nil || got.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the leader's view of the forwarded turn = %+v, %v", got, err)
	}
	wantInterrupted(t, s, b.Client(t), "kittest-cut")
	card := runtime.A2APath + runtime.CardPath
	if c, _ := Probe(http.MethodGet, b.Base+card, Token); c != http.StatusOK {
		t.Fatalf("the card through the standby = %d, want 200", c)
	}
	if c, _ := Probe(http.MethodGet, b.Base+card, ""); c != http.StatusUnauthorized {
		t.Fatalf("the card through the standby without a token = %d, want the leader's 401", c)
	}
	refused := metric(s, `auth_refused_total{reason="missing"}`)
	if got := [2]string{a.Metric(refused), b.Metric(refused)}; got != [2]string{"1", ""} {
		t.Errorf("refusals on a, b = %q, want the leader's alone: the check runs there", got)
	}
	if got := b.Metric(metric(s, `forwarded_requests_total{outcome="forwarded"}`)); got == "" || got == "0" {
		t.Errorf("the standby forwarded %q requests", got)
	}
	paused := want(t, b.Client(t), text(s.Pause), a2a.TaskStateInputRequired)

	if err := a.Stop(t); err != nil {
		t.Fatalf("the leader's Run after SIGTERM: %v", err)
	}
	stopped := time.Now()
	WaitFor(t, "b to take over", func() bool { return b.Metric(metric(s, "leader")) == "1" && b.Ready() })
	if since := time.Since(stopped); since > 750*time.Millisecond { // half the lease: a take at expiry would be later
		t.Errorf("b took over %s after a stopped: a released lease is taken on the next try, not when it expires", since)
	}
	if done := want(t, b.Client(t), goodAnswer(s, paused), a2a.TaskStateCompleted); done.ID != paused.ID {
		t.Fatalf("the answer on the new leader completed %s, want %s", done.ID, paused.ID)
	}
	if got := b.Metric(metric(s, "tasks_interrupted_total")); got != "0" {
		t.Errorf("the new leader failed %q tasks; a's turns had ended", got)
	}
	if len(a.Exits) != 0 || len(b.Exits) != 0 {
		t.Fatal("a replica exited")
	}
}

// A leader serves only once it has failed the turns a restart cut off: while its settlement waits (on a lock an outside
// transaction holds on the tasks), /readyz answers 503 and an A2A request 503 with Retry-After and the add-on's words.
func aLeaderSettlesBeforeItServes[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	cutOff(t, s, env, "kittest-cut")
	rollback := pgHold(t, env["POSTGRES_DSN"], `LOCK TABLE a2a_tasks IN EXCLUSIVE MODE`)
	a := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "the settlement to wait on the lock", func() bool { return lockWaiters(t, env["POSTGRES_DSN"]) > 0 })
	if c, _ := Probe(http.MethodGet, a.Base+"/readyz", ""); c != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while the leader settles = %d, want 503", c)
	}
	if c, retry := post(t, a.Base+runtime.A2APath, nil); c != http.StatusServiceUnavailable || retry == "" {
		t.Fatalf("an A2A request while the leader settles = %d, Retry-After %q; want 503 with Retry-After", c, retry)
	}
	words := "the " + s.Addon.Noun + " is taking over as the leader\n"
	if c, body := Probe(http.MethodPost, a.Base+runtime.A2APath, Token); c != http.StatusServiceUnavailable || body != words {
		t.Fatalf("an A2A request while the leader settles = %d %q, want 503 %q", c, body, words)
	}
	rollback()
	WaitFor(t, "a to be ready", a.Ready)
	wantInterrupted(t, s, a.Client(t), "kittest-cut")
	want(t, a.Client(t), text(s.Complete), a2a.TaskStateCompleted)
	a.WantMetric(t, metric(s, "tasks_interrupted_total"), "1")
}

// A standby forwards with the add-on's hop header and answers 503 to a request that already carries it.
func aStandbyForwardsWithTheHopHeader[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	hop := cluster.HopHeader(s.Addon.Name)
	hops := make(chan []string, 2)
	leader := newServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hops <- r.Header.Values(hop) }))
	seed := openStore(t, s, env)
	if _, ok, err := seed.TryLead(t.Context(), "kittest-seed", leader, time.Hour); err != nil || !ok {
		t.Fatalf("TryLead: %v, %v", ok, err)
	}
	b := Start(t, s, "kittest-b", env, runtime.Options{})
	WaitFor(t, "the standby to be ready", b.Ready)
	if c, retry := post(t, b.Base+runtime.A2APath, nil); c != http.StatusOK {
		t.Fatalf("an A2A request through the standby = %d (Retry-After %q), want the leader's 200", c, retry)
	}
	if got := <-hops; len(got) != 1 || got[0] != "1" {
		t.Fatalf("%s at the leader = %q, want [1]", hop, got)
	}
	if c, _ := post(t, b.Base+runtime.A2APath, map[string]string{hop: "1"}); c != http.StatusServiceUnavailable {
		t.Fatalf("a request another standby forwarded = %d, want 503", c)
	}
	if len(hops) != 0 {
		t.Fatal("the standby forwarded a request that another standby had forwarded")
	}
}

// A leader whose write finds that another replica has taken the lease stops at once with ExitLeadershipLost, before its
// next renewal would find the same.
func aLeaderWhoseWriteIsDeposedExits[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	slow := maps.Clone(env)
	maps.Copy(slow, map[string]string{"LEASE_DURATION": "15s", "LEASE_RENEW_DEADLINE": "10s", "LEASE_RETRY_PERIOD": "5s"})
	started := time.Now()
	a := Start(t, s, "kittest-a", slow, runtime.Options{})
	WaitFor(t, "a to be ready", a.Ready)
	depose(t, s, env)
	go func() {
		// The turn's first write is refused as deposed; what the client gets back does not matter here.
		_, _ = a.Client(t).SendMessage(context.Background(), &a2a.SendMessageRequest{Message: text(s.Complete)})
	}()
	select {
	case code := <-a.Exits:
		if code != cluster.ExitLeadershipLost {
			t.Fatalf("exit code %d, want %d", code, cluster.ExitLeadershipLost)
		}
		if since := time.Since(started); since >= 5*time.Second {
			t.Fatalf("exited %s after the start, at or after the first renewal: the write did not stop it", since)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the leader whose write was deposed did not exit before its next renewal")
	}
}

// A leader whose lease another replica takes stops with ExitLeadershipLost, and Run returns cluster.ErrLeadershipLost.
func aLeaderThatLosesTheLeaseExits[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	a := Start(t, s, "kittest-a", env, runtime.Options{})
	WaitFor(t, "a to be ready", a.Ready)
	depose(t, s, env)
	select {
	case code := <-a.Exits:
		if code != cluster.ExitLeadershipLost {
			t.Fatalf("exit code %d, want %d", code, cluster.ExitLeadershipLost)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the deposed leader did not exit")
	}
	if err := a.Wait(t, 15*time.Second); !errors.Is(err, cluster.ErrLeadershipLost) {
		t.Fatalf("Run = %v, want ErrLeadershipLost", err)
	}
}

// A replica that took the lease before it listens, and then cannot listen, hands the lease over as it stops.
func aReplicaThatCannotListenReleasesTheLease[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	all := map[string]string{"LISTEN_ADDR": taken.Addr().String(), "AUTH_TOKEN_FILE": TokenFile(t), "POD_NAME": "kittest-a",
		"ADVERTISE_URL": "http://" + taken.Addr().String()}
	maps.Copy(all, env)
	maps.Copy(all, s.Env)
	cfg, own, err := runtime.Parse(s.Addon, func(k string) string { return all[k] })
	if err != nil {
		t.Fatal(err)
	}
	var exits atomic.Int32
	if err := runtime.Run(t.Context(), s.Addon, cfg, own, runtime.Options{Log: discard(), Exit: func(int) { exits.Add(1) }}); err == nil {
		t.Fatal("Run listened on an address that was taken")
	}
	lease, err := openStore(t, s, env).CurrentLeader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if lease.Holder != "kittest-a" || lease.Live || exits.Load() != 0 {
		t.Fatalf("lease = %+v, exits %d; want kittest-a's, released, and no exit", lease, exits.Load())
	}
}

// depose has another store take the live lease. It names the intruder as the holder first, keeping the leader's epoch,
// so that its TryLead retakes the lease as a restarted process of the holder's pod would, however often the leader
// renews it meanwhile; the take moves the epoch on.
func depose[S runtime.Store](t *testing.T, s Subject[S], env map[string]string) {
	t.Helper()
	intruder := openStore(t, s, env)
	if err := pgExec(t, env["POSTGRES_DSN"], `UPDATE leadership SET holder = 'intruder'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := intruder.TryLead(t.Context(), "intruder", "http://127.0.0.1:1", time.Hour); err != nil || !ok {
		t.Fatalf("TryLead: %v, %v", ok, err)
	}
}

// post sends an empty A2A request with the token and the headers given, and returns its status and Retry-After.
func post(t *testing.T, url string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+Token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := prober.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

// newServer serves h on a loopback port until the test ends, and returns its URL.
func newServer(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// pgHold runs sql in a transaction of its own on dsn and keeps it open until the returned rollback runs (or the test
// ends).
func pgHold(t *testing.T, dsn, sql string) (rollback func()) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), sql); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	rollback = func() {
		once.Do(func() {
			_ = tx.Rollback(context.Background())
			_ = conn.Close(context.Background())
		})
	}
	t.Cleanup(rollback)
	return rollback
}

// lockWaiters is how many other sessions on dsn's database wait for a lock.
func lockWaiters(t *testing.T, dsn string) int {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var n int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pgExec runs one statement on dsn, outside any store.
func pgExec(t *testing.T, dsn, sql string) error {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(t.Context(), sql)
	return err
}
