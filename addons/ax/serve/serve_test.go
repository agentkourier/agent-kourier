package serve

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/agentkourier/agent-kourier/addons/ax/config"
)

const token = "fake-token-alpha"

// echo answers every turn at once.
type echo struct{}

func (echo) Name() string { return "echo" }
func (echo) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "done"}}}, TurnComplete: true}, nil)
	}
}

// running is an agent Run serves in the background, at base.
type running struct {
	base string
	stop func() error
}

// run starts Run with the minimal settings plus the overrides, on a loopback port of its own, with a token file and a
// SQLite file in a temporary directory, and waits until it listens.
func run(t *testing.T, overrides map[string]string) *running {
	t.Helper()
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tokens, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"MODEL_REGION": "us-east-1", "MODEL_NAME": "fake-model", "LISTEN_ADDR": "127.0.0.1:0",
		"AUTH_TOKEN_FILE": tokens, "SQLITE_PATH": filepath.Join(dir, "state", "ax.db"),
	}
	for k, v := range overrides {
		env[k] = v
	}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	listening := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, echo{}, Options{Version: "9.9.9-test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Listening: func(a net.Addr) { listening <- a },
			Routes: func(mux *http.ServeMux) {
				mux.HandleFunc("/_extra", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
			}})
	}()
	var stopped error
	stop := once(func() error { cancel(); stopped = <-done; return stopped })
	t.Cleanup(func() { _ = stop() })
	select {
	case a := <-listening:
		r := &running{base: "http://" + a.String(), stop: stop}
		// Until its takeover is done the agent answers the A2A endpoint and the card 503.
		waitFor(t, "the takeover", func() bool {
			c, _ := probe(http.MethodGet, r.base+"/a2a/.well-known/agent-card.json", token)
			return c != http.StatusServiceUnavailable
		})
		return r
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not listen")
	}
	return nil
}

// once makes fn run once, every later call returning its first result.
func once(fn func() error) func() error {
	var ran bool
	var err error
	return func() error {
		if !ran {
			ran, err = true, fn()
		}
		return err
	}
}

func get(t *testing.T, url string, header map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

type bearer struct{ base http.RoundTripper }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return b.base.RoundTrip(r)
}

// Standalone, the agent answers its own readiness and has no metadata proxy; the A2A endpoint and the card are behind
// the token check, the card names the version, and a turn runs.
func TestStandalone(t *testing.T) {
	r := run(t, nil)
	auth := map[string]string{"Authorization": "Bearer " + token}
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/metadata/x": 404, "/_extra": 418} {
		if got, _ := get(t, r.base+path, nil); got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
	if got, _ := get(t, r.base+"/a2a/.well-known/agent-card.json", nil); got != http.StatusUnauthorized {
		t.Errorf("the card without a token = %d, want 401", got)
	}
	status, body := get(t, r.base+"/a2a/.well-known/agent-card.json", auth)
	var card a2a.AgentCard
	if status != http.StatusOK || json.Unmarshal([]byte(body), &card) != nil || card.Version != "9.9.9-test" {
		t.Fatalf("card = %d %s", status, body)
	}
	if card.SupportedInterfaces[0].URL != "http://127.0.0.1:0/a2a" {
		t.Errorf("card URL = %s, want PUBLIC_URL's default", card.SupportedInterfaces[0].URL)
	}
	card.SupportedInterfaces[0].URL = r.base + "/a2a"
	client, err := a2aclient.NewFromCard(t.Context(), &card, a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: bearer{http.DefaultTransport}}))
	if err != nil {
		t.Fatal(err)
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg})
	task, ok := res.(*a2a.Task)
	if err != nil || !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("turn = %+v, %v", res, err)
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run after its context ended: %v", err)
	}
}

// Beside the AX task runner, /readyz and /metadata/ are the runner's.
func TestBesideTheRunner(t *testing.T) {
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/readyz":
			http.Error(w, "runner not ready", http.StatusServiceUnavailable)
		case strings.HasPrefix(r.URL.Path, "/metadata/"):
			_, _ = io.WriteString(w, "metadata "+r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer runner.Close()
	r := run(t, map[string]string{"RUNNER_URL": runner.URL})
	if got, body := get(t, r.base+"/readyz", nil); got != http.StatusServiceUnavailable || !strings.Contains(body, "runner not ready") {
		t.Errorf("/readyz = %d %q, want the runner's 503", got, body)
	}
	if got, body := get(t, r.base+"/metadata/task", nil); got != http.StatusOK || body != "metadata /metadata/task" {
		t.Errorf("/metadata/task = %d %q, want the runner's", got, body)
	}
	if got, _ := get(t, r.base+"/healthz", nil); got != http.StatusOK {
		t.Errorf("/healthz = %d, want the agent's 200", got)
	}
}

// A standalone agent that is not ready says so.
func TestMuxReadiness(t *testing.T) {
	mux, err := Mux(http.NotFoundHandler(), "", func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503", rec.Code)
	}
}

// A setting Run cannot use stops it before it listens.
func TestRunRefusesAnUnreadableTokenFile(t *testing.T) {
	cfg, err := config.Parse(func(k string) string {
		return map[string]string{"MODEL_REGION": "r", "MODEL_NAME": "m", "AUTH_TOKEN_FILE": filepath.Join(t.TempDir(), "absent")}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), cfg, echo{}, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil || !strings.Contains(err.Error(), "AUTH_TOKEN_FILE") {
		t.Fatalf("Run = %v, want an AUTH_TOKEN_FILE error", err)
	}
}
