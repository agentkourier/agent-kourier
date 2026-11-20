package broker

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/fakevendors"
)

// ownWebhookSnapshot is a snapshot whose one ChatConnection is the webhook connection ns/github with spec, as a reload reads it.
func ownWebhookSnapshot(t *testing.T, spec *v1alpha1.WebhookSpec) *config.Snapshot {
	t.Helper()
	set := &config.Set{ChatConnections: []v1alpha1.ChatConnection{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "github"},
		Spec: v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWebhook, CredentialsSecretRef: v1alpha1.SecretRef{Name: "github-webhook"},
			Webhook: spec},
	}}}
	snap, err := config.Resolve(t.Context(), set, keysOf{v1alpha1.WebhookSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// ownWebhookWorld is a supervisor whose own connection is the webhook connection ns/github, running on spec.
func ownWebhookWorld(t *testing.T, spec *v1alpha1.WebhookSpec) *supWorld {
	t.Helper()
	w := newSupWorld(t)
	own := webhookConn("github", cloneWebhook(spec), fakeWebhookSecret)
	w.sup.own = &own
	return w
}

// count is how many lines of the supervisor's log hold msg at level.
func (w *supWorld) count(level, msg string) int {
	n := 0
	for line := range strings.Lines(w.logs.String()) {
		if strings.Contains(line, "level="+level) && strings.Contains(line, `msg="`+msg+`"`) {
			n++
		}
	}
	return n
}

// An edit of the own webhook connection's spec.webhook that a reload brought is one WARN that names the connection, while the running
// platform keeps the spec it was built with: the same edit at the next look says nothing again, another edit is a WARN of its own, and a
// spec that is back to the running one is one INFO, after which the first edit, made again, is a drift again.
func TestSupervisorWarnsOnceOfAnOwnWebhookSpecThatDriftedFromTheRunningOne(t *testing.T) {
	running := webhookSpec()
	w := ownWebhookWorld(t, running)
	look := func(spec *v1alpha1.WebhookSpec) { w.sup.checkOwnWebhook(w.t.Context(), ownWebhookSnapshot(t, spec)) }
	added := func() *v1alpha1.WebhookSpec { // a field a new trigger may match on
		s := webhookSpec()
		s.Fields = append(s.Fields, v1alpha1.WebhookField{Name: "draft", From: []string{"/pull_request/draft"}})
		return s
	}
	moved := func() *v1alpha1.WebhookSpec {
		s := webhookSpec()
		s.Fields[0].From = []string{"/pull_request/number"}
		return s
	}
	steps := []struct {
		what        string
		spec        *v1alpha1.WebhookSpec
		warns, back int
	}{
		{"the running spec", webhookSpec(), 0, 0},
		{"a field added", added(), 1, 0},
		{"the same edit at the next look", added(), 1, 0},
		{"another edit", moved(), 2, 0},
		{"the other edit again", moved(), 2, 0},
		{"the running spec again", webhookSpec(), 2, 1},
		{"the running spec at the next look", webhookSpec(), 2, 1},
		{"the first edit made again", added(), 3, 1},
	}
	for _, st := range steps {
		look(st.spec)
		if got := w.count("WARN", ownWebhookDrifted); got != st.warns {
			t.Fatalf("after %s: %d drift WARNs; want %d\n%s", st.what, got, st.warns, w.logs.String())
		}
		if got := w.count("INFO", ownWebhookMatches); got != st.back {
			t.Fatalf("after %s: %d INFOs that the spec matches again; want %d\n%s", st.what, got, st.back, w.logs.String())
		}
	}
	for line := range strings.Lines(w.logs.String()) {
		if strings.Contains(line, ownWebhookDrifted) && !strings.Contains(line, "connection=ns/github") {
			t.Errorf("the WARN does not name the connection: %s", line)
		}
	}
	if strings.Contains(w.logs.String(), fakeWebhookSecret) {
		t.Error("the log holds the connection's secret")
	}
	if !reflect.DeepEqual(w.sup.own.Webhook, running) {
		t.Error("the look changed the running connection's spec.webhook")
	}
}

// Nothing is said of a connection that is not the own webhook connection: an own Slack connection (even one of the name of the snapshot's
// webhook connection), a broker with no own connection, and a snapshot that does not hold the own connection (on the files a directory whose
// one connection was renamed; a connection of its name in another namespace is not it) have no running spec.webhook to differ from.
func TestSupervisorSaysNothingOfASpecWebhookThatIsNotTheOwnRunningOne(t *testing.T) {
	edited := webhookSpec()
	edited.Fields = edited.Fields[:1]
	cases := map[string]func(t *testing.T) (*supWorld, *config.Snapshot){
		"an own Slack connection of the same name": func(t *testing.T) (*supWorld, *config.Snapshot) {
			w := newSupWorld(t)
			w.sup.own.Ref = v1alpha1.ObjectRef{Namespace: "ns", Name: "github"} // no spec.webhook, over a snapshot whose ns/github has one
			return w, ownWebhookSnapshot(t, edited)
		},
		"no own connection": func(t *testing.T) (*supWorld, *config.Snapshot) {
			return newTeamOnlySupWorld(t), ownWebhookSnapshot(t, edited)
		},
		"the own connection not in the snapshot": func(t *testing.T) (*supWorld, *config.Snapshot) {
			w := newSupWorld(t)
			own := webhookConn("gitlab", webhookSpec(), fakeWebhookSecret)
			w.sup.own = &own
			return w, ownWebhookSnapshot(t, edited)
		},
		"a connection of the same name in another namespace": func(t *testing.T) (*supWorld, *config.Snapshot) {
			w := ownWebhookWorld(t, webhookSpec())
			w.sup.own.Ref.Namespace = "other"
			return w, ownWebhookSnapshot(t, edited)
		},
		"no snapshot": func(t *testing.T) (*supWorld, *config.Snapshot) {
			return ownWebhookWorld(t, webhookSpec()), nil
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w, snap := c(t)
			w.sup.checkOwnWebhook(t.Context(), snap)
			if got := w.count("WARN", ownWebhookDrifted); got != 0 {
				t.Errorf("%d drift WARNs; want none\n%s", got, w.logs.String())
			}
		})
	}
}

// An own webhook connection whose ChatConnection a reload moved to another platform, which has no spec.webhook, runs on the webhook
// platform still: that is a drift too, said once.
func TestSupervisorWarnsOnceOfAnOwnWebhookConnectionWhoseSpecWebhookIsGone(t *testing.T) {
	w := ownWebhookWorld(t, webhookSpec())
	gone, err := config.Resolve(t.Context(), &config.Set{ChatConnections: []v1alpha1.ChatConnection{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "github"},
		Spec:       v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformSlack, CredentialsSecretRef: v1alpha1.SecretRef{Name: "github-webhook"}},
	}}}, keys{})
	if err != nil {
		t.Fatal(err)
	}
	w.sup.checkOwnWebhook(t.Context(), gone)
	w.sup.checkOwnWebhook(t.Context(), gone)
	if got := w.count("WARN", ownWebhookDrifted); got != 1 {
		t.Errorf("%d drift WARNs for a spec.webhook that is gone, looked at twice; want 1\n%s", got, w.logs.String())
	}
}

