// Command fakeslack serves the fake Slack of package fakeslack (Web API, Socket Mode, and the
// /_control/ surface) on one port, as the fast E2E tier's container. The control surface has
// no authentication: run it only behind a ClusterIP Service, and never publish its port. The
// server stops on SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
	"github.com/agentkourier/agent-kourier/test/httpserve"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeslack:", err)
		os.Exit(1)
	}
}

// appFlags collects the raw values of the repeatable -app flag. They are parsed after the flags are, because
// the flag package repeats a rejected value in its error, and a value may be a token.
type appFlags []string

func (a *appFlags) String() string { return fmt.Sprintf("%d apps", len(*a)) }

func (a *appFlags) Set(v string) error { *a = append(*a, v); return nil }

// parseApp parses one -app value: comma-separated key=value pairs with the keys app_id, bot_user, bot_id, team_id,
// team_name, bot_token and app_token. The error names a key and never repeats a value.
func parseApp(v string) (fakeslack.App, error) {
	var app fakeslack.App
	fields := map[string]*string{
		"app_id": &app.AppID, "bot_user": &app.BotUser, "bot_id": &app.BotID, "team_id": &app.TeamID,
		"team_name": &app.TeamName, "bot_token": &app.BotToken, "app_token": &app.AppToken,
	}
	for _, pair := range strings.Split(v, ",") {
		key, val, ok := strings.Cut(pair, "=")
		if !ok {
			return app, errors.New("each part of an app must be key=value")
		}
		dst, known := fields[key]
		if !known {
			return app, fmt.Errorf("unknown app key %q", key)
		}
		*dst = val
	}
	return app, app.Validate()
}

// run parses args and serves until ctx ends. The default address is loopback; the container
// passes -addr 0.0.0.0:8080. Each -app adds a Slack app beside the default one, which keeps
// answering every token no -app owns.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("fakeslack", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	var apps appFlags
	fs.Var(&apps, "app", "another Slack app to host: app_id=..,bot_user=..,bot_id=..[,team_id=..,team_name=..,bot_token=xoxb-..,app_token=xapp-..]; repeatable")
	strict := fs.Bool("strict-membership", false, "deliver the default app's events only for channels whose members include its bot")
	strictTokens := fs.Bool("strict-tokens", false, "answer invalid_auth to a token no -app owns, once an -app has a token")
	spread := fs.Bool("socket-spread", false, "send each event to one of an app's connections in turn, not to its newest")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("flags: %w", err)
	}
	var opts []fakeslack.Option
	for i, raw := range apps {
		app, err := parseApp(raw)
		if err != nil {
			return fmt.Errorf("-app #%d: %w", i+1, err)
		}
		opts = append(opts, fakeslack.WithApp(app))
	}
	if *strict {
		opts = append(opts, fakeslack.WithStrictMembership())
	}
	if *strictTokens {
		opts = append(opts, fakeslack.WithStrictTokens())
	}
	if *spread {
		opts = append(opts, fakeslack.WithSocketSpread())
	}
	slack := fakeslack.NewHandler(opts...)
	defer slack.Close()
	return httpserve.Run(ctx, "fakeslack", *addr, slack, out)
}
