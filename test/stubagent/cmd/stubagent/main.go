// Command stubagent serves the stub A2A agents of package stubagent on a local port, scripted
// as the WP-02 spike's mock models, so that the spike can run against it instead of a kagent
// cluster:
//
//	go run ./test/stubagent/cmd/stubagent &
//	go run ./spike/a2a -base http://127.0.0.1:18083 -token "$TOKEN" -out "$(mktemp -d)"
//
// TOKEN is an unsigned JWT with an email claim, which the identity case needs; -no-auth in
// place of -token skips that case. Never point the spike's -out at test/fixtures/kagent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18083", "address to listen on; the spike's default -base is this")
	tick := flag.Duration("tick", 300*time.Millisecond, "pause between the text chunks of a turn; the spike's slow agent needs it long enough to act between chunks")
	flag.Parse()
	if err := run(*addr, *tick); err != nil {
		fmt.Fprintln(os.Stderr, "stubagent:", err)
		os.Exit(1)
	}
}

func run(addr string, tick time.Duration) error {
	stub := stubagent.NewHandler(stubagent.WithTick(tick))
	defer stub.Close()
	stubagent.SpikeAgents(stub)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: stub, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.WithoutCancel(ctx)) // the shutdown must outlive the signal that starts it
	}()
	fmt.Println("stubagent: listening on", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