// logged says whether logs hold a line of level with msg that names the connection conn.
func logged(logs, level, msg, conn string) bool {
	for line := range strings.Lines(logs) {
		if strings.Contains(line, "level="+level) && strings.Contains(line, `msg="`+msg+`"`) && strings.Contains(line, "connection="+conn) {
			return true
		}
	}
	return false
}

// drifted says whether logs hold the drift WARN of the connection conn.
func drifted(logs, conn string) bool { return logged(logs, "WARN", ownWebhookDrifted, conn) }

// matchesAgain says whether logs hold the INFO that the spec.webhook of the connection conn is the running one again.
func matchesAgain(logs, conn string) bool { return logged(logs, "INFO", ownWebhookMatches, conn) }

// On the custom resources an edit of the own webhook connection's ChatConnection that adds a field to spec.webhook reaches the broker at the
// next reload, and the leader WARNs that the running connection still uses the spec.webhook it started with. The edit taken back is the
// INFO that the spec matches again: the spec.webhook that the informer's object decodes to is the one the platform was built with, so the
// object as it was is no drift.
func TestRunWarnsOfAnEditOfTheOwnWebhookConnectionOnTheCustomResources(t *testing.T) {
	w := newConnWorld(t, nil)
	runsAlongside(&w.c)
	w.c.ChatListener = someListener(t)
	w.c.ChatConnection = v1alpha1.ObjectRef{Namespace: whNamespace, Name: "github"}
	if _, err := w.kube.CoreV1().Secrets(whNamespace).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: whNamespace, Name: "github-webhook"},
		Data: map[string][]byte{v1alpha1.WebhookSecretKey: []byte(whSigningSecret)}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var conn *unstructured.Unstructured
	for _, obj := range recipeObjects(t, whRecipe{vendor: fakevendors.GitHub}.dir()) {
		obj.SetUID(types.UID("uid-" + obj.GetName()))
		obj.SetGeneration(1)
		gvr := map[string]schema.GroupVersionResource{"ChatConnection": crConnections, "Binding": crBindings, "AgentBackend": crBackends}[obj.GetKind()]
		if err := w.dyn.Tracker().Create(gvr, obj, whNamespace); err != nil {
			t.Fatal(err)
		}
		if obj.GetKind() == "ChatConnection" {
			conn = obj
		}
	}
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)
	if drifted(w.logs.String(), whNamespace+"/github") {
		t.Fatalf("a drift WARN before any edit:\n%s", w.logs.String())
	}

	original := conn.DeepCopy()
	fields, _, _ := unstructured.NestedSlice(conn.Object, "spec", "webhook", "fields")
	fields = append(fields, map[string]any{"name": "merged", "from": []any{"/pull_request/merged"}})
	if err := unstructured.SetNestedSlice(conn.Object, fields, "spec", "webhook", "fields"); err != nil {
		t.Fatal(err)
	}
	conn.SetGeneration(2)
	if err := w.dyn.Tracker().Update(crConnections, conn, whNamespace); err != nil {
		t.Fatal(err)
	}
	w.eventually("the drift WARN of the own connection", func() bool { return drifted(w.logs.String(), whNamespace+"/github") })

	original.SetGeneration(3)
	if err := w.dyn.Tracker().Update(crConnections, original, whNamespace); err != nil {
		t.Fatal(err)
	}
	w.eventually("the INFO that the own connection's spec.webhook matches again", func() bool {
		return matchesAgain(w.logs.String(), whNamespace+"/github")
	})
	if strings.Contains(w.logs.String(), whSigningSecret) {
		t.Error("the log holds the connection's secret")
	}
}

