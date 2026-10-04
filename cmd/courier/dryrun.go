package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// dryRunUsage is what `courier dry-run --help` prints.
const dryRunUsage = `usage: courier dry-run --config <dir> --binding <namespace/name> [options]

Replays the history of a Binding's channel through the Binding's chat triggers and prints what each message
would become, oldest first, and then a summary. It only reads Slack; it posts nothing and changes nothing.

  --config <dir>        the resource files, loaded as the broker loads them: defaults, presets and validation apply
                        (default: $COURIER_CONFIG_DIR)
  --binding <ns/name>   the Binding whose triggers to run
  --channel <id>        the channel to read (default: the Binding's chat.channel)
  --since <duration>    only messages newer than this, such as 72h (default: all of it)
  --limit <n>           only the newest n messages that were read (default: all of them)
  --json                print one JSON report instead of text
  --token-file <path>   a file that holds the bot token, alone or as SLACK_BOT_TOKEN=... (mode 600)

The bot token is read from --token-file, else from $SLACK_BOT_TOKEN; it is never an argument and never printed.
The bot needs channels:history (groups:history for a private channel) and must be a member of the channel.
`

// dryRunOptions are the options of `courier dry-run`.
//
// TODO(CT-4a): a --render option, which shows whether each match's prompt would be refused, with the ref and the
// reason, once internal/render has NewPrompt and Render on main.
type dryRunOptions struct {
	configDir, binding, channel, tokenFile string
	since                                  time.Duration
	limit                                  int
	json                                   bool

	// The seams of the tests; no flag or variable sets them, so that the bot token can only be sent to Slack.
	apiURL    string // Slack's Web API URL, with a trailing slash; Slack's if empty
	pageLimit int    // how many messages to ask for per page; the client's default if zero
}

// tokenShape matches what looks like a Slack token, as a person may type one in the wrong place.
var tokenShape = regexp.MustCompile(`xox[a-z]-[^\s"]+|xapp-[^\s"]+`)

// parseDryRun reads the command line of `courier dry-run`. It returns flag.ErrHelp for --help, and otherwise an
// error that says what is wrong with the arguments, with anything shaped like a token taken out of it: the flag
// package quotes the value it could not read, and a token is never an argument.
func parseDryRun(args []string, getenv func(string) string) (dryRunOptions, error) {
	o, err := parseDryRunArgs(args, getenv)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		err = errors.New(tokenShape.ReplaceAllString(err.Error(), "[redacted]"))
	}
	return o, err
}

