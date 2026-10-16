package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsmcp"
)

const fixture = "../../corpus/testdata/docs"

// TestMain lets the test binary stand in for the command: with DOCS_MCP_RUN set, it runs realMain.
func TestMain(m *testing.M) {
	if os.Getenv("DOCS_MCP_RUN") == "1" {
		os.Exit(realMain())
	}
	os.Exit(m.Run())
}

func TestParse(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    settings
		wantErr string
	}{
		{"flags", []string{"-corpus", "/docs", "-site-url", "https://d.example", "-http", ":9000"}, nil,
			settings{corpus: "/docs", siteURL: "https://d.example", addr: ":9000"}, ""},
		{"environment", nil, map[string]string{"CORPUS_DIR": "/c", "SITE_URL": "https://s.example", "MCP_ADDR": "127.0.0.1:1"},
			settings{corpus: "/c", siteURL: "https://s.example", addr: "127.0.0.1:1"}, ""},
		{"exclusions", []string{"-corpus", "/d", "-exclude", "brand,spikes"}, nil, settings{corpus: "/d", exclude: "brand,spikes"}, ""},
		{"a flag beats the environment", []string{"-corpus", "/flag"}, map[string]string{"CORPUS_DIR": "/env"}, settings{corpus: "/flag"}, ""},
		{"stdio when no address", []string{"-corpus", "/d"}, nil, settings{corpus: "/d"}, ""},
		{"no corpus", nil, nil, settings{}, "corpus"},
		{"an unknown flag", []string{"-corpus", "/d", "-nope"}, nil, settings{}, "nope"},
		{"a stray argument", []string{"-corpus", "/d", "extra"}, nil, settings{}, "extra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(tt.args, env(tt.env), &strings.Builder{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parse error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("parse = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// TestServeStdioStopsCleanlyWhenTheContextEnds pins that a signal ends the stdio server without an error: the SDK
// returns the context's error from Run, and that is how the command is asked to stop.
func TestServeStdioStopsCleanlyWhenTheContextEnds(t *testing.T) {
	ix, err := corpus.Load(t.Context(), fixture, corpus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := docsmcp.New(docsmcp.Config{Searcher: ix, Reader: ix})
	if err != nil {
		t.Fatal(err)
	}
	serverSide, clientSide := mcp.NewInMemoryTransports()
	ctx, signalled := context.WithCancel(t.Context())
	defer signalled()
	done := make(chan error, 1)
	go func() { done <- serveStdio(ctx, srv, serverSide) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	cs, err := client.Connect(t.Context(), clientSide, nil) // the server is up and a client is connected
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	signalled()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveStdio after the context ended = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveStdio did not return")
	}
}

// TestServeStdioReportsAFailure pins that only a stop by context is clean: any other error from the transport is
// still an error.
func TestServeStdioReportsAFailure(t *testing.T) {
	ix, err := corpus.Load(t.Context(), fixture, corpus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := docsmcp.New(docsmcp.Config{Searcher: ix, Reader: ix})
	if err != nil {
		t.Fatal(err)
	}
	if err := serveStdio(t.Context(), srv, failingTransport{}); err == nil || !errors.Is(err, errConnect) {
		t.Errorf("serveStdio = %v, want the transport's error", err)
	}
}

var errConnect = errors.New("cannot connect")

type failingTransport struct{}

func (failingTransport) Connect(context.Context) (mcp.Connection, error) { return nil, errConnect }

// TestStdioExitsZeroOnSIGTERM runs the real command over stdio, signals it, and checks its exit status.
func TestStdioExitsZeroOnSIGTERM(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Env = append(os.Environ(), "DOCS_MCP_RUN=1", "CORPUS_DIR="+fixture, "MCP_ADDR=")
	stdin, err := cmd.StdinPipe() // held open: the server is waiting for a client, not at the end of its input
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	stderr, err := cmd.StderrPipe()
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
		sc := bufio.NewScanner(stderr)
		started := false
		for sc.Scan() {
			if !started && strings.Contains(sc.Text(), "corpus loaded") {
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
		t.Fatal("the command never loaded its corpus")
	}
	// No pause for the command to reach Run: realMain installs the signal handler before it loads the corpus, so a
	// signal that arrives earlier or later cancels the same context, and Run returns at once or when it starts.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-read
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exit after SIGTERM: %v\n%s", err, logs.String())
	}
}