// On the files the same edit, in the directory's one ChatConnection, is the same WARN: the chart rolls the pod on it, but not when its ConfigMap
// is not the chart's or rolling is off, and the broker runs on the spec.webhook it started with until then. The file put back is the INFO that
// the spec matches again: the file read again decodes to the spec.webhook the platform was built with.
func TestRunWarnsOfAnEditOfTheOwnWebhookConnectionOnTheFiles(t *testing.T) {
	rc := whRecipe{vendor: fakevendors.GitHub}
	logs := &syncBuffer{}
	c := startConfig(t)
	runsAlongside(&c)
	c.ChatListener = someListener(t)
	c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	c.superviseEvery, c.reloadPoll = 10*time.Millisecond, 20*time.Millisecond
	c.ConfigDir = recipeConfigDir(t, rc, "http://agent.invalid")
	c.Client = fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: whNamespace, Name: "github-webhook"},
		Data: map[string][]byte{v1alpha1.WebhookSecretKey: []byte(whSigningSecret)}})
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	if drifted(logs.String(), rc.connection()) {
		t.Fatalf("a drift WARN before any edit:\n%s", logs.String())
	}

	path := filepath.Join(c.ConfigDir, "chatconnection.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(slices.Clone(b), []byte("      - {name: merged, from: [/pull_request/merged]}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the drift WARN of the own connection", func() bool { return drifted(logs.String(), rc.connection()) },
		testutil.Within(40*time.Second), testutil.Every(5*time.Millisecond))

	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the INFO that the own connection's spec.webhook matches again", func() bool { return matchesAgain(logs.String(), rc.connection()) },
		testutil.Within(40*time.Second), testutil.Every(5*time.Millisecond))
}
