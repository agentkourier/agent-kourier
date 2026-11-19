// Command faketeams serves the fake Microsoft Teams service of package faketeams as the fast E2E tier's container: Microsoft Entra's
// token endpoint, the Bot Framework's metadata and keys, the Bot Connector and Microsoft Graph over TLS (-tls-addr), as a broker's
// Teams connection reaches them through the test hooks, and the same with the /_control/ surface over plain HTTP (-addr), for the
// tests. The control surface has no authentication: run it only behind a ClusterIP Service, and never publish its ports. The server
// stops on SIGINT or SIGTERM.
//
// `faketeams cert -host H -dir D` writes a certificate for H, its own CA, and its key to D/tls.crt and D/tls.key, for the tier to put in
// the fake's Secret and the certificate alone in the broker's AGENTKOURIER_TEAMS_CA.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agentkourier/agent-kourier/test/faketeams"
	"github.com/agentkourier/agent-kourier/test/httpserve"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var err error
	if len(os.Args) > 1 && os.Args[1] == "cert" {
		err = cert(os.Args[2:])
	} else {
		err = run(ctx, os.Args[1:], os.Stdout)
	}
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "faketeams:", err)
		os.Exit(1)
	}
}

// listFlag collects a repeatable flag's values.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// cert writes the fake's certificate and key.
func cert(args []string) error {
	fs := flag.NewFlagSet("faketeams cert", flag.ContinueOnError)
	host := fs.String("host", "", "the DNS name the certificate is for")
	dir := fs.String("dir", "", "the directory to write tls.crt and tls.key to")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("flags: %w", err)
	}
	if *host == "" || *dir == "" {
		return errors.New("cert needs -host and -dir")
	}
	crt, key, err := faketeams.SelfSigned(*host)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "tls.crt"), crt, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dir, "tls.key"), key, 0o600)
}

// run parses args and serves until ctx ends.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("faketeams", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "the plain HTTP address: the control API, and the Microsoft side too")
	tlsAddr := fs.String("tls-addr", "127.0.0.1:8443", "the TLS address of the Microsoft side")
	certFile := fs.String("tls-cert", "", "the TLS certificate (PEM)")
	keyFile := fs.String("tls-key", "", "the TLS key (PEM)")
	cfg := faketeams.Config{}
	fs.StringVar(&cfg.PublicURL, "public-url", "", "the https base of the Microsoft side as the broker reaches it")
	fs.StringVar(&cfg.TenantID, "tenant", "", "the tenant's ID")
	fs.StringVar(&cfg.TeamID, "team", "", "the team's Teams ID (its General channel's)")
	fs.StringVar(&cfg.GroupID, "group", "", "the team's Microsoft Entra group ID")
	fs.StringVar(&cfg.ClientSecret, "client-secret", "", "the client secret of every app (a fake constant)")
	fs.BoolVar(&cfg.DeliverHTTP, "deliver-http", false, "send Graph's notifications for an https notificationUrl over plain HTTP, as an ingress in front of the chat listener would")
	var bots, channels listFlag
	fs.Var(&bots, "bot", "a bot the fake hosts: <app ID>=<messaging endpoint>; repeatable")
	fs.Var(&channels, "channel", "a channel of the team; repeatable")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("flags: %w", err)
	}
	cfg.Bots, cfg.Channels = map[string]string{}, channels
	for _, b := range bots {
		app, endpoint, ok := strings.Cut(b, "=")
		if !ok || app == "" || endpoint == "" {
			return errors.New("each -bot must be <app ID>=<messaging endpoint>")
		}
		cfg.Bots[app] = endpoint
	}
	pair, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return fmt.Errorf("the TLS certificate and key: %w", err)
	}
	cfg.Logger = slog.New(slog.NewTextHandler(out, nil))
	svc, err := faketeams.NewService(cfg)
	if err != nil {
		return err
	}
	defer svc.Close()
	h := svc.Handler()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 2)
	go func() { served <- httpserve.Run(ctx, "faketeams", *addr, h, out) }()
	go func() { served <- serveTLS(ctx, *tlsAddr, h, pair, out) }()
	err = <-served
	cancel()
	return errors.Join(err, <-served)
}

// serveTLS serves h over TLS with pair on addr until ctx ends.
func serveTLS(ctx context.Context, addr string, h http.Handler, pair tls.Certificate, out io.Writer) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(ln, srv.TLSConfig)) }()
	_, _ = fmt.Fprintf(out, "faketeams: TLS listening on %s\n", ln.Addr())
	select {
	case err := <-done:
		return fmt.Errorf("serve TLS: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve TLS: %w", err)
	}
	return nil
}
