package crsource_test

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

var created = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// teamConnection returns a ChatConnection of the corpus' kind in namespace/name, created at the time given, which lets the
// Bindings of the namespaces named in allowed use it.
func teamConnection(objs []*unstructured.Unstructured, namespace, name string, at time.Time, allowed ...string) *unstructured.Unstructured {
	u := find(objs, "slack-main")
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetUID(types.UID("uid-" + namespace + "-" + name))
	u.SetCreationTimestamp(metav1.NewTime(at))
	if len(allowed) == 0 {
		unstructured.RemoveNestedField(u.Object, "spec", "allowedNamespaces")
	} else {
		_ = unstructured.SetNestedStringSlice(u.Object, allowed, "spec", "allowedNamespaces")
	}
	return u
}

// bindingOn returns a Binding of namespace/name on the connection connNamespace/connName, on a channel of its own (derived from
// the name, so a test does not give two the same).
func bindingOn(objs []*unstructured.Unstructured, namespace, name, connNamespace, connName string) *unstructured.Unstructured {
	u := find(objs, "chat-only")
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetUID(types.UID("uid-" + namespace + "-" + name))
	_ = unstructured.SetNestedField(u.Object, connNamespace, "spec", "chat", "connectionRef", "namespace")
	_ = unstructured.SetNestedField(u.Object, connName, "spec", "chat", "connectionRef", "name")
	sum := 0
	for _, r := range namespace + "/" + name {
		sum = sum*31 + int(r)
	}
	_ = unstructured.SetNestedField(u.Object, fmt.Sprintf("C%010d", sum%1_000_000_000), "spec", "chat", "channel")
	return u
}

// startAdmitted is start for a broker that is admitted to the namespaces, with its log in a sink and a clock the test moves.
func (c *cluster) startAdmitted(t *testing.T, clock *testClock, namespaces ...string) (*crsource.Source, *logSink) {
	t.Helper()
	logs := &logSink{}
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own, ConnectionNamespaces: namespaces, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if clock != nil {
		src.SetClockForTest(clock.Now)
	}
	t.Cleanup(src.Stop)
	if err := src.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return src, logs
}

func connectionNames(s *config.Set) []string {
	return names(s.ChatConnections, func(c v1alpha1.ChatConnection) string { return c.Namespace + "/" + c.Name })
}

func unservedNames(items []config.UnservedConnection) []string {
	return names(items, func(u config.UnservedConnection) string { return u.Namespace + "/" + u.Name + " " + string(u.Reason) })
}

// The served set is the own connection and every ChatConnection of an admitted namespace; the Bindings that are read are those
// that name any of them, or name no connection that exists. A connection of a namespace that is not admitted, and the Bindings
// that name it, are another broker's.
func TestTheServedSetIsTheOwnConnectionAndTheAdmittedNamespaces(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	extra := []*unstructured.Unstructured{
		teamConnection(objs, "team-a", "slack", created),
		teamConnection(objs, "team-b", "slack", created),
		teamConnection(objs, "team-c", "slack", created), // not admitted
		bindingOn(objs, "team-a", "on-a", "team-a", "slack"),
		bindingOn(objs, "team-b", "on-b", "team-b", "slack"),
		bindingOn(objs, "team-c", "on-c", "team-c", "slack"),
		bindingOn(objs, "sre", "dangling", "team-a", "nowhere"), // names no connection that exists
	}
	src, _ := newCluster(t, append(objs, extra...)...).startAdmitted(t, nil, "team-a", "team-b")

	s, _ := set(t, src)
	if got, want := connectionNames(s), []string{"agent-kourier-system/slack-main", "team-a/slack", "team-b/slack"}; !slices.Equal(got, want) {
		t.Errorf("connections = %v; want the own and the two admitted", got)
	}
	if got, want := bindingNames(s), []string{"sre/chat-only", "sre/dangling", "sre/sre-alerts", "team-a/on-a", "team-b/on-b"}; !slices.Equal(got, want) {
		t.Errorf("Bindings = %v; want those of the three served connections and the one that names none (team-c's is another broker's)", got)
	}
	if len(s.Invalid) != 0 || len(s.UnservedConnections) != 0 {
		t.Errorf("Invalid %+v, unserved connections %+v; want nothing: team-c is not this broker's to report", s.Invalid, s.UnservedConnections)
	}
}

