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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	uniqueIDs := flag.Bool("unique-ids", false, "put a tag of this start in every id, so that a restart never makes an id of an earlier run that a client stored; the ids then differ from run to run")
	flag.Parse()
	if err := run(*addr, *tick, *requireAuth, *uniqueIDs); err != nil {
		fmt.Fprintln(os.Stderr, "stubagent:", err)
		os.Exit(1)
	}
}

// idTag is the tag that this start of the stub, and no other, puts in its ids when unique is set (-unique-ids), and none otherwise.
func idTag(unique bool) (string, error) {
	if !unique {
		return "", nil
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("make an id tag: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// newStub returns the server with every agent the command serves. idTag, if not empty, is in every id it makes.
func newStub(tick time.Duration, requireAuth bool, idTag string) *stubagent.Server {
	opts := []stubagent.Option{stubagent.WithTick(tick), stubagent.WithIDTag(idTag)}
	if requireAuth {
		opts = append(opts, stubagent.WithRequiredAuth())
	}
	stub := stubagent.NewHandler(opts...)
	stubagent.SpikeAgents(stub)
	stubagent.ScenarioAgents(stub)
	return stub
}

// releasePath is the control surface's one route: POST releasePath+gate opens the gate that
// scenario-gated holds on, and answers 204.
const releasePath = "/_control/release/"

// withControl serves POST /_control/release/{gate} for stub and passes everything else on. Like
// the fake Slack's /_control/ it has no authentication, not even with -require-auth, so the
// Service that reaches it must stay ClusterIP (hack/kind/fast/verify.sh checks that).
func withControl(stub *stubagent.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gate, ok := strings.CutPrefix(r.URL.Path, releasePath)
		if !ok || r.Method != http.MethodPost {
			stub.ServeHTTP(w, r)
			return
		}
		if !stubagent.ValidGate(gate) {
			http.Error(w, "a gate name is letters, digits, _ and -", http.StatusBadRequest)
			return
		}
		stub.Release(gate)
		w.WriteHeader(http.StatusNoContent)
	})
}

func run(addr string, tick time.Duration, requireAuth, uniqueIDs bool) error {
	tag, err := idTag(uniqueIDs)
	if err != nil {
		return err
	}
	stub := newStub(tick, requireAuth, tag)
	defer stub.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: withControl(stub), ReadHeaderTimeout: 10 * time.Second}
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
