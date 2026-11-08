package broker

import (
	"context"
	"errors"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
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
// Bindings that name it, which is statusConfig.ConnectionBindings.
type namedBindings map[string][]string

func (n namedBindings) of(namespace, name string) ([]string, bool, error) {
	b, ok := n[namespace+"/"+name]
	return slices.Clone(b), ok, nil
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
		cfg.Served, cfg.ConnectionBindings = record, named.of
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

// A connection that leaves the set while the broker runs (its namespace's connection is deleted and made again, or a connection
// older than it takes its place under the limit) tells its Bindings once: Accepted False with the reason ConnectionNotServed, and Ready
// with it. The broker does not put it back: a broker that serves the connection has the last word, and the two would otherwise go on at
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
	if got, want := record.recorded(), []string{ownConn}; !slices.Equal(got, want) {
		t.Errorf("the record = %v; want the own connection alone", got)
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
// exists, tells its Bindings, once. The record is then the set that is served, so a second restart forgets it, and says nothing again.
func TestABindingOfAConnectionTakenOffTheListWhileTheBrokerWasDownIsToldAfterARestart(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1, servedStatus(1)...))
	record := &recordedServed{set: []string{ownConn, teamConn}} // what the broker before it recorded
	r := newServedRig(t, c, ownServed(), record, bothExist())
	r.reconcile()

	got := c.stored(t, teamBind)
	if a := got["Accepted"]; a.Status != metav1.ConditionFalse || a.Reason != "ConnectionNotServed" {
		t.Fatalf("Accepted = %+v; want False, ConnectionNotServed, over the Ready=True that was left", a)
	}
	if rd := got["Ready"]; rd.Status != metav1.ConditionFalse || rd.Reason != "Accepted:ConnectionNotServed" {
		t.Errorf("Ready = %+v; want False, Accepted:ConnectionNotServed", rd)
	}
	if got, want := record.recorded(), []string{ownConn}; !slices.Equal(got, want) {
		t.Errorf("the record = %v; want the set that is served now", got)
	}
	writes := appliesTo(c, teamBind)
	for range 3 {
		r.pass()
	}
	if appliesTo(c, teamBind) != writes {
		t.Errorf("writes = %d after more passes; want %d: once, and not put back", appliesTo(c, teamBind), writes)
	}

	// A broker that starts again has a record of the own connection alone: it has nothing to compare, and leaves the words as they are.
	again := newServedRig(t, c, ownServed(), record, bothExist())
	again.reconcile()
	again.pass()
	if appliesTo(c, teamBind) != writes || c.stored(t, teamBind)["Accepted"].Reason != "ConnectionNotServed" {
		t.Errorf("after a second restart: %d writes, Accepted %+v; want the words as they were, written by none", appliesTo(c, teamBind), c.stored(t, teamBind)["Accepted"])
	}
}

// A connection in the record that no longer exists was deleted, not left out: its Bindings are everyone's to report as
// ConnectionNotFound, and the broker says nothing of it.
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
		t.Errorf("the record = %v; want the set that is served now", got)
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
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(over, 3))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.mu.Lock()
	r.report.UnservedConnections = []config.UnservedConnection{{Namespace: "teams", Name: "c16", Reason: config.UnservedCap,
		Message: "the broker serves at most 16 chat connections", Bindings: []string{over}}}
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
// failure is said once; the record is tried again at the next full pass, and a set that was not recorded is compared with the record
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

	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
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
