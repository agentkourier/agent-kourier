package crsource_test

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
	"github.com/agentkourier/agent-kourier/internal/config/sourcetest"
)

// startTeamOnly is startAdmitted for a broker with no ChatConnection of its own: it serves the connections of the namespaces and
// nothing else.
func (c *cluster) startTeamOnly(t *testing.T, clock *testClock, namespaces ...string) (*crsource.Source, *logSink) {
	t.Helper()
	logs := &logSink{}
	src, err := crsource.New(crsource.Options{Client: c.client, ConnectionNamespaces: namespaces, Logger: slog.New(slog.NewTextHandler(logs, nil))})
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

// A Source may have no own connection: Options.Connection is optional. A reference that names only one of the namespace and the name is
// a mistake and is refused, as it always was.
func TestNewTakesNoOwnConnectionAndRefusesHalfOfOne(t *testing.T) {
	c := newCluster(t)
	if src, err := crsource.New(crsource.Options{Client: c.client, ConnectionNamespaces: []string{"team-a"}}); err != nil {
		t.Errorf("New refused Options with no Connection: %v", err)
	} else {
		src.Stop()
	}
	for name, conn := range map[string]v1alpha1.ObjectRef{
		"a name and no namespace": {Name: "x"},
		"a namespace and no name": {Namespace: "x"},
	} {
		if _, err := crsource.New(crsource.Options{Client: c.client, Connection: conn}); err == nil {
			t.Errorf("%s: New accepted the options", name)
		}
	}
}

// The conformance suite, over a source that has no own connection and serves the corpus' connection as it serves a team's: the
// objects it reads, the fingerprint, the Sets, the invalid objects and the rules across objects are the files' all the same.
func TestConformanceWithoutAnOwnConnection(t *testing.T) {
	sourcetest.Run(t, func(t *testing.T, corpus sourcetest.Corpus) config.Source {
		leakCheck(t)
		var objs []*unstructured.Unstructured
		for i, d := range corpus.Docs {
			objs = append(objs, object(t, d, fmt.Sprintf("uid-%d", i), 1))
		}
		src, _ := newCluster(t, objs...).startTeamOnly(t, nil, corpus.Connection.Namespace)
		return src
	}, sourcetest.Options{PerObject: true, IgnoresUnknownFields: true})
}

// With no own connection the served set is the ChatConnections of the admitted namespaces and no other: the corpus' connection, which
// another broker might have as its own, is served only if its namespace is admitted. A Binding is read when it names a served
// connection or names none that exists; one that names the connection of a namespace that is not admitted is that connection's.
func TestWithoutAnOwnConnectionOnlyTheAdmittedNamespacesAreServed(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	extra := []*unstructured.Unstructured{
		teamConnection(objs, "team-a", "slack", created),
		teamConnection(objs, "team-b", "slack", created),
		bindingOn(objs, "team-a", "on-a", "team-a", "slack"),
		bindingOn(objs, "team-b", "on-b", "team-b", "slack"),
		bindingOn(objs, "sre", "dangling", "team-a", "nowhere"), // names no connection that exists
	}
	src, _ := newCluster(t, append(objs, extra...)...).startTeamOnly(t, nil, "team-a", "team-b")

	s, _ := set(t, src)
	if got, want := connectionNames(s), []string{"team-a/slack", "team-b/slack"}; !slices.Equal(got, want) {
		t.Errorf("connections = %v; want the two admitted, and not slack-main, whose namespace is not", got)
	}
	// sre/chat-only and sre/sre-alerts name slack-main, which exists and is not served: they are its broker's, and not reported here.
	if got, want := bindingNames(s), []string{"sre/dangling", "team-a/on-a", "team-b/on-b"}; !slices.Equal(got, want) {
		t.Errorf("Bindings = %v; want those of the two served connections and the one that names none that exists", got)
	}
	if len(s.Invalid) != 0 || len(s.UnservedConnections) != 0 {
		t.Errorf("Invalid %+v, unserved connections %+v; want nothing", s.Invalid, s.UnservedConnections)
	}
}

// A connection that was an own connection once is served, with no own, when its namespace is admitted, as any team connection is.
func TestWithoutAnOwnConnectionAFormerOwnIsServedWhenItsNamespaceIsAdmitted(t *testing.T) {
	leakCheck(t)
	src, _ := newCluster(t, corpus(t)...).startTeamOnly(t, nil, "agent-kourier-system")
	s, _ := set(t, src)
	if got := connectionNames(s); !slices.Equal(got, []string{"agent-kourier-system/slack-main"}) {
		t.Errorf("connections = %v; want slack-main, served as a team connection", got)
	}
	if got := bindingNames(s); !slices.Equal(got, []string{"sre/chat-only", "sre/sre-alerts"}) {
		t.Errorf("Bindings = %v; want those of slack-main", got)
	}
}

// With nothing admitted and no own connection the Source serves no connection. The broker refuses to start in that case; the Source
// is not the place that says so, and reads nothing.
func TestWithoutAnOwnConnectionAndNoNamespaceNothingIsServed(t *testing.T) {
	leakCheck(t)
	src, _ := newCluster(t, corpus(t)...).startTeamOnly(t, nil)
	s, _ := set(t, src)
	if len(s.ChatConnections) != 0 || len(s.Invalid) != 0 || len(s.UnservedConnections) != 0 {
		t.Errorf("connections %v, Invalid %+v, unserved %+v; want nothing", connectionNames(s), s.Invalid, s.UnservedConnections)
	}
}

// With no own connection all maxServedConnections places go by age: no place is kept for it. The 17th, the youngest, is left out for
// the cap with the Bindings that name it, and a connection of a namespace that is not admitted takes no place.
func TestWithoutAnOwnConnectionAllThePlacesGoByAge(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var extra []*unstructured.Unstructured
	for i := 1; i <= crsource.MaxServedConnections+1; i++ { // c01 is the oldest, c17 the youngest
		name := fmt.Sprintf("c%02d", i)
		extra = append(extra, teamConnection(objs, "teams", name, created.Add(time.Duration(i)*time.Hour)), bindingOn(objs, "teams", "on-"+name, "teams", name))
	}
	extra = append(extra, teamConnection(objs, "elsewhere", "old", created.Add(-time.Hour))) // older than all, and not admitted
	src, logs := newCluster(t, append(objs, extra...)...).startTeamOnly(t, nil, "teams")

	s, _ := set(t, src)
	if got := len(s.ChatConnections); got != crsource.MaxServedConnections {
		t.Errorf("%d connections served; want the cap, %d, all of them team connections", got, crsource.MaxServedConnections)
	}
	if got := connectionNames(s); !slices.Contains(got, "teams/c16") || slices.Contains(got, "teams/c17") || slices.Contains(got, "elsewhere/old") {
		t.Errorf("connections = %v; want c01 to c16 and not c17", got)
	}
	if got := unservedNames(s.UnservedConnections); !slices.Equal(got, []string{"teams/c17 cap"}) {
		t.Errorf("unserved connections = %v; want c17, for the cap", got)
	}
	if got := bindingNames(s); slices.Contains(got, "teams/on-c17") || !slices.Contains(got, "teams/on-c16") || len(got) != crsource.MaxServedConnections {
		t.Errorf("Bindings = %v; want one for each of the %d served connections, not c17's", got, crsource.MaxServedConnections)
	}
	if got := s.UnservedConnections[0].Bindings; !slices.Equal(got, []string{"teams/on-c17"}) {
		t.Errorf("the Bindings of the connection left out = %v; want teams/on-c17", got)
	}
	if n := strings.Count(logs.String(), "left out"); n != 1 || !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "teams/c17") {
		t.Errorf("the connection was said left out %d times; want once, at Warn, naming teams/c17\n%s", n, logs.String())
	}
}