// No list, and an empty one, serve the own connection alone: the Set is what it was before the list existed, whatever other
// namespaces hold.
func TestWithoutAnAdmissionListOnlyTheOwnConnectionIsServed(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	extra := []*unstructured.Unstructured{teamConnection(objs, "team-a", "slack", created), bindingOn(objs, "team-a", "on-a", "team-a", "slack")}
	// What a broker with no list reads of a cluster that holds a team's objects is what it reads of one that does not: the whole Set.
	aloneSrc, _ := newCluster(t, objs...).startAdmitted(t, nil)
	alone, _ := set(t, aloneSrc)
	var fingerprints []string
	for name, namespaces := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			src, _ := newCluster(t, append(slices.Clone(objs), extra...)...).startAdmitted(t, nil, namespaces...)
			s, fp := set(t, src)
			fingerprints = append(fingerprints, fp)
			if got := connectionNames(s); !slices.Equal(got, []string{"agent-kourier-system/slack-main"}) {
				t.Errorf("connections = %v; want the own alone", got)
			}
			if got := bindingNames(s); !slices.Equal(got, []string{"sre/chat-only", "sre/sre-alerts"}) {
				t.Errorf("Bindings = %v; want those of the own connection", got)
			}
			if !reflect.DeepEqual(s, alone) {
				t.Errorf("the Set differs from that of a cluster without the team's objects:\n got  %+v\n want %+v", s, alone)
			}
		})
	}
	if len(fingerprints) == 2 && fingerprints[0] != fingerprints[1] {
		t.Errorf("fingerprints %q and %q; want one for no list and an empty one", fingerprints[0], fingerprints[1])
	}
}

// A Binding of another namespace than its connection's needs the connection to list its namespace in allowedNamespaces, a team's
// connection as the own one: the Binding is read either way, and resolution says whether it is served.
func TestABindingOfAnotherNamespaceStillNeedsTheConnectionsAllowedNamespaces(t *testing.T) {
	leakCheck(t)
	for _, tc := range []struct {
		name    string
		allowed []string
		served  bool
	}{
		{"not listed", []string{"payments"}, false},
		{"none listed", nil, false},
		{"listed", []string{"sre"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := corpus(t)
			extra := []*unstructured.Unstructured{
				teamConnection(objs, "team-a", "slack", created, tc.allowed...),
				bindingOn(objs, "sre", "on-team-a", "team-a", "slack"),
			}
			src, _ := newCluster(t, append(objs, extra...)...).startAdmitted(t, nil, "team-a")
			if got := bindingNames(first(t, src)); !slices.Contains(got, "sre/on-team-a") {
				t.Fatalf("Bindings = %v; want the Binding read: it names a served connection", got)
			}
			rep := report(t, src)
			_, served := rep.Snapshot.Binding("sre", "on-team-a")
			var reasons []config.RejectionReason
			for _, r := range rep.Rejections {
				if r.Name == "on-team-a" {
					reasons = append(reasons, r.Reason)
				}
			}
			switch {
			case tc.served && (!served || len(reasons) != 0):
				t.Errorf("served %v, rejections %v; want it served", served, reasons)
			case !tc.served && (served || !slices.Equal(reasons, []config.RejectionReason{config.ReasonNamespaceNotAllowed})):
				t.Errorf("served %v, rejections %v; want it left out as NamespaceNotAllowed", served, reasons)
			}
		})
	}
}

// allowTeam lets the Bindings of the namespace use the corpus' backend, so that a team's Binding is rejected for its connection and not for that.
func allowTeam(objs []*unstructured.Unstructured, namespace string) {
	i := slices.IndexFunc(objs, func(u *unstructured.Unstructured) bool { return u.GetName() == "kagent-pilot" })
	_ = unstructured.SetNestedStringSlice(objs[i].Object, []string{"sre", "payments", namespace}, "spec", "allowedNamespaces")
}

