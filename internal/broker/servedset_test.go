package broker

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// recordedServed is a store.ConnectionServing in memory: the record of the connections a broker served, which outlives the writer
// that wrote it, as the store's does.
type recordedServed struct {
	mu       sync.Mutex
	set      []string
	replaces int
	failRead error
	failPut  error
}

func (s *recordedServed) ServedConnections(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failRead != nil {
		return nil, s.failRead
	}
	return slices.Clone(s.set), nil
}

func (s *recordedServed) ReplaceServedConnections(_ context.Context, connections []string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut != nil {
		return s.failPut
	}
	if now.IsZero() || !slices.IsSorted(connections) {
		return store.ErrInvalid
	}
	s.replaces++
	s.set = slices.Clone(connections)
	return nil
}

func (s *recordedServed) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.set)
}

func (s *recordedServed) fail(read, put error) {
	s.mu.Lock()
	s.failRead, s.failPut = read, put
	s.mu.Unlock()
}

// namedBindings is what the source knows of the cluster for the writer: each ChatConnection that exists ("namespace/name") and the
// Bindings that name it, which is (*crsource.Source).ConnectionBindings; the connection admits all of them. A test changes the map
// between passes as the cluster changes.
type namedBindings map[string][]string

func (n namedBindings) of(namespace, name string) (crsource.ConnectionBindings, error) {
	b, ok := n[namespace+"/"+name]
	return crsource.ConnectionBindings{Exists: ok, Bindings: slices.Clone(b), Admitted: slices.Clone(b)}, nil
}

// servedSnapshot resolves the Bindings of each connection in served (the "namespace/name" of a connection, and that of each Binding
// that names it), all at generation 1 and with channels of their own, on one AgentBackend that every namespace may use.
func servedSnapshot(t *testing.T, served map[string][]string) *config.Snapshot {
	t.Helper()
	namespaces := map[string]bool{"sre": true}
	for _, bindings := range served {
		for _, key := range bindings {
			ns, _, _ := strings.Cut(key, "/")
			namespaces[ns] = true
		}
	}
	allowed := slices.Sorted(maps.Keys(namespaces))
	set := &config.Set{AgentBackends: []v1alpha1.AgentBackend{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "agent"}, Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, AllowedNamespaces: allowed},
	}}}
	for _, conn := range slices.Sorted(maps.Keys(served)) {
		ns, name, _ := strings.Cut(conn, "/")
		c := v1alpha1.ChatConnection{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		c.Spec.AllowedNamespaces = allowed
		set.ChatConnections = append(set.ChatConnections, c)
		for _, key := range served[conn] {
			bns, bname, _ := strings.Cut(key, "/")
			b := v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: bns, Name: bname, Generation: 1}}
			b.Spec.Agent.BackendRef = v1alpha1.ObjectRef{Namespace: "sre", Name: "agent"}
			b.Spec.Chat.ConnectionRef = v1alpha1.ObjectRef{Namespace: ns, Name: name}
			b.Spec.Chat.Channel = "C-" + bns + "-" + bname
			set.Bindings = append(set.Bindings, b)
		}
	}
	snap, rejected := config.ResolvePartial(set)
	if len(rejected) != 0 {
		t.Fatalf("the fixture's Bindings were rejected: %+v", rejected)
	}
	return snap
}

const (
	ownConn  = "sre/slack"
	teamConn = "team-a/slack"
	teamBind = "team-a/on-a"
)

func bothServed() map[string][]string {
	return map[string][]string{ownConn: {sreAlerts}, teamConn: {teamBind}}
}

func ownServed() map[string][]string { return map[string][]string{ownConn: {sreAlerts}} }

// bothExist is the cluster of bothServed: both ChatConnections exist, and each has its Binding.
func bothExist() namedBindings {
	return namedBindings{ownConn: {sreAlerts}, teamConn: {teamBind}}
}

// servedRig is a statusRig whose writer records the connections it serves in record and finds the Bindings of one it no longer
// serves in named. The Bindings it serves are probed all True.
type servedRig struct {
	*statusRig
	record *recordedServed
}

// newServedRig builds a writer over c that serves served. record is the store of the broker, with what an earlier one recorded.
func newServedRig(t *testing.T, c *statusCluster, served map[string][]string, record *recordedServed, named namedBindings) *servedRig {
	t.Helper()
	r := &servedRig{record: record, statusRig: newStatusRigWith(t, c, nil, func(cfg *statusConfig) {
		cfg.Served = newServedRecord(record, named.of, cfg.Now)
	})}
	r.serving(t, served)
	return r
}