// Connections of one age are told apart by namespace, then name, with no own connection as with one: all 16 places go in that order,
// and the 17th is the last of them.
func TestWithoutAnOwnConnectionConnectionsOfOneAgeAreOrderedByNamespaceThenName(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var extra []*unstructured.Unstructured
	for i := 1; i <= (crsource.MaxServedConnections+2)/2; i++ { // 2 x 9 = 18, of which 16 are served
		for _, ns := range []string{"team-b", "team-a"} {
			extra = append(extra, teamConnection(objs, ns, fmt.Sprintf("c%d", i), created))
		}
	}
	src, _ := newCluster(t, append(objs, extra...)...).startTeamOnly(t, nil, "team-a", "team-b")

	s, _ := set(t, src)
	if len(s.ChatConnections) != crsource.MaxServedConnections {
		t.Fatalf("%d connections served; want %d", len(s.ChatConnections), crsource.MaxServedConnections)
	}
	if got := unservedNames(s.UnservedConnections); !slices.Equal(got, []string{"team-b/c8 cap", "team-b/c9 cap"}) {
		t.Errorf("unserved connections = %v; want team-b/c8 and team-b/c9 (the last two by namespace, then name)", got)
	}
}

// A connection that the cap left out is served when a place opens, with no own connection too: the deletion that frees it takes
// effect, and the left-out connection is the 16th at the read after.
func TestWithoutAnOwnConnectionAConnectionLeftOutByTheCapIsServedWhenAPlaceOpens(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var extra []*unstructured.Unstructured
	for i := 1; i <= crsource.MaxServedConnections+1; i++ { // c17 is the youngest, and is left out
		extra = append(extra, teamConnection(objs, "teams", fmt.Sprintf("c%02d", i), created.Add(time.Duration(i)*time.Hour)))
	}
	c := newCluster(t, append(objs, extra...)...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, _ := c.startTeamOnly(t, clock, "teams")
	if s, _ := read(t, src); !slices.Equal(unservedNames(s.UnservedConnections), []string{"teams/c17 cap"}) {
		t.Fatalf("unserved connections = %v; want c17", unservedNames(s.UnservedConnections))
	}

	until(t, func() { c.remove(extra[0]) }, func() bool { return src.CachedGenerationForTest("ChatConnection", "teams", "c01") < 0 })
	if s, _ := read(t, src); len(s.ChatConnections) != crsource.MaxServedConnections || !slices.Contains(connectionNames(s), "teams/c01") || slices.Contains(connectionNames(s), "teams/c17") {
		t.Errorf("while c01's deletion is deferred: connections %v; want %d, c01 still among them and c17 out", connectionNames(s), crsource.MaxServedConnections)
	}
	clock.advance(8 * time.Second)
	read(t, src) // c01's deletion takes effect, and its place is c17's at the next read
	s, _ := read(t, src)
	if len(s.ChatConnections) != crsource.MaxServedConnections || slices.Contains(connectionNames(s), "teams/c01") || !slices.Contains(connectionNames(s), "teams/c17") || len(s.UnservedConnections) != 0 {
		t.Errorf("after the deletion: connections %v, unserved %v; want c01 gone and c17 served", connectionNames(s), unservedNames(s.UnservedConnections))
	}
}

// With no own connection a deleted ChatConnection is held by nothing, and neither are its Bindings: they stop as any object does, at
// the first read past the floor that finds nothing newly gone, whatever was deleted with them or in what order, and say no word in
// Invalid or in the log. What was the last connection is the last one: the Set is then empty.
func TestWithoutAnOwnConnectionNothingIsHeldWhenAConnectionIsDeleted(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	allowTeam(objs, "team-a")
	team := teamConnection(objs, "team-a", "slack", created, "team-a")
	onTeam := bindingOn(objs, "team-a", "on-a", "team-a", "slack")
	c := newCluster(t, append(objs, team, onTeam)...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, logs := c.startTeamOnly(t, clock, "team-a")
	if s, _ := read(t, src); !slices.Equal(connectionNames(s), []string{"team-a/slack"}) || !slices.Equal(bindingNames(s), []string{"team-a/on-a"}) {
		t.Fatalf("connections %v, Bindings %v; want the team's", connectionNames(s), bindingNames(s))
	}

	// What a rollback of the team's release does: the Binding, then the connection.
	until(t, func() { c.remove(onTeam); c.remove(team) }, func() bool {
		return src.CachedGenerationForTest("ChatConnection", "team-a", "slack") < 0 && src.CachedGenerationForTest("Binding", "team-a", "on-a") < 0
	})
	s, fp := read(t, src)
	if !slices.Equal(connectionNames(s), []string{"team-a/slack"}) || len(s.Invalid) != 0 {
		t.Errorf("at the first read that finds them gone: connections %v, Invalid %+v; want them served, with no word of it", connectionNames(s), s.Invalid)
	}
	clock.advance(8 * time.Second)
	if src.Fingerprint(t.Context()) == fp {
		t.Error("the Fingerprint does not say that the deletion is due")
	}
	s, _ = read(t, src)
	if len(s.ChatConnections) != 0 || len(s.Bindings) != 0 || len(s.Invalid) != 0 || len(held(s)) != 0 {
		t.Errorf("at the first read past the floor: connections %v, Bindings %v, Invalid %+v; want nothing served and nothing held",
			connectionNames(s), bindingNames(s), s.Invalid)
	}
	clock.advance(2 * time.Minute)
	if s, _ := read(t, src); len(s.ChatConnections) != 0 || len(s.Bindings) != 0 || len(s.Invalid) != 0 {
		t.Errorf("after the grace: connections %v, Bindings %v, Invalid %+v; want nothing", connectionNames(s), bindingNames(s), s.Invalid)
	}
	if strings.Contains(logs.String(), "held") {
		t.Errorf("the log speaks of a hold:\n%s", logs.String())
	}
}

// A connection of a namespace that is not admitted is not the broker's, so deleting it is nothing to the broker, and no deletion of the
// cluster's brings the broker to hold anything: the corpus is deleted the way Helm deletes it and the Set is what it was.
func TestWithoutAnOwnConnectionTheDeletionOfAnotherBrokersObjectsHoldsNothing(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	team := teamConnection(objs, "team-a", "slack", created)
	c := newCluster(t, append(objs, team)...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, _ := c.startTeamOnly(t, clock, "team-a")
	read(t, src)

	until(t, func() { c.deleteForRollback(objs) }, func() bool {
		return src.CachedGenerationForTest("ChatConnection", "agent-kourier-system", "slack-main") < 0
	})
	read(t, src)
	clock.advance(8 * time.Second)
	s, _ := read(t, src)
	if !slices.Equal(connectionNames(s), []string{"team-a/slack"}) || len(s.Invalid) != 0 || len(s.Bindings) != 0 {
		t.Errorf("connections %v, Bindings %v, Invalid %+v; want the team's connection alone and nothing held", connectionNames(s), bindingNames(s), s.Invalid)
	}
}