func parseDryRunArgs(args []string, getenv func(string) string) (dryRunOptions, error) {
	var o dryRunOptions
	fs := flag.NewFlagSet("courier dry-run", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // the caller reports the error, and the usage is its own text
	fs.Usage = func() {}
	fs.StringVar(&o.configDir, "config", getenv("COURIER_CONFIG_DIR"), "")
	fs.StringVar(&o.binding, "binding", "", "")
	fs.StringVar(&o.channel, "channel", "", "")
	fs.StringVar(&o.tokenFile, "token-file", "", "")
	fs.DurationVar(&o.since, "since", 0, "")
	fs.IntVar(&o.limit, "limit", 0, "")
	fs.BoolVar(&o.json, "json", false, "")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	switch {
	case fs.NArg() > 0:
		return o, errors.New("unexpected argument at position 1; the command takes options only")
	case o.configDir == "":
		return o, errors.New("--config is required (or COURIER_CONFIG_DIR)")
	case o.binding == "":
		return o, errors.New("--binding is required, as namespace/name")
	case !strings.Contains(o.binding, "/") || strings.HasPrefix(o.binding, "/") || strings.HasSuffix(o.binding, "/"):
		return o, fmt.Errorf("--binding %q is not namespace/name", o.binding)
	case o.limit < 0:
		return o, fmt.Errorf("--limit %d is negative", o.limit)
	case o.since < 0:
		return o, fmt.Errorf("--since %s is negative", o.since)
	}
	return o, nil
}

// dryRunCommand is `courier dry-run`: it returns the exit code, 0 for a report, 2 for a wrong command line, config
// or token, 1 when Slack rejects the token, and 4 for anything else.
func dryRunCommand(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	opts, err := parseDryRun(args, getenv)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprint(stderr, dryRunUsage)
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "courier dry-run: %v\nrun courier dry-run --help for the usage\n", err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return execDryRun(ctx, opts, getenv, stdout, stderr)
}

// execDryRun loads the Binding, reads the history of its channel and prints the report. It returns the exit code of
// dryRunCommand.
func execDryRun(ctx context.Context, o dryRunOptions, getenv func(string) string, stdout, stderr io.Writer) int {
	report, err := dryRunReportOf(ctx, o, getenv, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "courier dry-run: %v\n", err)
		return dryRunExitCode(err)
	}
	write := report.writeText
	if o.json {
		write = report.writeJSON
	}
	if err := write(stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "courier dry-run: write the report: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// usageError marks an error as one of the command line, the config or the token: the exit code is exitUsage.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// usagef is a usageError with the message of fmt.Errorf.
func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

// dryRunExitCode is the exit code of an error of execDryRun.
func dryRunExitCode(err error) int {
	var slackErr slackgo.SlackErrorResponse
	switch {
	case errors.As(err, &usageError{}):
		return exitUsage
	case errors.As(err, &slackErr) && slackAuthCodes[firstWord(slackErr.Err)]:
		return exitSlackAuth
	}
	return exitFailure
}

// slackAuthCodes are the codes with which Slack rejects the token for good: a retry would be rejected again.
var slackAuthCodes = map[string]bool{"invalid_auth": true, "not_authed": true, "token_revoked": true, "token_expired": true, "account_inactive": true}

// firstWord is the text up to the first space.
func firstWord(s string) string {
	w, _, _ := strings.Cut(s, " ")
	return w
}

// dryRunReportOf does the work of the command: it loads the config as the broker does, reads the channel's history
// as the adapter decodes it, and runs the Binding's triggers over it. Notes that are not the report go to stderr.
func dryRunReportOf(ctx context.Context, o dryRunOptions, getenv func(string) string, stderr io.Writer) (dryRunReport, error) {
	set, err := config.Load(ctx, o.configDir)
	if err != nil {
		return dryRunReport{}, usagef("load %s: %w", o.configDir, err)
	}
	b, err := findBinding(set, o.binding)
	if err != nil {
		return dryRunReport{}, usagef("%w", err)
	}
	replay, err := chat.NewReplay(b)
	if err != nil {
		return dryRunReport{}, usagef("Binding %s: %w", o.binding, err)
	}
	if !hasBotTrigger(b) {
		_, _ = fmt.Fprintf(stderr, "courier dry-run: note: Binding %s has no botMessage trigger, so every bot message is other_bot\n", o.binding)
	}
	token, err := loadBotToken(o.tokenFile, getenv)
	if err != nil {
		return dryRunReport{}, usagef("%w", err)
	}
	channel := cmp.Or(o.channel, b.Spec.Chat.Channel)
	client := slack.New(slack.Config{
		BotToken: token, APIURL: o.apiURL, PageLimit: o.pageLimit,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	})
	var since time.Time
	if o.since > 0 {
		since = time.Now().Add(-o.since)
	}
	conn := v1alpha1.NamespacedName(cmp.Or(b.Spec.Chat.ConnectionRef.Namespace, b.Namespace), b.Spec.Chat.ConnectionRef.Name)
	history, err := client.History(ctx, conn, channel, since)
	if err != nil {
		return dryRunReport{}, fmt.Errorf("read %s: %w%s", channel, err, slackHint(err))
	}
	if o.limit > 0 && len(history) > o.limit {
		history = history[len(history)-o.limit:]
	}
	return buildReport(o.binding, channel, history, replay, closingStates(b)), nil
}

// findBinding returns the Binding namespace/name of the set, or an error that lists the Bindings it has.
func findBinding(set *config.Set, namespaced string) (*v1alpha1.Binding, error) {
	var have []string
	for i := range set.Bindings {
		b := &set.Bindings[i]
		name := v1alpha1.NamespacedName(b.Namespace, b.Name)
		if name == namespaced {
			return b, nil
		}
		have = append(have, name)
	}
	return nil, fmt.Errorf("no Binding %s in the config (it has: %s)", namespaced, cmp.Or(strings.Join(have, ", "), "none"))
}

// hasBotTrigger says whether the Binding has a botMessage trigger.
func hasBotTrigger(b *v1alpha1.Binding) bool {
	for _, t := range b.Spec.Chat.Triggers {
		if t.Type == v1alpha1.TriggerBotMessage {
			return true
		}
	}
	return false
}

// closingStates maps the index of each botMessage trigger to the state that closes its thread (thread.closeWhen.state),
// RESOLVED if it names none: the state after which a new FIRING is a new cycle.
func closingStates(b *v1alpha1.Binding) map[int]string {
	out := map[int]string{}
	for i, t := range b.Spec.Chat.Triggers {
		out[i] = cmp.Or(t.Thread.CloseWhen["state"], "RESOLVED")
	}
	return out
}

// slackHint is what to do about the error codes of reading a channel that people meet, as a sentence that starts
// with a space, or nothing.
func slackHint(err error) string {
	var slackErr slackgo.SlackErrorResponse
	if !errors.As(err, &slackErr) {
		return ""
	}
	switch firstWord(slackErr.Err) {
	case "not_in_channel":
		return " (invite the bot to the channel: /invite @<the app's name>)"
	case "channel_not_found":
		return " (the channel ID is wrong, or the bot cannot see it: a private channel needs an invite)"
	case "missing_scope":
		return " (the Slack app needs the scope channels:history, or groups:history for a private channel; add it and reinstall the app)"
	}
	return ""
}
