package broker

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// orphanRig is the leader's interactions over a store and the real Bindings, with the sweeper's clock the test moves: a question waits past its
// deadline on a thread of each connection given, downOwn, which runs, and downTeam, which does not. Each thread is under the Binding sre/oncall of
// the fixture's config, with a task that the session records.
type orphanRig struct {
	*fixture
	d       interactionDeps
	ix      *interactionRuntime
	mgr     *session.Manager
	sw      *interact.Sweeper
	clock   time.Time
	refused *refusals
	plat    *askedPlatforms
}

// emptyConfig is the snapshot of a config that has no objects: every Binding is gone.
func emptyConfig(t *testing.T) *config.Snapshot {
	t.Helper()
	snap, err := config.Resolve(t.Context(), &config.Set{}, keys{})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// connectionsConfig is the snapshot of a config in which the Binding sre/oncall is on the ChatConnection bindingOn, and conns are the
// ChatConnections ("namespace/name") it has. bindingOn is one of them. Every connection admits the namespace sre.
func connectionsConfig(t *testing.T, url, bindingOn string, conns ...string) *config.Snapshot {
	t.Helper()
	set := &config.Set{AgentBackends: []v1alpha1.AgentBackend{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "kagent"},
		Spec:       v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, URL: url, AllowedNamespaces: []string{"sre"}},
	}}}
	for _, c := range conns {
		ns, name, _ := strings.Cut(c, "/")
		set.ChatConnections = append(set.ChatConnections, v1alpha1.ChatConnection{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec:       v1alpha1.ChatConnectionSpec{Platform: "slack", CredentialsSecretRef: v1alpha1.SecretRef{Name: "slack"}, AllowedNamespaces: []string{"sre"}},
		})
	}
	ns, name, _ := strings.Cut(bindingOn, "/")
	set.Bindings = []v1alpha1.Binding{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "oncall"},
		Spec: v1alpha1.BindingSpec{
			Agent:    v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "platform", Name: "kagent"}, Name: "test-agent", Namespace: "kagent"},
			Identity: v1alpha1.Identity{TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "creds"}},
			Chat:     v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: ns, Name: name}, Channel: "C1"},
			Session:  v1alpha1.Session{ThreadTTL: metav1.Duration{Duration: 36 * time.Hour}},
		},
	}}
	snap, err := config.Resolve(t.Context(), set, keys{})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// setConfig makes the connections and the Binding's connection what the leader's Reloader reports from now on.
func (r *orphanRig) setConfig(t *testing.T, bindingOn string, conns ...string) {
	t.Helper()
	r.snaps.p.Store(connectionsConfig(t, r.url, bindingOn, conns...))
}

func newOrphanRig(t *testing.T, conns ...string) *orphanRig {
	t.Helper()
	f := newFixture(t)
	f.snaps.p.Store(connectionsConfig(t, f.url, downOwn, downOwn, downTeam))
	d, ix, mgr, plat := downParts(t, f.b)
	r := &orphanRig{fixture: f, d: d, ix: ix, mgr: mgr, clock: time.Now(), refused: &refusals{}, plat: plat}
	d.now = func() time.Time { return r.clock }
	d.served = connectionServed(func() config.Report { return config.Report{Snapshot: f.snaps.Current()} }, downOwn, r.refused)
	r.d = d
	due := r.clock.Add(-time.Hour)
	for _, conn := range conns {
		k := pendingQuestion(t, d.st, conn, "ix-"+conn[3:], due, "")
		if err := d.st.SetSessionTask(t.Context(), k, "", "task-ix-"+conn[3:], store.TaskTurn{ID: "turn"}); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if r.sw, err = d.newSweeper(mgr, ix); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *orphanRig) sweep(t *testing.T) {
	t.Helper()
	if err := r.sw.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep = %v", err)
	}
}

