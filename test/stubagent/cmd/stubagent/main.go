// Command stubagent serves the stub A2A agents of package stubagent on a local port, scripted
// as the WP-02 spike's mock models, so that the spike can run against it instead of a kagent
// cluster:
//
//	go run ./test/stubagent/cmd/stubagent &
//	go run ./spike/a2a -base http://127.0.0.1:18083 -token "$TOKEN" -out "$(mktemp -d)"
//
// TOKEN is an unsigned JWT with an email claim, which the identity case needs; -no-auth in
// place of -token skips that case. Never point the spike's -out at test/fixtures/kagent.
// `make stub-oracle` does all of this and checks the result. The server stops on SIGINT or
// SIGTERM.
//
// It also serves the agents of ScenarioAgents, for the E2E scenarios; the stubagent package doc
// of ScenarioAgents maps each scenario to its agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// defaultTick is the pause between the chunks of a turn when -tick is not given, and so the tick
// the fast tier's Deployment runs at.
const defaultTick = 300 * time.Millisecond

func main() {
	addr := flag.String("addr", "127.0.0.1:18083", "address to listen on; the spike's default -base is this")
	tick := flag.Duration("tick", defaultTick, "pause between the text chunks of a turn; the spike's slow agent needs it long enough to act between chunks")
	requireAuth := flag.Bool("require-auth", false, "answer 401 to a request with no bearer token, as kagent in OIDC mode does; the spike's -no-auth mode then fails")
	flag.Parse()
	if err := run(*addr, *tick, *requireAuth); err != nil {
		fmt.Fprintln(os.Stderr, "stubagent:", err)
		os.Exit(1)
	}
}

// newStub returns the server with every agent the command serves.
func newStub(tick time.Duration, requireAuth bool) *stubagent.Server {
	opts := []stubagent.Option{stubagent.WithTick(tick)}
	if requireAuth {
		opts = append(opts, stubagent.WithRequiredAuth())
	}
	stub := stubagent.NewHandler(opts...)
	stubagent.SpikeAgents(stub)
	stubagent.ScenarioAgents(stub)
	return stub
}

func run(addr string, tick time.Duration, requireAuth bool) error {
	stub := newStub(tick, requireAuth)
	defer stub.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: stub, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.WithoutCancel(ctx)) // the shutdown must outlive the signal that starts it
	}()
	fmt.Println("stubagent: listening on", addr) // after the bind, so a script can wait for this line
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
