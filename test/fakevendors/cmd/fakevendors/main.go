// Command fakevendors serves the fake vendors of package fakevendors (the senders of GitHub's, Bitbucket Cloud's, Jira Cloud's and
// Linear's webhooks, and their APIs as the agent's tools call them) on one port, as the fast E2E tier's container. Each -source is one
// vendor's connection on the broker: its route on the chat listener at -chat-url, the secret it signs with and the agent's own account,
// which the deliveries of the agent's posts name as their actor. The control surface has no authentication: run it only behind a
// ClusterIP Service, and never publish its port. The server stops on SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/agentkourier/agent-kourier/test/fakevendors"
	"github.com/agentkourier/agent-kourier/test/httpserve"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakevendors:", err)
		os.Exit(1)
	}
}

// sourceFlags collects the raw values of the repeatable -source flag. They are parsed after the flags are, because the flag package
// repeats a rejected value in its error, and a value holds a secret.
type sourceFlags []string

func (s *sourceFlags) String() string { return fmt.Sprintf("%d sources", len(*s)) }

func (s *sourceFlags) Set(v string) error { *s = append(*s, v); return nil }

// parseSource parses one -source value: comma-separated key=value pairs with the keys vendor, path, identity and secret. The error names
// a key and never repeats a value.
func parseSource(v string) (vendor string, src fakevendors.Source, err error) {
	var secret string
	fields := map[string]*string{"vendor": &vendor, "path": &src.Path, "identity": &src.Identity, "secret": &secret}
	for _, pair := range strings.Split(v, ",") {
		key, val, ok := strings.Cut(pair, "=")
		if !ok {
			return "", src, errors.New("each part of a source must be key=value")
		}
		dst, known := fields[key]
		if !known {
			return "", src, fmt.Errorf("unknown source key %q", key)
		}
		*dst = val
	}
	src.Secret = []byte(secret)
	return vendor, src, nil
}

// run parses args and serves until ctx ends. The default address is loopback; the container passes -addr 0.0.0.0:8080.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("fakevendors", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	chatURL := fs.String("chat-url", "", "the broker's chat listener, scheme and host, which the deliveries are POSTed to")
	var sources sourceFlags
	fs.Var(&sources, "source", "a vendor's connection: vendor=github|bitbucket|jira|linear,path=/chat/webhook/<namespace>/<name>,identity=..,secret=..; repeatable, one for each vendor")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("flags: %w", err)
	}
	if u, err := url.Parse(*chatURL); err != nil || u.Host == "" {
		return errors.New("-chat-url must be an absolute http or https URL")
	}
	if len(sources) == 0 {
		return errors.New("give at least one -source")
	}
	cfg := fakevendors.Config{ChatURL: *chatURL, Sources: map[string]fakevendors.Source{}}
	for i, raw := range sources {
		vendor, src, err := parseSource(raw)
		if err != nil {
			return fmt.Errorf("-source #%d: %w", i+1, err)
		}
		if _, dup := cfg.Sources[vendor]; dup {
			return fmt.Errorf("-source #%d: a second source of the vendor %q", i+1, vendor)
		}
		cfg.Sources[vendor] = src
	}
	vendors, err := fakevendors.New(cfg)
	if err != nil {
		return err
	}
	return httpserve.Run(ctx, "fakevendors", *addr, vendors, out)
}
