package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsagent"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/scriptedmodel"
)

const fixture = "../../corpus/testdata/docs"

func TestSettingsFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    settings
		wantErr string
	}{
		{"defaults", nil, settings{corpus: "/corpus", siteURL: "https://agentkourier.dev", listen: ":8080", publicURL: "http://localhost:8080/a2a", subject: "Agent Kourier"}, ""},
		{"everything set", map[string]string{
			"CORPUS_DIR": "/kb", "CORPUS_EXCLUDE": "drafts, private/notes.md", "SITE_URL": "https://kb.example.com",
			"LISTEN_ADDR": ":9000", "PUBLIC_URL": "https://agent.example.com/a2a", "DOCS_SUBJECT": "Acme Relay",
		}, settings{corpus: "/kb", exclude: []string{"drafts", "private/notes.md"}, siteURL: "https://kb.example.com", listen: ":9000", publicURL: "https://agent.example.com/a2a", subject: "Acme Relay"}, ""},
		{"an external MCP server", map[string]string{"MCP_URL": "http://kb-search.kb.svc:8090/"},
			settings{corpus: "/corpus", siteURL: "https://agentkourier.dev", listen: ":8080", publicURL: "http://localhost:8080/a2a", mcpURL: "http://kb-search.kb.svc:8090/", subject: "Agent Kourier"}, ""},
		{"an empty site URL means no URLs", map[string]string{"SITE_URL": "-"},
			settings{corpus: "/corpus", listen: ":8080", publicURL: "http://localhost:8080/a2a", subject: "Agent Kourier"}, ""},
		{"a bad MCP URL", map[string]string{"MCP_URL": "kb-search:8090"}, settings{}, "MCP_URL"},
		{"a bad public URL", map[string]string{"PUBLIC_URL": "/a2a"}, settings{}, "PUBLIC_URL"},
		{"an MCP URL with credentials", map[string]string{"MCP_URL": "http://user:pw@kb-search:8090/"}, settings{}, "MCP_URL"},
		{"a public URL with a query", map[string]string{"PUBLIC_URL": "https://agent.example.com/a2a?x=1"}, settings{}, "PUBLIC_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := settingsFromEnv(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("settings = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// gatedModel is the scripted model, held at the step that follows the search until the test releases it. The turn is
// then in flight, with the page still to be read through the MCP server.
type gatedModel struct {
	scriptedmodel.Model
	reached chan struct{} // closed when the turn has searched and waits
	release chan struct{}
	once    sync.Once
}

func (m *gatedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if req != nil && len(req.Contents) > 0 {
		for _, p := range req.Contents[len(req.Contents)-1].Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "search_docs" {
				m.once.Do(func() { close(m.reached) })
				select {
				case <-m.release:
				case <-ctx.Done():
				}
			}
		}
	}
	return m.Model.GenerateContent(ctx, req, stream)
}

// dial tries to connect to addr, for a moment.
func dial(t *testing.T, addr string) (net.Conn, error) {
	t.Helper()
	d := net.Dialer{Timeout: 100 * time.Millisecond}
	return d.DialContext(t.Context(), "tcp", addr)
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// TestShutdownDrainsATurnBeforeStoppingTheMCPServer starts both servers, begins a turn, ends the context while the
// turn is between its search and its page read, and checks that the read still succeeds (so the MCP server outlives
// the A2A drain) and that the stop is clean.
func TestShutdownDrainsATurnBeforeStoppingTheMCPServer(t *testing.T) {
	gm := &gatedModel{reached: make(chan struct{}), release: make(chan struct{})}
	ln := listen(t)
	endpoint := "http://" + ln.Addr().String() + "/a2a"

	mcpCtx, stopMCP := context.WithCancel(t.Context())
	defer stopMCP()
	handler, mcpDone, err := docsagent.Assemble(mcpCtx, docsagent.AssembleOptions{Corpus: fixture, Model: gm, PublicURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	ctx, signalled := context.WithCancel(t.Context())
	defer signalled()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, ln, handler, mcpDone, stopMCP, 10*time.Second) }()

	card := &a2a.AgentCard{Name: "t", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(endpoint, a2a.TransportProtocolJSONRPC)}}
	client, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	type turn struct {
		text string
		err  error
	}
	turnDone := make(chan turn, 1)
	go func() {
		var b strings.Builder
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("How do I connect an agent?"))
		for event, err := range client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: msg}) {
			if err != nil {
				turnDone <- turn{err: err}
				return
			}
			if u, ok := event.(*a2a.TaskArtifactUpdateEvent); ok && u.Artifact != nil {
				for _, p := range u.Artifact.Parts {
					if text, ok := p.Content.(a2a.Text); ok {
						b.WriteString(string(text))
					}
				}
			}
		}
		turnDone <- turn{text: b.String()}
	}()

	select {
	case <-gm.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never reached its page read")
	}
	signalled() // what SIGTERM does
	// Once the A2A listener is closed, the server is draining; the turn is still in flight.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := dial(t, ln.Addr().String())
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the A2A server kept accepting connections after the signal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-served:
		t.Fatalf("serve returned (%v) while a turn was in flight", err)
	default:
	}
	close(gm.release)

	select {
	case got := <-turnDone:
		if got.err != nil {
			t.Fatalf("the turn failed during the drain: %v", got.err)
		}
		if !strings.Contains(got.text, "how-to/connect-agent.md") {
			t.Errorf("answer = %q, want one that read and cited the page, which needs the MCP server during the drain", got.text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the turn did not finish")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve after a signal = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return after the drain")
	}
}