// serving replaces what the source serves.
func (r *servedRig) serving(t *testing.T, served map[string][]string) {
	t.Helper()
	snap := servedSnapshot(t, served)
	r.mu.Lock()
	r.report.Snapshot = snap
	for _, bindings := range served {
		for _, key := range bindings {
			r.probed[key] = probe(1)
		}
	}
	r.mu.Unlock()
}

// pass lets the gap of a write go by, and makes a pass.
func (r *servedRig) pass() {
	r.advance(11 * time.Second)
	r.reconcile()
}

// appliesTo is the number of statuses written to the Binding key so far.
func appliesTo(c *statusCluster, key string) int {
	n := 0
	for _, a := range c.snapshotApplies() {
		if a.Namespace+"/"+a.Name == key {
			n++
		}
	}
	return n
}

// servedStatus is the status of a Binding at generation as the broker that serves it writes it.
func servedStatus(generation int64) []metav1.Condition {
	at := statusT0.Add(-time.Hour)
	return []metav1.Condition{
		cond("Accepted", metav1.ConditionTrue, "OK", generation, at), cond("BackendReachable", metav1.ConditionTrue, "OK", generation, at),
		cond("ChannelJoined", metav1.ConditionTrue, "OK", generation, at), cond("Ready", metav1.ConditionTrue, "OK", generation, at),
	}
}

// A Binding of a team connection is served as one of the own connection is: written, and put back if someone changes it. The leader
// records the connections it serves when it starts and does not write the record again while the set is the same.
func TestABindingOfATeamConnectionIsServedAndPutBack(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1))
	record := &recordedServed{}
	r := newServedRig(t, c, bothServed(), record, bothExist())
	r.reconcile()
	if got := c.stored(t, teamBind); got["Accepted"].Status != metav1.ConditionTrue || got["Ready"].Status != metav1.ConditionTrue {
		t.Fatalf("the Binding of the team connection says Accepted %+v, Ready %+v; want both True", got["Accepted"], got["Ready"])
	}
	if got, want := record.recorded(), []string{ownConn, teamConn}; !slices.Equal(got, want) {
		t.Errorf("the record = %v; want the two connections served", got)
	}
	foreign := storedBinding(teamBind, 1, cond("Ready", metav1.ConditionFalse, "Edited", 1, statusT0))
	if err := c.update(foreign, "team-a"); err != nil {
		t.Fatal(err)
	}
	r.pass()
	if got := c.stored(t, teamBind); got["Ready"].Status != metav1.ConditionTrue || appliesTo(c, teamBind) != 2 {
		t.Errorf("after an edit: Ready %+v, %d writes; want it put back by a second", got["Ready"], appliesTo(c, teamBind))
	}
	for range 3 {
		r.pass()
	}
	if record.replaces != 1 {
		t.Errorf("the record was written %d times for a set that did not change; want 1", record.replaces)
	}
}

// A connection that the Snapshot drops while the broker runs, and that still exists (the cases the source does not report itself: the
// ones it does, the connections the limit leaves out, are in the over-cap test below), tells its Bindings once: Accepted False with the
// reason ConnectionNotServed, and Ready with it. The broker does not put it back: a broker that serves the connection has the last word, and the two would otherwise go on at
// each other. When the connection is served again the Binding is served, and its words are put back as any served Binding's.
func TestABindingOfAConnectionTakenOutOfTheSetIsToldOnceAndNotPutBack(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1))
	record := &recordedServed{}
	r := newServedRig(t, c, bothServed(), record, bothExist())
	r.reconcile()

	r.serving(t, ownServed())
	r.pass()
	got := c.stored(t, teamBind)
	if a := got["Accepted"]; a.Status != metav1.ConditionFalse || a.Reason != "ConnectionNotServed" || a.ObservedGeneration != 1 || !strings.Contains(a.Message, teamConn) {
		t.Fatalf("Accepted = %+v; want False, ConnectionNotServed at generation 1, naming %s", a, teamConn)
	}
	if rd := got["Ready"]; rd.Status != metav1.ConditionFalse || rd.Reason != "Accepted:ConnectionNotServed" {
		t.Errorf("Ready = %+v; want False, Accepted:ConnectionNotServed", rd)
	}
	if _, probed := got["BackendReachable"]; probed {
		t.Errorf("a Binding that is not served has the probes of a served one: %v", got)
	}
	if got, want := record.recorded(), []string{ownConn, teamConn}; !slices.Equal(got, want) {
		t.Errorf("the record = %v; want it to keep the connection that stopped, which still exists", got)
	}
	if appliesTo(c, sreAlerts) != 1 || appliesTo(c, teamBind) != 2 {
		t.Errorf("writes: own Binding %d, team Binding %d; want 1 and 2 (served, then told)", appliesTo(c, sreAlerts), appliesTo(c, teamBind))
	}

	// The broker that serves the connection writes its own words, and has the last word.
	if err := c.update(storedBinding(teamBind, 1, servedStatus(1)...), "team-a"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		r.advance(time.Minute)
		r.reconcile()
	}
	if appliesTo(c, teamBind) != 2 || c.stored(t, teamBind)["Ready"].Status != metav1.ConditionTrue {
		t.Errorf("writes %d, Ready %+v; want the other broker's words left alone", appliesTo(c, teamBind), c.stored(t, teamBind)["Ready"])
	}

	// Served again here, its Binding is served, with the words of a served one.
	r.serving(t, bothServed())
	r.set(teamBind, probe(1)...)
	r.pass()
	if got := c.stored(t, teamBind); got["Accepted"].Status != metav1.ConditionTrue || got["BackendReachable"].Status != metav1.ConditionTrue {
		t.Errorf("after the connection is served again: Accepted %+v, BackendReachable %+v; want both True", got["Accepted"], got["BackendReachable"])
	}
}