func (r *orphanRig) state(t *testing.T, id string) store.InteractionState {
	t.Helper()
	row, err := r.d.st.GetInteraction(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row.State
}

func (r *orphanRig) taskOf(t *testing.T, conn string) string {
	t.Helper()
	sess, err := r.d.st.GetSession(t.Context(), store.ConversationKey{Connection: conn, Channel: "C0123456789", Thread: "1.1"}, 100*365*24*time.Hour, r.clock)
	if err != nil {
		t.Fatal(err)
	}
	return sess.TaskID
}

// requireOrphaned fails unless the interaction ix was ended without a word: its session's task cleared, no request to the agent, nothing posted
// or queued, and the outcome counted for the Binding.
func (r *orphanRig) requireOrphaned(t *testing.T, id, conn string) {
	t.Helper()
	if got := r.state(t, id); got != store.InteractionTimedOut {
		t.Errorf("%s is %s; want it ended", id, got)
	}
	if got := r.taskOf(t, conn); got != "" {
		t.Errorf("the session of %s records task %q; want it cleared, so that retention can purge the session", conn, got)
	}
	if r.mgr.HoldsTask(store.ConversationKey{Connection: conn, Channel: "C0123456789", Thread: "1.1"}, "task-"+id) {
		t.Error("a runner still holds the task")
	}
	if rows, err := r.d.st.ListUnposted(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("unposted rows = %+v, %v; want none: no notice, no edit", rows, err)
	}
	got := strings.Join(samples(t, r.d.metrics), "\n")
	if want := `agentkourier_approval_outcomes_total{binding=sre/oncall,outcome=orphaned} 1`; !strings.Contains(got, want) {
		t.Errorf("the series lack %q:\n%s", want, got)
	}
	if strings.Contains(got, "agentkourier_interaction_timeouts_total 1") {
		t.Errorf("the end was counted as a timeout:\n%s", got)
	}
}

// TestADeletedBindingEndsItsPendingInteractionOnTheLeader: the Binding is deleted while its question waits. The sweep of the leader's
// wiring (the real Bindings, session manager, store) lets the Binding's absence last the grace, then ends the interaction, clears the task from
// the session, calls the agent not at all and posts nothing, whether or not the connection runs.
func TestADeletedBindingEndsItsPendingInteractionOnTheLeader(t *testing.T) {
	for name, conn := range map[string]string{"the connection runs": downOwn, "the connection was removed": downTeam} {
		t.Run(name, func(t *testing.T) {
			r := newOrphanRig(t, conn)
			r.snaps.p.Store(emptyConfig(t))
			id := "ix-" + conn[3:]

			r.sweep(t)
			if got := r.state(t, id); got != store.InteractionPending {
				t.Fatalf("%s is %s at the first sight of the missing Binding; want it pending", id, got)
			}
			r.clock = r.clock.Add(5*time.Minute - time.Second)
			r.sweep(t)
			if got := r.state(t, id); got != store.InteractionPending {
				t.Fatalf("%s is %s inside the grace; want it pending", id, got)
			}
			r.clock = r.clock.Add(time.Second)
			r.sweep(t)

			r.requireOrphaned(t, id, conn)
			if got := r.lastAuth(); got != "" {
				t.Errorf("the agent got a request (Authorization %q); want none", got)
			}
		})
	}
}

// TestABindingThatIsBackWithinTheGraceKeepsItsInteraction: a config that lost the Binding for a moment (a reload, a takeover) and has it again
// at the next sweep leaves the interaction to be timed out as usual.
func TestABindingThatIsBackWithinTheGraceKeepsItsInteraction(t *testing.T) {
	r := newOrphanRig(t, downOwn)
	full := r.snaps.p.Load()
	r.snaps.p.Store(emptyConfig(t))
	r.sweep(t)
	r.clock = r.clock.Add(4 * time.Minute)
	r.snaps.p.Store(full)
	r.clock = r.clock.Add(2 * time.Minute)
	r.sweep(t)
	got := strings.Join(samples(t, r.d.metrics), "\n")
	if strings.Contains(got, "outcome=orphaned") {
		t.Errorf("the interaction was ended as an orphan's though its Binding was back:\n%s", got)
	}
}

// TestTheInteractionOfARefusedConnectionIsEndedAndOfOneThatIsDownIsKept: a second connection of a Slack app keeps its Bindings in the config and
// is not run, so its pause is ended after the grace, with the agent asked to cancel the task (the Binding is there to ask through), and no notice.
// A connection that is served and not running (its credentials rejected, or not started yet) keeps its interaction, however long it takes.
func TestTheInteractionOfARefusedConnectionIsEndedAndOfOneThatIsDownIsKept(t *testing.T) {
	r := newOrphanRig(t, downOwn, downTeam)
	r.plat.stopped = map[string]error{downTeam: &connectionStopped{conn: downTeam, rejected: true, code: "invalid_auth"}}
	for range 3 { // rejected credentials: served, and down for now
		r.sweep(t)
		r.clock = r.clock.Add(time.Hour)
	}
	if got := r.state(t, "ix-team"); got != store.InteractionPending {
		t.Fatalf("the interaction of a connection stopped for rejected credentials is %s; want it pending, for as long as it takes", got)
	}
	if got := r.taskOf(t, downTeam); got != "task-ix-team" {
		t.Errorf("the session records task %q; want the pause left alone", got)
	}

	r.refused.set(map[string]string{downTeam: refusedMessage}) // the supervisor finds it the second connection of an app
	r.sweep(t)
	if got := r.state(t, "ix-team"); got != store.InteractionPending {
		t.Fatalf("the interaction is %s at the first sight of the refusal; want it pending: the older connection may go", got)
	}
	r.clock = r.clock.Add(5 * time.Minute)
	r.sweep(t)
	if got := r.state(t, "ix-team"); got != store.InteractionTimedOut {
		t.Errorf("the interaction of the refused connection is %s; want it ended", got)
	}
	if got := r.lastAuth(); got == "" {
		t.Error("the agent was not asked to cancel the task, though the Binding is there to ask through")
	}
	rows, err := r.d.st.ListUnposted(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Connection == downTeam {
			t.Errorf("a row was queued for the refused connection: %+v; want none, no platform serves it", row)
		}
	}
	if got := r.state(t, "ix-own"); got != store.InteractionTimedOut {
		t.Errorf("the interaction of the connection that runs is %s; want it timed out as ever", got)
	}
}

// TestRetentionPurgesTheSessionOfAnInteractionThatWasEndedWithoutItsBinding: a session with a pending interaction and a task is never purged, which is
// why a pause that nothing could end held its session for ever. Once the sweep has ended the pause the session goes at its horizon.
func TestRetentionPurgesTheSessionOfAnInteractionThatWasEndedWithoutItsBinding(t *testing.T) {
	r := newOrphanRig(t, downTeam)
	r.snaps.p.Store(emptyConfig(t))
	day := 24 * time.Hour
	k := store.ConversationKey{Connection: downTeam, Channel: "C0123456789", Thread: "1.1"}
	if err := r.d.st.UpsertSession(t.Context(), store.Session{Key: k, Binding: "sre/oncall", LastActivity: sweepNow.Add(-200 * day)}); err != nil {
		t.Fatal(err)
	}
	if err := r.d.st.SetSessionTask(t.Context(), k, "", "task-ix-team", store.TaskTurn{ID: "turn"}); err != nil { // the upsert took the task off
		t.Fatal(err)
	}
	if got := r.taskOf(t, downTeam); got != "task-ix-team" {
		t.Fatalf("the session records task %q; want the pause's", got)
	}
	retain := newStaleRig(t, nil, func(c *retentionConfig) {
		c.Store = r.d.st
		c.ThreadTTLs = func() (map[string]time.Duration, bool) { return map[string]time.Duration{}, true } // the Binding is gone
	}).r

	retain.purgeSessions(t.Context())
	if _, err := r.d.st.GetSession(t.Context(), k, 100*365*day, sweepNow); err != nil {
		t.Fatalf("the session was purged with a pause waiting on it: %v", err)
	}
	r.sweep(t)
	r.clock = r.clock.Add(5 * time.Minute)
	r.sweep(t)
	if got := r.state(t, "ix-team"); got != store.InteractionTimedOut {
		t.Fatalf("the interaction is %s; want it ended", got)
	}
	retain.purgeSessions(t.Context())
	if _, err := r.d.st.GetSession(t.Context(), k, 100*365*day, sweepNow); err == nil {
		t.Error("the session is still there after the pause was ended; want retention to have purged it")
	}
}

// TestTheLeaderTellsTheSweepWhichConnectionsAreRefused: refusals.served is the sweeper's Served, and says false for exactly the connections
// the supervisor refused.
func TestTheLeaderTellsTheSweepWhichConnectionsAreRefused(t *testing.T) {
	r := &refusals{}
	if !r.served("team-a/slack") {
		t.Error("a connection nobody refused is not served")
	}
	r.set(map[string]string{"team-a/slack": refusedMessage})
	if r.served("team-a/slack") || !r.served("team-b/slack") {
		t.Errorf("served = %v for the refused connection and %v for another; want false and true", r.served("team-a/slack"), r.served("team-b/slack"))
	}
	r.set(nil)
	if !r.served("team-a/slack") {
		t.Error("a connection that is not refused any more is not served")
	}
}

// TestAPausesConnectionThatIsDeletedAfterTheBindingMovedOffItIsEnded: a team points its Binding at the platform connection, then deletes its own
// ChatConnection, while a question waits on a thread of that connection. The Binding is still in the config, and a connection that is in the
// config but does not run (its credentials were rejected) keeps its pauses for as long as it takes; once the connection is gone from the config
// nothing can run it again, so the pause is ended after the grace, with one request to the agent through the Binding, and nothing posted.
func TestAPausesConnectionThatIsDeletedAfterTheBindingMovedOffItIsEnded(t *testing.T) {
	r := newOrphanRig(t, downTeam)
	r.plat.stopped = map[string]error{downTeam: &connectionStopped{conn: downTeam, rejected: true, code: "invalid_auth"}}
	r.setConfig(t, downOwn, downOwn, downTeam) // the Binding moved to the platform connection; the team's still exists
	for range 3 {
		r.sweep(t)
		r.clock = r.clock.Add(time.Hour)
	}
	if got := r.state(t, "ix-team"); got != store.InteractionPending {
		t.Fatalf("the interaction is %s while its connection is in the config and down for rejected credentials; want it pending for as long as it takes", got)
	}

	r.setConfig(t, downOwn, downOwn) // the team deletes its ChatConnection
	r.sweep(t)
	if got := r.state(t, "ix-team"); got != store.InteractionPending {
		t.Fatalf("the interaction is %s at the first sight of the connection gone; want it pending: the grace covers a reload", got)
	}
	r.clock = r.clock.Add(5*time.Minute - time.Second)
	r.sweep(t)
	if got := r.state(t, "ix-team"); got != store.InteractionPending {
		t.Fatalf("the interaction is %s inside the grace; want it pending", got)
	}
	r.clock = r.clock.Add(time.Second)
	r.sweep(t)
	r.requireOrphaned(t, "ix-team", downTeam)
	if got := r.lastAuth(); got == "" {
		t.Error("the agent was not asked to cancel the task, though the Binding is in the config to ask through")
	}
}

// TestAPausesConnectionThatRunsKeepsItsTimeoutWhenTheBindingMovedOffIt: the Binding is pointed at another connection while the first still
// runs. The pause's thread is on the first, whose platform edits the prompt and posts the notice, and the Binding still reaches the agent, so the
// timeout ends it as it ends any: it is not an orphan's, which would leave the prompt with its buttons.
func TestAPausesConnectionThatRunsKeepsItsTimeoutWhenTheBindingMovedOffIt(t *testing.T) {
	r := newOrphanRig(t, downTeam)
	r.plat.running[downTeam] = builtPlatform{Platform: chattest.NewFake()}
	r.setConfig(t, downOwn, downOwn, downTeam)
	r.sweep(t)
	r.sweep(t)
	if got := r.state(t, "ix-team"); got != store.InteractionTimedOut {
		t.Errorf("the interaction is %s; want it timed out at once through the Binding", got)
	}
	got := strings.Join(samples(t, r.d.metrics), "\n")
	if strings.Contains(got, "outcome=orphaned") || !strings.Contains(got, "agentkourier_interaction_timeouts_total{} 1") {
		t.Errorf("the end was counted as an orphan's, not a timeout:\n%s", got)
	}
}

// TestConnectionServedIsWhatTheConfigAndTheSupervisorSay: served is the own connection; a connection of the Snapshot that is not refused; nothing
// that is not in the Snapshot, whatever its Bindings say; and everything before the first load.
func TestConnectionServedIsWhatTheConfigAndTheSupervisorSay(t *testing.T) {
	url := "http://agent.test"
	snap := connectionsConfig(t, url, "ns/b", "ns/b", "ns/c")
	var current *config.Snapshot
	refused := &refusals{}
	served := connectionServed(func() config.Report { return config.Report{Snapshot: current} }, "ns/own", refused)
	for _, c := range []struct {
		name, conn string
		snap       *config.Snapshot
		refuse     map[string]string
		want       bool
	}{
		{"before the first load", "ns/b", nil, nil, true},
		{"in the config", "ns/b", snap, nil, true},
		{"not in the config", "ns/gone", snap, nil, false},
		{"refused", "ns/c", snap, map[string]string{"ns/c": refusedMessage}, false},
		{"the own, whatever the config has", "ns/own", snap, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			current = c.snap
			refused.set(c.refuse)
			if got := served(c.conn); got != c.want {
				t.Errorf("served(%q) = %v; want %v", c.conn, got, c.want)
			}
		})
	}
}
