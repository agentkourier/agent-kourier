// Command ax-fixture provides the local kind tier's deterministic child and TLS
// termination. It is built into the kind fixture image only.
package main

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("expected echo or proxy")
	}
	switch os.Args[1] {
	case "echo":
		echo()
	case "proxy":
		log.Fatal(proxy())
	default:
		log.Fatal("expected echo or proxy")
	}
}

func echo() {
	goal, err := os.ReadFile(os.Getenv("AX_GOAL_FILE")) //nolint:gosec // AX supplies the fixture input file
	if err != nil {
		os.Exit(2)
	}
	switch {
	case strings.Contains(string(goal), "AX_RESULT_FAIL"):
		_, _ = os.Stderr.WriteString("ax-fixture-private-stderr")
		os.Exit(7)
	case strings.Contains(string(goal), "AX_RESULT_HOLD"):
		time.Sleep(10 * time.Minute)
	case strings.Contains(string(goal), "AX_RESULT_SILENT"):
		time.Sleep(12 * time.Second)
	default:
		time.Sleep(2 * time.Second)
	}
	if err := os.WriteFile(os.Getenv("AX_OUTPUT_FILE"), goal, 0o600); err != nil { //nolint:gosec // AX supplies the fixture output path
		os.Exit(3)
	}
}

func proxy() error {
	target, err := url.Parse("http://agent-kourier-ax.agent-kourier.svc:8090")
	if err != nil || target.Scheme != "http" || target.Host == "" {
		return errors.New("invalid fixture upstream")
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	mux := http.NewServeMux()
	mux.Handle(result.Path, forward)
	mux.Handle(result.StartPath, forward)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	server := &http.Server{Addr: ":8443", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return server.ListenAndServeTLS("/tls/tls.crt", "/tls/tls.key")
}