// The admission list is read when the broker starts, so a namespace taken off it takes effect at a restart, and the broker that
// restarts cannot remember what it served. The store does: a connection that its record has and the set has not, and that still
// exists, tells its Bindings, once. It stays in the record, so that every later start finds it stopped too, and a Binding made on it
// afterwards is told as well. It leaves the record when it is served again (it stays, as served) or when it is deleted.
func TestABindingOfAConnectionTakenOffTheListWhileTheBrokerWasDownIsToldAfterEachRestart(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1, servedStatus(1)...))
	record := &recordedServed{set: []string{ownConn, teamConn}} // what the broker before it recorded
	named := bothExist()
	r := newServedRig(t, c, ownServed(), record, named)
	r.reconcile()

	got := c.stored(t, teamBind)
	if a := got["Accepted"]; a.Status != metav1.ConditionFalse || a.Reason != "ConnectionNotServed" {
		t.Fatalf("Accepted = %+v; want False, ConnectionNotServed, over the Ready=True that was left", a)
	}
	if rd := got["Ready"]; rd.Status != metav1.ConditionFalse || rd.Reason != "Accepted:ConnectionNotServed" {
		t.Errorf("Ready = %+v; want False, Accepted:ConnectionNotServed", rd)
	}
	if want := []string{ownConn, teamConn}; !slices.Equal(record.recorded(), want) || record.replaces != 0 {
		t.Errorf("the record = %v after %d writes; want it as it was: the stopped connection stays", record.recorded(), record.replaces)
	}
	writes := appliesTo(c, teamBind)
	for range 3 {
		r.pass()
	}
	if appliesTo(c, teamBind) != writes {
		t.Errorf("writes = %d after more passes; want %d: once, and not put back", appliesTo(c, teamBind), writes)
	}

	// A broker that starts again finds the connection stopped as well: the words are as they were and no write is made, and a Binding
	// made on the connection since is told.
	later := "team-a/later"
	if err := c.add(storedBinding(later, 1)); err != nil {
		t.Fatal(err)
	}
	named[teamConn] = []string{teamBind, later}
	again := newServedRig(t, c, ownServed(), record, named)
	again.reconcile()
	again.pass()
	if appliesTo(c, teamBind) != writes || c.stored(t, teamBind)["Accepted"].Reason != "ConnectionNotServed" {
		t.Errorf("after a second restart: %d writes, Accepted %+v; want the words as they were, written by none", appliesTo(c, teamBind), c.stored(t, teamBind)["Accepted"])
	}
	if a := c.stored(t, later)["Accepted"]; a.Reason != "ConnectionNotServed" {
		t.Errorf("a Binding made on the stopped connection since says Accepted %+v; want ConnectionNotServed", a)
	}
	if want := []string{ownConn, teamConn}; !slices.Equal(record.recorded(), want) {
		t.Errorf("the record = %v after the second restart; want %v", record.recorded(), want)
	}

	// Deleted, the connection leaves the record, and its Bindings are told nothing: they are ConnectionNotFound, everyone's to say.
	delete(named, teamConn)
	deleted := newServedRig(t, c, ownServed(), record, named)
	deleted.reconcile()
	if want := []string{ownConn}; !slices.Equal(record.recorded(), want) {
		t.Errorf("the record = %v after the connection was deleted; want it pruned to %v", record.recorded(), want)
	}
}

