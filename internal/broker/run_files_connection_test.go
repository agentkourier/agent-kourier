package broker

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/fakevendors"
)

// filesSuperviseEvery is how often the supervisor of these tests looks at the team connections.
const filesSuperviseEvery = 10 * time.Millisecond

// filesBroker is a broker of these tests, ready: its config directory, its log, and a read of its supervisor's count of passes.
type filesBroker struct {
	dir    string
	logs   *syncBuffer
	passes atomic.Pointer[func() int64]
}

// filesConnectionBroker runs the GitHub webhook recipe on the files route, its ChatConnection acme-webhooks/github the broker's own, and
// returns it once it is ready.
func filesConnectionBroker(t *testing.T) *filesBroker {
	t.Helper()
	rc := whRecipe{vendor: fakevendors.GitHub}
	logs := &syncBuffer{}
	b := &filesBroker{logs: logs}
	c := startConfig(t)
	runsAlongside(&c)
	c.supervisorStarted = func(passes func() int64) { b.passes.Store(&passes) }
	c.ChatListener = someListener(t)
	c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	c.superviseEvery, c.reloadPoll = filesSuperviseEvery, 20*time.Millisecond
	c.ConfigDir = recipeConfigDir(t, rc, "http://agent.invalid")
	c.Client = fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: whNamespace, Name: "github-webhook"},
		Data: map[string][]byte{v1alpha1.WebhookSecretKey: []byte(whSigningSecret)}})
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	b.dir = c.ConfigDir
	return b
}

// rewrite replaces from with to in the file name of dir, and fails the test if from is not there exactly once.
func rewrite(t *testing.T, dir, name, from, to string) {
	t.Helper()
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), from); n != 1 {
		t.Fatalf("%s holds %q %d times; want once", name, from, n)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), from, to, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// refusedFor says whether logs hold the failed reload of a directory whose ChatConnections are conns, the one the broker started with being
// acme-webhooks/github.
func refusedFor(logs, conns string) bool {
	for line := range strings.Lines(logs) {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, `msg="config reload failed; keeping the last good snapshot"`) &&
			strings.Contains(line, "the directory holds the ChatConnections ["+conns+"]") &&
			strings.Contains(line, "the files route serves the ChatConnection it started with, acme-webhooks/github, until the broker restarts") {
			return true
		}
	}
	return false
}

// served says whether logs say the broker serves conn, as the supervisor or the chat listener say it.
func served(logs, conn string) bool {
	for line := range strings.Lines(logs) {
		if (strings.Contains(line, `msg="serving a ChatConnection"`) || strings.Contains(line, `msg="the chat listener serves the connection"`)) &&
			slices.Contains(strings.Fields(line), "connection="+conn) {
			return true
		}
	}
	return false
}

// letTheSupervisorLook waits for the supervisor to have begun and ended a few passes from now, each of which reads the config as it is
// then, so that a connection that a pass would start has been started.
func (b *filesBroker) letTheSupervisorLook(t *testing.T) {
	t.Helper()
	var read func() int64
	testutil.Eventually(t, "the supervisor's start", func() bool {
		if p := b.passes.Load(); p != nil {
			read = *p
		}
		return read != nil
	}, testutil.Within(40*time.Second), testutil.Every(5*time.Millisecond))
	from := read()
	testutil.Eventually(t, "three passes of the supervisor", func() bool { return read() >= from+3 },
		testutil.Within(40*time.Second), testutil.Every(5*time.Millisecond))
}

// On the files route a reload that renames the one ChatConnection, its Binding following it, is refused: the broker keeps the last good
// config and the connection it started with, and serves no other beside it.
func TestRunOnTheFilesRefusesAReloadThatRenamesTheConnection(t *testing.T) {
	b := filesConnectionBroker(t)
	dir, logs := b.dir, b.logs
	if !served(logs.String(), "acme-webhooks/github") {
		t.Fatalf("the own connection is not served:\n%s", logs.String())
	}
	rewrite(t, dir, "binding.yaml", "connectionRef: {name: github}", "connectionRef: {name: github2}")
	rewrite(t, dir, "chatconnection.yaml", "name: github, namespace: acme-webhooks", "name: github2, namespace: acme-webhooks")
	testutil.Eventually(t, "the refusal of the renamed connection", func() bool { return refusedFor(logs.String(), "acme-webhooks/github2") },
		testutil.Within(40*time.Second), testutil.Every(5*time.Millisecond))
	b.letTheSupervisorLook(t)
	if served(logs.String(), "acme-webhooks/github2") {
		t.Fatalf("the renamed connection is served beside the one the broker started with:\n%s", logs.String())
	}
}

// On the files route a reload that adds a second ChatConnection file is refused, and the second is not served; the directory put back
// is loaded again.
func TestRunOnTheFilesRefusesAReloadThatAddsASecondConnection(t *testing.T) {
	b := filesConnectionBroker(t)
	dir, logs := b.dir, b.logs
	conn, err := os.ReadFile(filepath.Join(dir, "chatconnection.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "chatconnection2.yaml")
	if err := os.WriteFile(second, []byte(strings.Replace(string(conn), "name: github,", "name: github2,", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the refusal of the second connection", func() bool {
		return refusedFor(logs.String(), "acme-webhooks/github, acme-webhooks/github2")
	}, testutil.Within(40*time.Second), testutil.Every(5*time.Millisecond))
	b.letTheSupervisorLook(t)
	if served(logs.String(), "acme-webhooks/github2") {
		t.Fatalf("the second connection is served beside the one the broker started with:\n%s", logs.String())
	}

	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the reload of the directory put back", func() bool {
		return strings.Contains(logs.String(), `msg="config reloaded after failed reloads"`)
	}, testutil.Within(40*time.Second), testutil.Every(5*time.Millisecond))
}
