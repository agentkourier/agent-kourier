package broker

import (
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// agedConnections resolves a snapshot of ChatConnections, each with the creation time given (seconds, 0 for none).
func agedConnections(t *testing.T, ages map[string]int64) *config.Snapshot {
	t.Helper()
	set := &config.Set{}
	for n, age := range ages {
		ns, name, _ := strings.Cut(n, "/")
		meta := metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name), Generation: 3}
		if age > 0 {
			meta.CreationTimestamp = metav1.NewTime(time.Unix(age, 0))
		}
		set.ChatConnections = append(set.ChatConnections, v1alpha1.ChatConnection{
			ObjectMeta: meta,
			Spec:       v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformSlack, CredentialsSecretRef: v1alpha1.SecretRef{Name: "creds-" + name}},
		})
	}
	snap, err := config.Resolve(t.Context(), set, keys{})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

var ownRef = v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}

// ChooseConnections returns the own connection and the team connections of the snapshot, oldest first, each with its own tokens. A team
// connection whose Secret cannot be read comes back with the reason and no tokens, and fails nothing.
func TestChooseConnectionsReturnsTheTeamConnectionsOldestFirst(t *testing.T) {
	r := &reader{vals: map[string]string{
		"platform/creds-slack/botToken": "xoxb-own", "platform/creds-slack/appToken": "xapp-own",
		"team-a/creds-slack/botToken": "xoxb-a", "team-a/creds-slack/appToken": "xapp-a",
		"team-c/creds-slack/botToken": "xoxb-c", "team-c/creds-slack/appToken": "xapp-c",
		// team-b's Secret is not there: its informer has not synced it
	}}
	snap := agedConnections(t, map[string]int64{"platform/slack": 500, "team-a/slack": 300, "team-b/slack": 100, "team-c/slack": 100})

	got, err := ChooseConnections(t.Context(), snap, ownRef, r)

	if err != nil {
		t.Fatalf("ChooseConnections = %v; want a team connection's Secret never to fail it", err)
	}
	if got.Own.Ref != ownRef || got.Own.Credentials["botToken"].Reveal() != "xoxb-own" {
		t.Errorf("Own = %+v; want platform/slack with its tokens", got.Own.Ref)
	}
	var names []string
	for _, c := range got.Team {
		names = append(names, c.Name())
	}
	if want := "team-b/slack team-c/slack team-a/slack"; strings.Join(names, " ") != want {
		t.Fatalf("Team = %v; want %s: oldest first, then by namespace and name", names, want)
	}
	if b := got.Team[0]; b.Err == nil || len(b.Credentials) != 0 || b.Ref.Name != "slack" || b.Generation != 3 {
		t.Errorf("team-b = %+v; want its reason and no tokens, still named", b)
	}
	if a := got.Team[2]; a.Err != nil || a.Credentials["botToken"].Reveal() != "xoxb-a" || a.UID != "uid-slack" {
		t.Errorf("team-a = %+v; want its own tokens", a)
	}
}

// The own connection's failures are the broker's and unchanged: it is a wiring error when it is missing or its Secret lacks a token, and
// the files route, with no own, keeps exactly one connection and has no team.
func TestChooseConnectionsKeepsTheFailuresOfTheOwnConnection(t *testing.T) {
	r := &reader{vals: map[string]string{"team-a/creds-slack/botToken": "xoxb-a", "team-a/creds-slack/appToken": "xapp-a"}}

	_, err := ChooseConnections(t.Context(), agedConnections(t, map[string]int64{"platform/slack": 1, "team-a/slack": 2}), ownRef, r)
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "platform/creds-slack") {
		t.Errorf("own Secret missing: ChooseConnections = %v; want ErrWiring naming the Secret", err)
	}
	_, err = ChooseConnections(t.Context(), agedConnections(t, map[string]int64{"team-a/slack": 2}), ownRef, r)
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "platform/slack") {
		t.Errorf("own missing: ChooseConnections = %v; want ErrWiring naming it", err)
	}
	_, err = ChooseConnections(t.Context(), agedConnections(t, map[string]int64{"platform/a": 1, "platform/b": 2}), v1alpha1.ObjectRef{}, r)
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("files route with two: ChooseConnections = %v; want ErrWiring saying exactly one", err)
	}
	r.set("platform/creds-a/botToken", "xoxb-a")
	r.set("platform/creds-a/appToken", "xapp-a")
	got, err := ChooseConnections(t.Context(), agedConnections(t, map[string]int64{"platform/a": 1}), v1alpha1.ObjectRef{}, r)
	if err != nil || len(got.Team) != 0 {
		t.Errorf("files route with one: ChooseConnections = %+v, %v; want it and no team connection", got.Team, err)
	}
}