// A connection that the record has and that is served again is served, and stays in the record.
func TestAStoppedConnectionThatIsServedAgainStaysRecorded(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1))
	record := &recordedServed{set: []string{ownConn, teamConn}}
	r := newServedRig(t, c, bothServed(), record, bothExist())
	r.reconcile()
	r.pass()
	if want := []string{ownConn, teamConn}; !slices.Equal(record.recorded(), want) || record.replaces != 0 {
		t.Errorf("the record = %v after %d writes; want it unchanged", record.recorded(), record.replaces)
	}
	if got := c.stored(t, teamBind)["Accepted"]; got.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %+v; want True: the connection is served", got)
	}
}

// A connection in the record that no longer exists was deleted, not left out: its Bindings are everyone's to report as
// ConnectionNotFound, and the broker says nothing of it. It leaves the record.
func TestADeletedConnectionIsNotToldNotServed(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1, servedStatus(1)...))
	record := &recordedServed{set: []string{ownConn, teamConn}}
	r := newServedRig(t, c, ownServed(), record, namedBindings{ownConn: {sreAlerts}})
	r.reconcile()
	r.pass()
	if appliesTo(c, teamBind) != 0 {
		t.Errorf("%d writes to the Binding of a connection that was deleted; want none", appliesTo(c, teamBind))
	}
	if got, want := record.recorded(), []string{ownConn}; !slices.Equal(got, want) {
		t.Errorf("the record = %v; want the deleted connection pruned", got)
	}
}

// The status of a Binding says a ChatConnection exists and is not served only if the connection admits the Binding's namespace: a Binding
// that it does not admit learns no more of it than that it does not exist for it, and is left alone, whatever way the connection came to
// be left out.
func TestABindingTheConnectionDoesNotAdmitIsToldNothing(t *testing.T) {
	const outsider = "other/outsider"
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1, servedStatus(1)...), storedBinding(outsider, 1, servedStatus(1)...))
	record := &recordedServed{set: []string{ownConn, teamConn}}
	named := func(ns, name string) (crsource.ConnectionBindings, error) {
		if ns+"/"+name == teamConn {
			return crsource.ConnectionBindings{Exists: true, Bindings: []string{outsider, teamBind}, Admitted: []string{teamBind}}, nil
		}
		return crsource.ConnectionBindings{Exists: true}, nil
	}
	r := &servedRig{record: record, statusRig: newStatusRigWith(t, c, nil, func(cfg *statusConfig) { cfg.Served = newServedRecord(record, named, cfg.Now) })}
	r.serving(t, ownServed())
	r.reconcile()
	if c.stored(t, teamBind)["Accepted"].Reason != "ConnectionNotServed" || appliesTo(c, outsider) != 0 {
		t.Errorf("the admitted Binding says %+v and the outsider was written %d times; want the first told and the second left alone",
			c.stored(t, teamBind)["Accepted"], appliesTo(c, outsider))
	}
}

// The retention keeps the sessions of a Binding of a connection the broker served before and does not now, after a restart, as it
// keeps those of any Binding that exists and is not served: their threadTTL is not known, and may exceed the longest that is served.
// Those of a Binding that was deleted, as old, go. Against the real store, which also holds the record.
func TestRetentionKeepsTheSessionsOfAStoppedConnectionsBindingAfterARestart(t *testing.T) {
	st := sqlitetest.Open(t)
	if err := st.ReplaceServedConnections(t.Context(), []string{ownConn, teamConn}, sweepNow.Add(-time.Hour)); err != nil { // the broker before
		t.Fatal(err)
	}
	day := 24 * time.Hour
	stopped := store.ConversationKey{Connection: teamConn, Channel: "C1", Thread: "1.1"}
	deleted := store.ConversationKey{Connection: teamConn, Channel: "C1", Thread: "2.2"}
	for k, binding := range map[store.ConversationKey]string{stopped: teamBind, deleted: "team-a/long-gone"} {
		if err := st.UpsertSession(t.Context(), store.Session{Key: k, Binding: binding, LastActivity: sweepNow.Add(-400 * day)}); err != nil {
			t.Fatal(err)
		}
	}
	snap := servedSnapshot(t, ownServed())
	report := func() config.Report { return config.Report{Snapshot: snap} }
	record := newServedRecord(st, bothExist().of, func() time.Time { return sweepNow })
	known := record.retentionKnows(t.Context(), report, testutil.DiscardLogger(), func() (map[string]time.Duration, []string, bool) {
		return map[string]time.Duration{sreAlerts: 24 * time.Hour}, nil, true
	})
	newSessionRetention(t, &fakeRetentionStore{}, func(c *retentionConfig) {
		c.Store, c.ThreadTTLs, c.ThreadTTLsAndUnserved = st, nil, known
	}).purgeSessions(t.Context())
	for k, want := range map[store.ConversationKey]bool{stopped: true, deleted: false} {
		_, err := st.GetSession(t.Context(), k, 100*365*day, sweepNow)
		if present := err == nil; present != want {
			t.Errorf("session %s present = %v (%v); want %v", k.Thread, present, err, want)
		}
	}

	// A record that cannot be read leaves the retention knowing nothing, so that it deletes nothing: the stopped connections are unknown.
	broken := newServedRecord(&recordedServed{failRead: errors.New("store is down")}, bothExist().of, nil)
	if _, _, ok := broken.retentionKnows(t.Context(), report, testutil.DiscardLogger(),
		func() (map[string]time.Duration, []string, bool) { return nil, nil, true })(); ok {
		t.Error("the retention is told it knows the Bindings while the record cannot be read")
	}
}

