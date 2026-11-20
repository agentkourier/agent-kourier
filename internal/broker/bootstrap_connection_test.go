package broker

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// filesSnapshot is a snapshot of webhook ChatConnections, each "namespace/name" of names, with spec.webhook identity id, as a reload of
// the files resolves it.
func filesSnapshot(t *testing.T, id string, names ...string) *config.Snapshot {
	t.Helper()
	set := &config.Set{}
	for _, n := range names {
		ns, name, _ := strings.Cut(n, "/")
		set.ChatConnections = append(set.ChatConnections, v1alpha1.ChatConnection{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWebhook, CredentialsSecretRef: v1alpha1.SecretRef{Name: "creds"},
				Webhook: &v1alpha1.WebhookSpec{Identity: id}},
		})
	}
	snap, err := config.Resolve(t.Context(), set, keysOf{v1alpha1.WebhookSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// A reload on the files is accepted when its directory holds the one ChatConnection that the snapshot it replaces holds, edited or not,
// and the first load is left to the start's own check. A directory that renamed the connection, added a second or took it away is refused
// with an error that names the connection the broker started with and the ones the directory holds.
func TestAReloadOnTheFilesKeepsTheConnectionItStartedWith(t *testing.T) {
	started := filesSnapshot(t, "bot", "ns/github")
	cases := []struct {
		name    string
		current *config.Snapshot
		next    *config.Snapshot
		want    string // the error's text, or empty for none
	}{
		{"the first load", nil, filesSnapshot(t, "bot", "ns/github", "ns/github2"), ""},
		{"the same connection", started, filesSnapshot(t, "bot", "ns/github"), ""},
		{"the same connection edited", started, filesSnapshot(t, "another-bot", "ns/github"), ""},
		{"renamed", started, filesSnapshot(t, "bot", "ns/github2"),
			"the directory holds the ChatConnections [ns/github2], and the files route serves the ChatConnection it started with, ns/github, until the broker restarts"},
		{"moved to another namespace", started, filesSnapshot(t, "bot", "other/github"),
			"the directory holds the ChatConnections [other/github], and the files route serves the ChatConnection it started with, ns/github, until the broker restarts"},
		{"a second added", started, filesSnapshot(t, "bot", "ns/github", "ns/github2"),
			"the directory holds the ChatConnections [ns/github, ns/github2], and the files route serves the ChatConnection it started with, ns/github, until the broker restarts"},
		{"taken away", started, filesSnapshot(t, "bot"),
			"the directory holds the ChatConnections [], and the files route serves the ChatConnection it started with, ns/github, until the broker restarts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := keepStartedConnection(tc.current, tc.next)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("keepStartedConnection = %v; want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("keepStartedConnection = %v; want %q", err, tc.want)
			}
		})
	}
}