// TestServeFailsWhenTheMCPServerStopsUnexpectedly pins that the clean-stop path does not hide a dead embedded
// server: the agent could answer nothing, so serve must return an error and stop the A2A server.
func TestServeFailsWhenTheMCPServerStopsUnexpectedly(t *testing.T) {
	for name, cause := range map[string]error{"with an error": errors.New("serve: boom"), "with no error": nil} {
		t.Run(name, func(t *testing.T) {
			ln := listen(t)
			mcpDone := make(chan error, 1)
			served := make(chan error, 1)
			go func() {
				served <- serve(t.Context(), ln, http.NotFoundHandler(), mcpDone, func() {}, time.Second)
			}()
			mcpDone <- cause
			select {
			case err := <-served:
				if err == nil || !strings.Contains(err.Error(), "embedded MCP server stopped") {
					t.Fatalf("serve = %v, want an error naming the embedded MCP server", err)
				}
				if cause != nil && !errors.Is(err, cause) {
					t.Errorf("serve = %v, want it to carry %v", err, cause)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("serve did not return")
			}
			if c, err := dial(t, ln.Addr().String()); err == nil {
				_ = c.Close()
				t.Error("the A2A server still accepts connections")
			}
		})
	}
}

// TestServeStopsCleanlyOnSignal repeats the clean stop; with one context shared by both servers it was a race that
// exited 1 in most runs.
func TestServeStopsCleanlyOnSignal(t *testing.T) {
	for i := range 20 {
		ln := listen(t)
		mcpCtx, stopMCP := context.WithCancel(t.Context())
		handler, mcpDone, err := docsagent.Assemble(mcpCtx, docsagent.AssembleOptions{Corpus: fixture, Model: &scriptedmodel.Model{}, PublicURL: "http://" + ln.Addr().String() + "/a2a"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, signalled := context.WithCancel(t.Context())
		served := make(chan error, 1)
		go func() { served <- serve(ctx, ln, handler, mcpDone, stopMCP, time.Second) }()
		signalled()
		select {
		case err := <-served:
			if err != nil {
				t.Fatalf("run %d: serve after a signal = %v, want nil", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("run %d: serve did not return", i)
		}
		stopMCP()
	}
}

// TestMain lets the test binary stand in for the command: with DOCS_AGENT_RUN set, it runs realMain.
func TestMain(m *testing.M) {
	if os.Getenv("DOCS_AGENT_RUN") == "1" {
		os.Exit(realMain())
	}
	os.Exit(m.Run())
}

// TestExitsZeroOnSIGTERM runs the real command, signals it, and checks its exit status: a normal stop is not a
// failure. The loop is for the race the shared context had.
func TestExitsZeroOnSIGTERM(t *testing.T) {
	for i := range 8 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0])
			cmd.Env = append(os.Environ(), "DOCS_AGENT_RUN=1", "CORPUS_DIR="+fixture, "LISTEN_ADDR=127.0.0.1:0",
				"MODEL_PROVIDER=openai", "MODEL_NAME=some-model", "MODEL_API_KEY="+"test-key-not-a-real-credential-0000")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var logs strings.Builder // written by the reader goroutine only, and read after it ends
			ready, read := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(read)
				sc := bufio.NewScanner(out)
				started := false
				for sc.Scan() {
					if !started && strings.Contains(sc.Text(), "serving A2A") {
						started = true
						close(ready)
					}
					logs.WriteString(sc.Text() + "\n")
				}
			}()
			select {
			case <-ready:
			case <-time.After(20 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("the command never started serving")
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			<-read
			if err := cmd.Wait(); err != nil {
				t.Fatalf("exit after SIGTERM: %v\n%s", err, logs.String())
			}
		})
	}
}