// A connection that is invalid and has no good generation is not in the Snapshot, but its Bindings are still the broker's and are
// rejected as ConnectionRejected, which says more: the broker does not replace it with ConnectionNotServed.
func TestAMoreSpecificRejectionIsNotReplacedByNotServed(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1))
	record := &recordedServed{set: []string{ownConn, teamConn}}
	r := newServedRig(t, c, ownServed(), record, bothExist())
	r.reject(config.Rejection{Namespace: "team-a", Name: "on-a", Reason: config.ReasonConnectionRejected, Generation: 1, Message: "spec.chat.connectionRef: ChatConnection team-a/slack generation 1 is invalid"})
	r.reconcile()
	if a := c.stored(t, teamBind)["Accepted"]; a.Reason != "ConnectionRejected" {
		t.Errorf("Accepted = %+v; want ConnectionRejected", a)
	}
}

// The connections that the cap leaves out are in the report with their Bindings, and the broker tells those Bindings, as it tells those
// of a connection it stopped serving: once, not put back.
func TestABindingOfAnOverCapConnectionIsToldOnceAndNotPutBack(t *testing.T) {
	over := "teams/on-c16"
	const outsider = "other/outsider" // names the connection too, which does not admit its namespace
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(over, 3), storedBinding(outsider, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.mu.Lock()
	r.report.UnservedConnections = []config.UnservedConnection{{Namespace: "teams", Name: "c16", Reason: config.UnservedCap,
		Message: "the broker serves at most 16 chat connections", Bindings: []string{over, outsider}, Admitted: []string{over}}}
	r.mu.Unlock()
	r.reconcile()

	got := c.stored(t, over)
	if a := got["Accepted"]; a.Status != metav1.ConditionFalse || a.Reason != "ConnectionNotServed" || a.ObservedGeneration != 3 ||
		!strings.Contains(a.Message, "teams/c16") || !strings.Contains(a.Message, "at most 16 chat connections") {
		t.Fatalf("Accepted = %+v; want False, ConnectionNotServed at generation 3, naming the connection and the limit", a)
	}
	if rd := got["Ready"]; rd.Reason != "Accepted:ConnectionNotServed" {
		t.Errorf("Ready = %+v; want Accepted:ConnectionNotServed", rd)
	}
	if appliesTo(c, outsider) != 0 {
		t.Errorf("a Binding the connection does not admit was written %d times; want none", appliesTo(c, outsider))
	}
	if err := c.update(storedBinding(over, 3, servedStatus(3)...), "teams"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		r.advance(time.Minute)
		r.reconcile()
	}
	if appliesTo(c, over) != 1 || c.stored(t, over)["Ready"].Status != metav1.ConditionTrue {
		t.Errorf("writes = %d, Ready %+v; want the one, and the other broker's words left alone", appliesTo(c, over), c.stored(t, over)["Ready"])
	}

	// A place frees: the connection is served, its Bindings are the source's again and leave the report's list.
	r.mu.Lock()
	r.report.UnservedConnections = nil
	r.mu.Unlock()
	r.advance(11 * time.Second)
	r.reconcile()
	if appliesTo(c, over) != 1 {
		t.Errorf("writes = %d after the connection left the list; want none by the connection's reporter", appliesTo(c, over))
	}
}

// The record is the writer's to keep, and a store that fails it does not stop the writer writing the statuses of what is served. The
// failure is said once; the record is tried again at every pass that moves, and a set that was not recorded is compared with the record
// that is there.
func TestAFailingRecordDoesNotStopTheStatusesAndIsTriedAgain(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	record := &recordedServed{}
	record.fail(errors.New("store is down"), nil)
	r := newServedRig(t, c, ownServed(), record, bothExist())
	r.reconcile()
	if c.stored(t, sreAlerts)["Accepted"].Status != metav1.ConditionTrue {
		t.Error("the Binding that is served has no status while the record cannot be read")
	}
	for range 3 {
		r.pass()
	}
	if n := strings.Count(r.logs.String(), "cannot be compared with the record"); n != 1 {
		t.Errorf("the failure was said %d times; want once:\n%s", n, r.logs)
	}
	record.fail(nil, errors.New("store is read only"))
	r.pass()
	if len(record.recorded()) != 0 {
		t.Fatalf("the record = %v while writing it fails", record.recorded())
	}
	record.fail(nil, nil)
	r.pass()
	if got, want := record.recorded(), []string{ownConn}; !slices.Equal(got, want) {
		t.Errorf("the record = %v; want %v once the store takes it", got, want)
	}
}

// The reason ConnectionNotServed is one of the reasons Accepted orders and the reference page lists, in the same order.
func TestBindingStatusReferenceListsTheAcceptedReasonsInOrder(t *testing.T) {
	page, err := os.ReadFile("../../docs/reference/binding-status.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(page)
	start := strings.Index(text, "### `Accepted`")
	if start < 0 {
		t.Fatal("binding-status.md has no section for Accepted")
	}
	section := text[start+len("### `Accepted`"):]
	if end := strings.Index(section, "\n##"); end >= 0 {
		section = section[:end]
	}
	var documented []config.RejectionReason
	row := regexp.MustCompile("(?m)^\\| `([A-Za-z]+)` \\|")
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		if m[1] != "reason" {
			documented = append(documented, config.RejectionReason(m[1]))
		}
	}
	if !slices.Equal(documented, acceptedReasonOrder) {
		t.Errorf("binding-status.md lists the reasons of Accepted as %v; the writer orders them %v", documented, acceptedReasonOrder)
	}
}

// The sessions of a Binding of a connection the broker leaves out are not those of a deleted Binding: the retention keeps them, as it
// keeps those of a Binding that is rejected, and deletes those of a Binding that is gone, which are as old.
func TestRetentionKeepsTheSessionsOfAnOverCapConnectionsBinding(t *testing.T) {
	f := newFixture(t)
	r := &reporting{report: config.Report{
		Snapshot: snapshot(t, f.url, "kagent-v1", 48*time.Hour),
		UnservedConnections: []config.UnservedConnection{{Namespace: "teams", Name: "c16", Reason: config.UnservedCap, Message: "the broker serves at most 16 chat connections",
			Bindings: []string{"teams/on-c16"}}},
	}}
	b, err := NewBindings(r, f.reader, f.audit, Options{OnRejected: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	st := sqlitetest.Open(t)
	day := 24 * time.Hour
	overCap := store.ConversationKey{Connection: "teams/c16", Channel: "C1", Thread: "1.1"}
	deleted := store.ConversationKey{Connection: "teams/c16", Channel: "C1", Thread: "2.2"}
	for k, binding := range map[store.ConversationKey]string{overCap: "teams/on-c16", deleted: "teams/long-gone"} {
		if err := st.UpsertSession(t.Context(), store.Session{Key: k, Binding: binding, LastActivity: sweepNow.Add(-400 * day)}); err != nil {
			t.Fatal(err)
		}
	}
	newSessionRetention(t, &fakeRetentionStore{}, func(c *retentionConfig) {
		c.Store, c.ThreadTTLs, c.ThreadTTLsAndUnserved = st, nil, b.ThreadTTLsAndUnserved
	}).purgeSessions(t.Context())
	for k, want := range map[store.ConversationKey]bool{overCap: true, deleted: false} {
		_, err := st.GetSession(t.Context(), k, 100*365*day, sweepNow)
		if present := err == nil; present != want {
			t.Errorf("session %s present = %v (%v); want %v", k.Thread, present, err, want)
		}
	}
}

// A connection whose Bindings cannot be found yet is not forgotten, and nothing is told or deleted on a guess. The record keeps it
// when it is written for another reason; the retention, which needs the Bindings to know whose sessions to keep, is told it knows
// nothing, so it deletes no session; and when the lookup works the Bindings are known to both, and told.
func TestAConnectionWhoseBindingsCannotBeFoundYetIsKeptAndNothingIsDeleted(t *testing.T) {
	const otherBind = "team-b/on-b"
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1, servedStatus(1)...), storedBinding(otherBind, 1))
	record := &recordedServed{set: []string{ownConn, teamConn}}
	failing := true
	named := bothExist()
	naming := func(ns, name string) (crsource.ConnectionBindings, error) {
		if failing && ns+"/"+name == teamConn {
			return crsource.ConnectionBindings{}, crsource.ErrNotSynced
		}
		return named.of(ns, name)
	}
	var rec *servedRecord
	r := &servedRig{record: record, statusRig: newStatusRigWith(t, c, nil, func(cfg *statusConfig) {
		rec = newServedRecord(record, naming, cfg.Now)
		cfg.Served = rec
	})}
	// Another connection is served now, so the record is written while the lookup fails.
	r.serving(t, map[string][]string{ownConn: {sreAlerts}, "team-b/slack": {otherBind}})
	r.reconcile()
	if want := []string{ownConn, teamConn, "team-b/slack"}; !slices.Equal(record.recorded(), want) {
		t.Errorf("the record = %v after a write while the lookup failed; want %v: the connection is not forgotten", record.recorded(), want)
	}
	if appliesTo(c, teamBind) != 0 {
		t.Errorf("a Binding was written %d times while its connection could not be looked up; want none", appliesTo(c, teamBind))
	}

	r.mu.Lock()
	report := r.report
	r.mu.Unlock()
	reported := func() config.Report { return report }
	known := rec.retentionKnows(t.Context(), reported, testutil.DiscardLogger(), func() (map[string]time.Duration, []string, bool) {
		return map[string]time.Duration{sreAlerts: time.Hour}, nil, true
	})
	if _, _, ok := known(); ok {
		t.Error("the retention is told it knows the Bindings while a stopped connection cannot be looked up")
	}

	failing = false
	ttls, unserved, ok := known()
	if !ok || !slices.Equal(unserved, []string{teamBind}) || ttls[sreAlerts] != time.Hour {
		t.Errorf("after the lookup works the retention is told %v, %v, %v; want the Binding of the stopped connection unserved", ttls, unserved, ok)
	}
	r.pass()
	if a := c.stored(t, teamBind)["Accepted"]; a.Reason != "ConnectionNotServed" {
		t.Errorf("Accepted = %+v after the lookup works; want ConnectionNotServed", a)
	}
}

// The retention reads the report and the Bindings it keeps from one config. If a reload falls between its reads, it reads again once;
// if the config moves again, it knows nothing this tick. A connection that is stopped in the config the Bindings were read from, and
// served in the newer one, would otherwise leave its Bindings in neither list.
func TestRetentionKnowsReadsOneSnapshot(t *testing.T) {
	stopped, serving := servedSnapshot(t, ownServed()), servedSnapshot(t, bothServed())
	record := func() *servedRecord {
		return newServedRecord(&recordedServed{set: []string{ownConn, teamConn}}, bothExist().of, nil)
	}
	known := func(calls *int) func() (map[string]time.Duration, []string, bool) {
		return func() (map[string]time.Duration, []string, bool) {
			*calls++
			return map[string]time.Duration{sreAlerts: time.Hour}, nil, true
		}
	}
	sequence := func(snaps ...*config.Snapshot) func() config.Report {
		i := 0
		return func() config.Report { s := snaps[min(i, len(snaps)-1)]; i++; return config.Report{Snapshot: s} }
	}
	log := testutil.DiscardLogger()

	var reads int
	if _, unserved, ok := record().retentionKnows(t.Context(), sequence(stopped), log, known(&reads))(); !ok || reads != 1 || !slices.Equal(unserved, []string{teamBind}) {
		t.Errorf("a config that does not move: unserved %v, ok %v after %d reads; want the stopped Binding, ok, one read", unserved, ok, reads)
	}

	// The reload that serves the connection falls between the reads: the second attempt sees it served, and its Binding is no one's to keep.
	reads = 0
	if ttls, unserved, ok := record().retentionKnows(t.Context(), sequence(stopped, serving, serving, serving), log, known(&reads))(); !ok || reads != 2 || len(unserved) != 0 || ttls[sreAlerts] == 0 {
		t.Errorf("a config that moved once: unserved %v, ok %v after %d reads; want none unserved, ok, two reads", unserved, ok, reads)
	}

	// A config that moves at every read is never read whole.
	reads = 0
	flip := 0
	moving := func() config.Report {
		flip++
		if flip%2 == 0 {
			return config.Report{Snapshot: stopped}
		}
		return config.Report{Snapshot: serving}
	}
	if _, _, ok := record().retentionKnows(t.Context(), moving, log, known(&reads))(); ok || reads != 2 {
		t.Errorf("a config that moves at every read: ok %v after %d reads; want not ok after two", ok, reads)
	}
}

// The names the source lists for a connection are those of Bindings that exist and name it; one of them may be served by now (the
// cache lags a Binding that was moved from a served connection to a stopped one). A Binding the Snapshot serves is never told that
// its connection is not served.
func TestABindingTheSnapshotServesIsNotToldItsConnectionIsNotServed(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1, servedStatus(1)...))
	record := &recordedServed{set: []string{ownConn, teamConn}}
	named := namedBindings{ownConn: {sreAlerts}, teamConn: {teamBind, sreAlerts}}
	r := newServedRig(t, c, ownServed(), record, named)
	r.reconcile()
	r.pass()
	if a := c.stored(t, sreAlerts)["Accepted"]; a.Status != metav1.ConditionTrue {
		t.Errorf("a Binding the Snapshot serves says Accepted %+v; want True", a)
	}
	if a := c.stored(t, teamBind)["Accepted"]; a.Reason != "ConnectionNotServed" {
		t.Errorf("the other Binding says Accepted %+v; want ConnectionNotServed", a)
	}
}