func first(t *testing.T, src *crsource.Source) *config.Set {
	t.Helper()
	s, _ := set(t, src)
	return s
}

// The broker serves at most maxServedConnections, its own included, and the own connection counts first. The oldest of the others
// fill the rest, by creationTimestamp and not by name; the one left out is listed with its reason, said once at Warn, and its
// Bindings are not read (they are nobody's yet).
func TestTheOldestTeamConnectionsAreServedUpToTheCap(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var extra []*unstructured.Unstructured
	// c01 is the youngest and c16 the oldest, so that an order by name would leave out the last and this one leaves out the first.
	for i := 1; i <= crsource.MaxServedConnections; i++ {
		name := fmt.Sprintf("c%02d", i)
		extra = append(extra, teamConnection(objs, "teams", name, created.Add(-time.Duration(i)*time.Hour)), bindingOn(objs, "teams", "on-"+name, "teams", name))
	}
	// A Binding of another namespace names the connection that is left out too, and so does one the broker cannot read (invalid); both
	// are the connection's, whatever they hold.
	extra = append(extra, bindingOn(objs, "sre", "on-c01", "teams", "c01"))
	broken := bindingOn(objs, "payments", "broken-on-c01", "teams", "c01")
	_ = unstructured.SetNestedField(broken.Object, "#general", "spec", "chat", "channel")
	extra = append(extra, broken)
	src, logs := newCluster(t, append(objs, extra...)...).startAdmitted(t, nil, "teams")

	s, _ := set(t, src)
	if got := len(s.ChatConnections); got != crsource.MaxServedConnections {
		t.Errorf("%d connections served; want the cap, %d", got, crsource.MaxServedConnections)
	}
	if got := connectionNames(s); slices.Contains(got, "teams/c01") || !slices.Contains(got, "agent-kourier-system/slack-main") || !slices.Contains(got, "teams/c02") {
		t.Errorf("connections = %v; want the own and c02 to c16, not the youngest, c01", got)
	}
	if got := unservedNames(s.UnservedConnections); !slices.Equal(got, []string{"teams/c01 cap"}) {
		t.Errorf("unserved connections = %v; want c01, for the cap", got)
	}
	if m := s.UnservedConnections[0].Message; !strings.Contains(m, fmt.Sprint(crsource.MaxServedConnections)) {
		t.Errorf("the message %q does not say the limit", m)
	}
	if got := bindingNames(s); slices.Contains(got, "teams/on-c01") || !slices.Contains(got, "teams/on-c02") || len(got) != 2+crsource.MaxServedConnections-1 {
		t.Errorf("Bindings = %v; want the two of the own and one for each served team connection, not c01's", got)
	}
	// It names the Bindings that name it, sorted, the unreadable one too and none of another connection's.
	wantBindings := []string{"payments/broken-on-c01", "sre/on-c01", "teams/on-c01"}
	if got := s.UnservedConnections[0].Bindings; !slices.Equal(got, wantBindings) {
		t.Errorf("the Bindings of the connection left out = %v; want %v", got, wantBindings)
	}
	// The report carries it for the status writer and the retention that come later.
	rep := report(t, src)
	if !slices.Equal(unservedNames(rep.UnservedConnections), []string{"teams/c01 cap"}) || !slices.Equal(rep.UnservedConnections[0].Bindings, wantBindings) {
		t.Errorf("Report.UnservedConnections = %+v; want c01, for the cap, with its Bindings %v", rep.UnservedConnections, wantBindings)
	}
	// Said once, at Warn, however many reads follow.
	set(t, src)
	set(t, src)
	if n := strings.Count(logs.String(), "left out"); n != 1 || !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "teams/c01") {
		t.Errorf("the connection was said left out %d times; want once, at Warn, naming teams/c01\n%s", n, logs.String())
	}
}