// A broker with no own connection serves every ChatConnection of the snapshot as a team connection, oldest first (the order that decides which of
// two connections of one Slack app is served), and has no failure of its own to report: a connection whose Secret cannot be read comes back with its
// Err, as a team connection does beside an own one.
func TestChooseTeamConnectionsWithNoOwnReturnsEveryConnectionOldestFirst(t *testing.T) {
	r := &reader{vals: map[string]string{
		"team-a/creds-slack/botToken": "xoxb-a", "team-a/creds-slack/appToken": "xapp-a",
		"team-c/creds-slack/botToken": "xoxb-c", "team-c/creds-slack/appToken": "xapp-c",
		// team-b's Secret is not there: its informer has not synced it
	}}
	snap := agedConnections(t, map[string]int64{"team-a/slack": 300, "team-b/slack": 100, "team-c/slack": 100})

	got := ChooseTeamConnections(t.Context(), snap, r)

	if got.Own != nil {
		t.Errorf("Own = %+v; want none: the broker has no connection of its own", got.Own)
	}
	var names []string
	for _, c := range got.Team {
		names = append(names, c.Name())
	}
	if want := "team-b/slack team-c/slack team-a/slack"; strings.Join(names, " ") != want {
		t.Fatalf("Team = %v; want %s: every connection, oldest first, then by namespace and name", names, want)
	}
	if b := got.Team[0]; b.Err == nil || len(b.Credentials) != 0 {
		t.Errorf("team-b = %+v; want its Secret's error and no tokens", b)
	}
	if c := got.Team[1]; c.Err != nil || c.Credentials["botToken"].Reveal() != "xoxb-c" {
		t.Errorf("team-c = %+v; want its own tokens", c)
	}
	if none := ChooseTeamConnections(t.Context(), nil, r); none.Own != nil || len(none.Team) != 0 {
		t.Errorf("ChooseTeamConnections(nil snapshot) = %+v; want none", none)
	}
}

// A ChatConnection on a platform with no adapter is a wiring error of its own, and a team connection's is its Err and no more.
func TestAChatConnectionOnAPlatformWithNoAdapterIsAWiringError(t *testing.T) {
	c := &v1alpha1.ChatConnection{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "slack"}, Spec: v1alpha1.ChatConnectionSpec{Platform: "teams"}}
	if _, err := connectionOf(t.Context(), c, &reader{vals: map[string]string{}}); !errors.Is(err, ErrWiring) {
		t.Errorf("connectionOf = %v; want ErrWiring", err)
	}
}

// The fingerprint of a team connection changes with its generation, its object and the content of its credentials, and with nothing else, so
// that the supervisor restarts a runtime for exactly those. It does not hold a token in a form that prints.
func TestATeamConnectionsFingerprintFollowsItsGenerationObjectAndTokens(t *testing.T) {
	base := team("team-a/slack", 1, "tok", 1)
	if base.fingerprint() != team("team-a/slack", 1, "tok", 9).fingerprint() {
		t.Error("the fingerprint changed with the age; want it not to")
	}
	for name, other := range map[string]TeamConnection{
		"generation": team("team-a/slack", 2, "tok", 1),
		"token":      team("team-a/slack", 1, "tok2", 1),
		"name":       team("team-b/slack", 1, "tok", 1),
	} {
		if base.fingerprint() == other.fingerprint() {
			t.Errorf("the fingerprint did not change with the %s", name)
		}
	}
	uid := base
	uid.UID = "another"
	if base.fingerprint() == uid.fingerprint() {
		t.Error("the fingerprint did not change with the object (a deleted and made again ChatConnection)")
	}
	if strings.Contains(base.fingerprint(), "tok") {
		t.Error("the fingerprint holds the token")
	}
}