// The leader half is wired to the record: with the custom resources, the status writer tells the Bindings of a connection the store
// records as served and that is not served now, and the retention keeps their sessions, as old as a deleted Binding's that it deletes.
// Without Served in the writer's config, or the retention's wrapper, one of the two halves fails; without OnUnserved the metric stays 0.
func TestRunTellsAndKeepsTheBindingsOfAConnectionItStoppedServing(t *testing.T) {
	c := crStartConfig(t, nil)
	dyn := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	cluster := newStatusClusterOn(dyn)
	c.StatusClient = cluster.dyn
	conn := crObject(t, "chatconnection.yaml")
	conn.SetNamespace("team-a")
	conn.SetName("slack")
	conn.SetUID("uid-team-a-slack")
	bound := crObject(t, "binding-minimal.yaml")
	bound.SetNamespace("team-a")
	bound.SetName("on-a")
	bound.SetUID("uid-team-a-on-a")
	for field, value := range map[string]string{"namespace": "team-a", "name": "slack"} {
		_ = unstructured.SetNestedField(bound.Object, value, "spec", "chat", "connectionRef", field)
	}
	_ = unstructured.SetNestedField(bound.Object, "C0TEAMA0001", "spec", "chat", "channel")
	if err := dyn.Tracker().Create(crConnections, conn, "team-a"); err != nil {
		t.Fatal(err)
	}
	if err := dyn.Tracker().Create(crBindings, bound, "team-a"); err != nil {
		t.Fatal(err)
	}

	c.retentionEvery = 10 * time.Millisecond
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	if err := seed.ReplaceServedConnections(t.Context(), []string{testConnection.Namespace + "/" + testConnection.Name, "team-a/slack"}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	stopped := store.ConversationKey{Connection: "team-a/slack", Channel: "C1", Thread: "1.1"}
	deleted := store.ConversationKey{Connection: "team-a/slack", Channel: "C1", Thread: "2.2"}
	for k, binding := range map[store.ConversationKey]string{stopped: "team-a/on-a", deleted: "team-a/long-gone"} {
		if err := seed.UpsertSession(t.Context(), store.Session{Key: k, Binding: binding, LastActivity: time.Now().Add(-400 * day)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	timeout := time.After(30 * time.Second)
	for cluster.stored(t, "team-a/on-a")["Accepted"].Reason != "ConnectionNotServed" {
		select {
		case <-cluster.applied:
		case <-timeout:
			t.Fatalf("the Binding of the stopped connection was not told: applies = %d", cluster.count())
		}
	}
	// The leader counts the Bindings it told, in the metric an operator alerts on.
	r.waitMetric(`agentkourier_config_binding_rejections{reason="ConnectionNotServed"} 1`)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) { // the store's own metric says the sweep ran
		if _, body := r.get("/metrics"); strings.Contains(body, `operation="agentkourier.store.PurgeExpiredSessions"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session purge never ran")
		}
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for k, want := range map[store.ConversationKey]bool{stopped: true, deleted: false} {
		_, err := st.GetSession(t.Context(), k, 100*365*day, time.Now())
		if present := err == nil; present != want {
			t.Errorf("session %s present = %v (%v); want %v", k.Thread, present, err, want)
		}
	}
}