// Connections of one age are told apart by namespace, then name; the own connection is served whatever its age.
func TestConnectionsOfOneAgeAreOrderedByNamespaceThenName(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var extra []*unstructured.Unstructured
	for i := 1; i <= (crsource.MaxServedConnections-1)/2+1; i++ {
		for _, ns := range []string{"team-b", "team-a"} {
			extra = append(extra, teamConnection(objs, ns, fmt.Sprintf("c%d", i), created))
		}
	}
	// The own connection is younger than all of them, and is served all the same.
	ownConn := find(objs, "slack-main")
	ownConn.SetCreationTimestamp(metav1.NewTime(created.Add(time.Hour)))
	objs[slices.IndexFunc(objs, func(u *unstructured.Unstructured) bool { return u.GetName() == "slack-main" })] = ownConn
	src, _ := newCluster(t, append(objs, extra...)...).startAdmitted(t, nil, "team-a", "team-b")

	s, _ := set(t, src)
	if len(s.ChatConnections) != crsource.MaxServedConnections || !slices.Contains(connectionNames(s), "agent-kourier-system/slack-main") {
		t.Fatalf("connections = %v; want %d with the own among them", connectionNames(s), crsource.MaxServedConnections)
	}
	// 2 x 8 = 16 team connections of one age: 15 are served, and the one left out is the last by namespace and then name.
	if got := unservedNames(s.UnservedConnections); !slices.Equal(got, []string{"team-b/c8 cap"}) {
		t.Errorf("unserved connections = %v; want team-b/c8 (the last by namespace, then name)", got)
	}
}

// A connection that is invalid holds its place under the cap: which connections are served depends on the objects' ages and
// names and on nothing in their specs, so a typo does not move another connection in or out.
func TestAnInvalidConnectionHoldsItsPlaceUnderTheCap(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var extra []*unstructured.Unstructured
	for i := 1; i <= crsource.MaxServedConnections; i++ { // c01 is the oldest, c16 the youngest
		extra = append(extra, teamConnection(objs, "teams", fmt.Sprintf("c%02d", i), created.Add(time.Duration(i)*time.Hour)))
	}
	_ = unstructured.SetNestedField(extra[0].Object, "teams", "spec", "platform") // c01: invalid, from the start
	src, _ := newCluster(t, append(objs, extra...)...).startAdmitted(t, nil, "teams")

	s, _ := set(t, src)
	if got := connectionNames(s); slices.Contains(got, "teams/c01") || !slices.Contains(got, "teams/c15") || slices.Contains(got, "teams/c16") {
		t.Errorf("connections = %v; want the own and c02 to c15: c01 is invalid and is not served, and c16 is left out although a place is empty", got)
	}
	if len(s.Invalid) != 1 || s.Invalid[0].Name != "c01" || s.Invalid[0].Served != 0 {
		t.Errorf("Invalid = %+v; want c01, with no generation served", s.Invalid)
	}
	if got := unservedNames(s.UnservedConnections); !slices.Equal(got, []string{"teams/c16 cap"}) {
		t.Errorf("unserved connections = %v; want c16, for the cap", got)
	}
}

// A team connection that becomes invalid keeps serving its last good generation, as any object does, and is reported.
func TestATeamConnectionThatBecomesInvalidKeepsItsLastGoodGeneration(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	team := teamConnection(objs, "team-a", "slack", created)
	c := newCluster(t, append(objs, team)...)
	src, _ := c.startAdmitted(t, nil, "team-a")
	_, fp := set(t, src)

	bad := team.DeepCopy()
	bad.SetGeneration(2)
	_ = unstructured.SetNestedField(bad.Object, "teams", "spec", "platform")
	until(t, func() { c.apply(bad) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, _ := set(t, src)
	if got := connectionNames(s); !slices.Equal(got, []string{"agent-kourier-system/slack-main", "team-a/slack"}) {
		t.Errorf("connections = %v; want both: the team's at its last good generation", got)
	}
	i := slices.IndexFunc(s.Invalid, func(i config.Invalid) bool { return i.Name == "slack" })
	if i < 0 || s.Invalid[i].Namespace != "team-a" || s.Invalid[i].Generation != 2 || s.Invalid[i].Served != 1 || s.Invalid[i].Deleted {
		t.Errorf("Invalid = %+v; want team-a/slack generation 2 refused, 1 served", s.Invalid)
	}
}

// A team connection that is deleted has no hold: it stops as any object does, at the first read past the floor that finds nothing
// newly gone, and is then not served and not in Invalid. Until then it is served, with no word of it. Its Binding, still there,
// then names a connection that does not exist, and is rejected as ConnectionNotFound.
func TestADeletedTeamConnectionIsNotHeld(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	allowTeam(objs, "team-a")
	team := teamConnection(objs, "team-a", "slack", created, "team-a")
	onTeam := bindingOn(objs, "team-a", "on-a", "team-a", "slack")
	c := newCluster(t, append(objs, team, onTeam)...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, logs := c.startAdmitted(t, clock, "team-a")
	read(t, src)

	until(t, func() { c.remove(team) }, func() bool { return src.CachedGenerationForTest("ChatConnection", "team-a", "slack") < 0 })
	s, fp := read(t, src)
	if got := connectionNames(s); !slices.Contains(got, "team-a/slack") || len(s.Invalid) != 0 {
		t.Errorf("at the first read that finds it gone: connections %v, Invalid %+v; want it still served and no word of it", got, s.Invalid)
	}
	clock.advance(8 * time.Second)
	if due := src.Fingerprint(t.Context()); due == fp {
		t.Error("the Fingerprint does not say that the deletion is due")
	}
	s, _ = read(t, src)
	if got := connectionNames(s); !slices.Equal(got, []string{"agent-kourier-system/slack-main"}) {
		t.Errorf("connections = %v at the first read past the floor; want the own alone", got)
	}
	if len(s.Invalid) != 0 || len(held(s)) != 0 {
		t.Errorf("Invalid %+v; a deleted team connection is not held", s.Invalid)
	}
	if got := bindingNames(s); !slices.Contains(got, "team-a/on-a") {
		t.Errorf("Bindings = %v; want the Binding of the deleted connection still read: it names none that exists, so every broker reports it", got)
	}
	rep := report(t, src)
	var reasons []config.RejectionReason
	for _, r := range rep.Rejections {
		if r.Namespace == "team-a" {
			reasons = append(reasons, r.Reason)
		}
	}
	if !slices.Equal(reasons, []config.RejectionReason{config.ReasonConnectionNotFound}) {
		t.Errorf("rejections of team-a = %v; want ConnectionNotFound", reasons)
	}
	if strings.Contains(logs.String(), "held") {
		t.Errorf("the log speaks of a hold:\n%s", logs.String())
	}
	// And it stays gone, past the grace, for good.
	clock.advance(2 * time.Minute)
	if s, _ := read(t, src); len(s.ChatConnections) != 1 || len(s.Invalid) != 0 {
		t.Errorf("after the grace: connections %v, Invalid %+v; want the own alone", connectionNames(s), s.Invalid)
	}
}

// The deletion of a team and everything of it together (a team removed, or a Helm release rolled back) takes the connection and
// its Bindings together, and holds nothing; the own connection's Bindings are untouched.
func TestADeletedTeamAndItsBindingsAreGoneTogether(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	team := teamConnection(objs, "team-a", "slack", created)
	onTeam := bindingOn(objs, "team-a", "on-a", "team-a", "slack")
	c := newCluster(t, append(objs, team, onTeam)...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, _ := c.startAdmitted(t, clock, "team-a")
	read(t, src)

	until(t, func() { c.remove(onTeam); c.remove(team) }, func() bool {
		return src.CachedGenerationForTest("ChatConnection", "team-a", "slack") < 0 && src.CachedGenerationForTest("Binding", "team-a", "on-a") < 0
	})
	read(t, src)
	clock.advance(8 * time.Second)
	s, _ := read(t, src)
	if got := bindingNames(s); !slices.Equal(got, []string{"sre/chat-only", "sre/sre-alerts"}) || len(s.ChatConnections) != 1 || len(s.Invalid) != 0 {
		t.Errorf("Bindings %v, connections %v, Invalid %+v; want only the own connection's, and nothing held", got, connectionNames(s), s.Invalid)
	}
}

// A connection left out by the cap is served again when a place opens, and that is said once at Info. A deleted connection keeps
// its place until its deletion takes effect, so the cap is never exceeded while it waits.
func TestAConnectionLeftOutByTheCapIsServedWhenAPlaceOpens(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var extra []*unstructured.Unstructured
	for i := 1; i <= crsource.MaxServedConnections; i++ { // c16 is the youngest, and is left out
		extra = append(extra, teamConnection(objs, "teams", fmt.Sprintf("c%02d", i), created.Add(time.Duration(i)*time.Hour)))
	}
	c := newCluster(t, append(objs, extra...)...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, logs := c.startAdmitted(t, clock, "teams")
	if s, _ := read(t, src); !slices.Equal(unservedNames(s.UnservedConnections), []string{"teams/c16 cap"}) {
		t.Fatalf("unserved connections = %v; want c16", unservedNames(s.UnservedConnections))
	}

	until(t, func() { c.remove(extra[0]) }, func() bool { return src.CachedGenerationForTest("ChatConnection", "teams", "c01") < 0 })
	// c01 is gone, and still served, and so keeps its place: c16 stays out and the cap holds.
	if s, _ := read(t, src); len(s.ChatConnections) != crsource.MaxServedConnections || !slices.Contains(connectionNames(s), "teams/c01") || slices.Contains(connectionNames(s), "teams/c16") {
		t.Errorf("while c01's deletion is deferred: connections %v; want %d, c01 still among them and c16 out", connectionNames(s), crsource.MaxServedConnections)
	}
	clock.advance(8 * time.Second)
	read(t, src) // c01's deletion takes effect, and its place is c16's at the next read
	s, _ := read(t, src)
	if len(s.ChatConnections) != crsource.MaxServedConnections || slices.Contains(connectionNames(s), "teams/c01") || !slices.Contains(connectionNames(s), "teams/c16") || len(s.UnservedConnections) != 0 {
		t.Errorf("after the deletion: connections %v, unserved %v; want c01 gone and c16 served", connectionNames(s), unservedNames(s.UnservedConnections))
	}
	read(t, src)
	if n := strings.Count(logs.String(), "served again"); n != 1 || !strings.Contains(logs.String(), "level=INFO") {
		t.Errorf("served again was said %d times; want once, at Info\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "left out"); n != 1 {
		t.Errorf("left out was said %d times; want once", n)
	}
}

// The own connection's hold is unchanged with an admission list: it is held when deleted, and a team connection's Bindings are not
// held with it.
func TestTheOwnConnectionIsStillHeldWithAnAdmissionList(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	team := teamConnection(objs, "team-a", "slack", created)
	onTeam := bindingOn(objs, "team-a", "on-a", "team-a", "slack")
	c := newCluster(t, append(objs, team, onTeam)...)
	src, _ := c.startAdmitted(t, nil, "team-a")
	set(t, src)
	until(t, func() { c.deleteForRollback(objs) }, func() bool { s, _ := set(t, src); return len(held(s)) == 4 })
	s, _ := set(t, src)
	if got := strings.Join(held(s), " "); got != allHeld {
		t.Errorf("held = %s; want %s", got, allHeld)
	}
	if got := connectionNames(s); !slices.Equal(got, []string{"agent-kourier-system/slack-main", "team-a/slack"}) {
		t.Errorf("connections = %v; want the held own and the team's", got)
	}
}

// A place that a deletion frees is taken by the connection the cap left out at the poll after the one that settled the deletion, not at
// the Reloader's recheck: the read that settles it still counted the deleted connection, and says that it must be read again. The
// Reloader here has an hour between rechecks and polls every few milliseconds, so only the fingerprint can bring the second read.
func TestAPlaceThatADeletionFreesIsTakenAtTheNextPoll(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var extra []*unstructured.Unstructured
	for i := 1; i <= crsource.MaxServedConnections; i++ { // c16 is the youngest, and is left out
		extra = append(extra, teamConnection(objs, "teams", fmt.Sprintf("c%02d", i), created.Add(time.Duration(i)*time.Hour)))
	}
	c := newCluster(t, append(objs, extra...)...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, _ := c.startAdmitted(t, clock, "teams")
	r := config.NewReloader(config.ReloaderConfig{
		Source: src, Secrets: noSecrets{}, PollInterval: 5 * time.Millisecond, SecretsInterval: time.Hour, Logger: slog.New(slog.DiscardHandler), PerObject: true,
	})
	ctx, cancel := context.WithCancel(t.Context())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); r.Wait() })
	serves := func(name string) bool {
		return slices.ContainsFunc(r.Current().ChatConnections(), func(c *v1alpha1.ChatConnection) bool { return c.Namespace == "teams" && c.Name == name })
	}
	if serves("c16") || !serves("c15") {
		t.Fatal("the cap did not leave out c16 and serve c15 at the start")
	}

	until(t, func() { c.remove(extra[0]) }, func() bool { return src.CachedGenerationForTest("ChatConnection", "teams", "c01") < 0 })
	until(t, func() {}, func() bool { return len(r.Report().UnservedConnections) == 1 && serves("c01") }) // the poll has read it: deferred, so c01 still serves
	clock.advance(8 * time.Second)                                                                        // the deletion is due; the next poll settles it
	until(t, func() {}, func() bool { return serves("c16") })
	if serves("c01") || len(r.Report().UnservedConnections) != 0 {
		t.Errorf("c01 served: %v, unserved %+v; want c01 gone and nothing left out", serves("c01"), r.Report().UnservedConnections)
	}
}

// ConnectionBindings answers for any ChatConnection of the cluster, served or not: whether it exists, and the Bindings that name it,
// sorted, the unreadable one too. The broker asks it of a connection it served before and does not now, which no read of the Set lists.
func TestConnectionBindingsNamesTheBindingsOfAnyConnection(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	broken := bindingOn(objs, "payments", "broken-on-c", "team-c", "slack")
	_ = unstructured.SetNestedField(broken.Object, "#general", "spec", "chat", "channel")
	extra := []*unstructured.Unstructured{
		teamConnection(objs, "team-a", "slack", created),
		teamConnection(objs, "team-c", "slack", created), // not admitted
		bindingOn(objs, "team-c", "on-c", "team-c", "slack"),
		bindingOn(objs, "sre", "also-on-c", "team-c", "slack"),
		broken,
		bindingOn(objs, "team-a", "on-a", "team-a", "slack"),
		bindingOn(objs, "sre", "dangling", "team-z", "gone"), // names no connection that exists
	}
	src, _ := newCluster(t, append(objs, extra...)...).startAdmitted(t, nil, "team-a")

	for name, tc := range map[string]struct {
		namespace, name string
		want            []string
		exists          bool
	}{
		"a connection that is not admitted": {"team-c", "slack", []string{"payments/broken-on-c", "sre/also-on-c", "team-c/on-c"}, true},
		"a connection that is served":       {"team-a", "slack", []string{"team-a/on-a"}, true},
		"the own connection":                {"agent-kourier-system", "slack-main", []string{"sre/chat-only", "sre/sre-alerts"}, true},
		"one that does not exist":           {"team-z", "gone", []string{"sre/dangling"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, exists, err := src.ConnectionBindings(tc.namespace, tc.name)
			if err != nil || exists != tc.exists || !slices.Equal(got, tc.want) {
				t.Errorf("ConnectionBindings(%s/%s) = %v, %v, %v; want %v, %v", tc.namespace, tc.name, got, exists, err, tc.want, tc.exists)
			}
		})
	}
}
