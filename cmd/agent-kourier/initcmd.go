package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"text/template"
	"time"

	slackgo "github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// initUsage is what `agent-kourier init --help` prints.
const initUsage = `usage: agent-kourier init [options]

Writes the files of a first install with the Helm chart (one Slack app, one agent, one channel), checks them, and prints
the commands that install the chart, create the Secrets and, on custom resources, grant them to the broker and apply the
team's file. It posts nothing and changes nothing in Slack or in a cluster. A value that no option gives is asked for on standard input.

On custom resources (--source crds, the chart's default route) the broker has no Slack app of its own, and the team
brings its own. It writes two files:
  my-values.yaml   the platform team's values: config.connectionNamespaces, the team's namespace
  <team>.yaml      the team's ChatConnection <team>-slack (the team's app, reading the Secret <team>-slack), its
                   AgentBackend agent and its Binding my-binding, all in the team's namespace
<team>.yaml goes in the directory of --out. With --source files it writes one values file for the ConfigMap route
(config.source files) that holds the three resources, in the release's namespace, on the broker's own Slack app.

  --source <crds|files>     the chart's config route (default: crds)
  --out <path>              the values file to write (default: my-values.yaml); init stops if it, or the team's file,
                            exists (see --force)
  --force                   overwrite the files, if they are regular files
  --dialect <name>          the agent's dialect: a2a or kagent-v1 (default when asked: a2a)
  --agent-url <url>         the agent's URL, called exactly as written
  --agent-namespace <ns>    the agent's namespace
  --team-namespace <ns>     crds only: the team's namespace, which must exist (default when asked: the agent's namespace)
  --agent-name <name>       the agent's name
  --channel <id|name>       the channel, by ID (C... or G...), or by name, which is looked up in Slack and needs a bot token
  --agent-token             the agent verifies tokens: the Binding reads one from the Secret agent-kourier-agent-token,
                            in the Binding's namespace
  --namespace <ns>          the namespace of the release (default: agent-kourier), which the commands it prints name;
                            with --source files the resources go in it
  --token-file <path>       a file that holds the bot token, alone or as SLACK_BOT_TOKEN=... (mode 600); on custom
                            resources, the team's app's

With a bot token, from --token-file or $SLACK_BOT_TOKEN, init also checks that the bot can read the channel (the bot needs
channels:history, or groups:history for a private channel, and must be a member of the channel), and a channel name is
looked up (channels:read and groups:read). The token is never an argument and never printed. Without one the Slack checks are
skipped, and a channel must be given by ID.
`

// defaultInitNamespace is the namespace of the release when --namespace is not given: the one the install guide uses.
const defaultInitNamespace = "agent-kourier"

// The names the generated values and the printed commands use; they are the ones the guides and the starter file use.
const (
	slackSecretName      = "slack"
	agentTokenSecretName = "agent-kourier-agent-token" //nolint:gosec // G101: the name of a Secret, not a credential
	chartReference       = "oci://ghcr.io/agentkourier/charts/agent-kourier"
	initBindingName      = "my-binding"
	// initReleaseName is the release the printed helm install makes. The chart names its ServiceAccount after a release
	// whose name holds agent-kourier, so the team's RoleBinding names this too.
	initReleaseName = "agent-kourier"
)

// The routes of the chart's config.source that init writes for.
const (
	sourceCRDs  = "crds"
	sourceFiles = "files"
)

// resourceAPIVersion is the apiVersion the chart gives each resource file (templates/_helpers.tpl, agentkourier.resource).
const resourceAPIVersion = "agentkourier.dev/v1alpha1"

// releaseVersion matches a version of a release, as the binary's --version prints it: v0.1.1 or 0.1.1-rc.1.
var releaseVersion = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$`)

// initOptions are the options of `agent-kourier init`.
type initOptions struct {
	source, out, dialect, agentURL, agentNamespace, teamNamespace, agentName, channel, namespace, tokenFile string
	agentToken, force                                                                                       bool
	agentTokenSet                                                                                           bool // --agent-token was on the command line

	// The seam of the tests; no flag or variable sets it, so that the bot token can only be sent to Slack.
	apiURL string // Slack's Web API URL, with a trailing slash; Slack's if empty
}

// parseInit reads the command line of `agent-kourier init`. It returns flag.ErrHelp for --help, and otherwise an error
// that says what is wrong with the arguments, with anything shaped like a token taken out of it.
func parseInit(args []string) (initOptions, error) {
	o, err := parseInitArgs(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		err = errors.New(tokenShape.ReplaceAllString(err.Error(), "[redacted]"))
	}
	return o, err
}

func parseInitArgs(args []string) (initOptions, error) {
	var o initOptions
	fs := flag.NewFlagSet("agent-kourier init", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // the caller reports the error, and the usage is its own text
	fs.Usage = func() {}
	fs.StringVar(&o.source, "source", sourceCRDs, "")
	fs.StringVar(&o.out, "out", "my-values.yaml", "")
	fs.StringVar(&o.dialect, "dialect", "", "")
	fs.StringVar(&o.agentURL, "agent-url", "", "")
	fs.StringVar(&o.agentNamespace, "agent-namespace", "", "")
	fs.StringVar(&o.teamNamespace, "team-namespace", "", "")
	fs.StringVar(&o.agentName, "agent-name", "", "")
	fs.StringVar(&o.channel, "channel", "", "")
	fs.StringVar(&o.namespace, "namespace", defaultInitNamespace, "")
	fs.StringVar(&o.tokenFile, "token-file", "", "")
	fs.BoolVar(&o.agentToken, "agent-token", false, "")
	fs.BoolVar(&o.force, "force", false, "")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "agent-token" {
			o.agentTokenSet = true
		}
	})
	o.out = strings.TrimSpace(o.out) // before any path is made from it
	switch {
	case fs.NArg() > 0:
		return o, errors.New("unexpected argument at position 1; the command takes options only")
	case o.out == "":
		return o, errors.New("--out is empty")
	case o.source != sourceCRDs && o.source != sourceFiles:
		return o, fmt.Errorf("--source %q is not %s or %s", o.source, sourceCRDs, sourceFiles)
	case o.source == sourceFiles && o.teamNamespace != "":
		return o, errors.New("--team-namespace is for --source crds: on the ConfigMap route every resource is in the release's namespace")
	}
	return o, nil
}

// initCommand is `agent-kourier init`: it returns the exit code, 0 for a written file, 2 for a wrong command line, input
// or file, 1 when Slack rejects the token, and 4 for anything else. Questions go to stderr and are answered on stdin; the
// commands to run next go to stdout.
func initCommand(args []string, ver string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	opts, err := parseInit(args)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprint(stderr, initUsage)
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent-kourier init: %v\nrun agent-kourier init --help for the usage\n", err)
		return exitUsage
	}
	return execInit(context.Background(), opts, ver, stdin, getenv, stdout, stderr)
}

// execInit does the work of the command and returns the exit code of initCommand.
func execInit(ctx context.Context, o initOptions, ver string, stdin io.Reader, getenv func(string) string, stdout, stderr io.Writer) int {
	if err := runInit(ctx, o, ver, stdin, getenv, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "agent-kourier init: %s\n", tokenShape.ReplaceAllString(err.Error(), "[redacted]"))
		return dryRunExitCode(err)
	}
	return exitOK
}

// runInit asks for what is missing, checks the values, writes the file and prints the next steps. Nothing is written
// unless the values load and, with a token, the bot can read the channel.
func runInit(ctx context.Context, o initOptions, ver string, stdin io.Reader, getenv func(string) string, stdout, stderr io.Writer) error {
	note := func(format string, args ...any) {
		_, _ = fmt.Fprintf(stderr, "agent-kourier init: "+format+"\n", args...)
	}
	token, haveToken, err := optionalBotToken(o.tokenFile, getenv)
	if err != nil {
		return usagef("%w", err)
	}
	// The values file is known before the questions, and so is the team's file when --team-namespace is given; else it
	// is checked after them.
	if err := refuseExisting(o.out, o.force, "name another file with --out"); err != nil {
		return err
	}
	if o.source == sourceCRDs && isNamespaceName(strings.TrimSpace(o.teamNamespace)) {
		early := o
		early.teamNamespace = strings.TrimSpace(o.teamNamespace)
		if err := refuseExisting(early.teamFile(), o.force, teamFileAdvice); err != nil {
			return err
		}
	}
	// A token in an option is refused before a question can show it as a default.
	if err := o.refuseTokens(); err != nil {
		return err
	}
	asker := &prompter{in: bufio.NewReader(stdin), out: stderr}
	if err := o.fill(asker, haveToken); err != nil {
		return err
	}
	if err := o.validate(); err != nil {
		return err
	}
	paths, err := o.paths()
	if err != nil {
		return err
	}
	for _, p := range paths[1:] {
		if err := refuseExisting(p, o.force, teamFileAdvice); err != nil {
			return err
		}
	}
	// The signals cancel the Slack requests from here on. They are not caught while the questions are asked: a question
	// waits on stdin, which no context ends, so a caught Ctrl-C there would be lost and the command would not stop.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var client *slack.Client
	if haveToken {
		client = slack.New(slack.Config{BotToken: token, APIURL: o.apiURL, HTTPClient: &http.Client{Timeout: 30 * time.Second}})
	}
	channel := o.channel
	if !v1alpha1.IsChannelID(channel) {
		if client == nil {
			return usagef("--channel %q is a name: set %s (or name a file with --token-file) so that Slack can look it up, or give the channel's ID, which starts with C (G for an older private channel)", channel, botTokenKey)
		}
		id, err := client.ChannelID(ctx, channel)
		if errors.Is(err, slack.ErrChannelNotFound) {
			return usagef("the bot sees no channel named %q: check the name, invite the bot to a private channel, or give the channel's ID", channel)
		}
		if err != nil {
			return fmt.Errorf("look up the channel %q: %w%s", channel, err, lookupHint(err))
		}
		note("the channel %q is %s", channel, id)
		channel = id
	}

	v := initValues{
		Dialect: o.dialect, AgentURL: o.agentURL, AgentNamespace: o.agentNamespace, AgentName: o.agentName,
		Channel: channel, AgentToken: o.agentToken, TeamNamespace: o.teamNamespace,
	}
	var files []initFile
	if o.source == sourceFiles {
		values, err := renderInitValues(v)
		if err != nil {
			return err
		}
		if _, err := loadInitValues(ctx, values, o.namespace); err != nil {
			return usagef("the values would not load: %w", err)
		}
		note("checked: Agent Kourier loads the resources that the chart renders from these values")
		files = []initFile{{paths[0], values}}
	} else {
		if files, err = renderInitResources(v, paths); err != nil {
			return err
		}
		if err := checkInitResources(ctx, files[1]); err != nil {
			return usagef("the resources would not load: %w", err)
		}
		note("checked: Agent Kourier loads %s, and the Binding's references to the ChatConnection and the AgentBackend resolve", filepath.Base(paths[1]))
	}

	if client != nil {
		// Reading from now on reaches conversations.history, and so finds a missing scope or an uninvited bot.
		conn := v1alpha1.NamespacedName(o.namespace, slackSecretName)
		if o.source == sourceCRDs {
			conn = v1alpha1.NamespacedName(o.teamNamespace, teamConnectionName(o.teamNamespace))
		}
		if _, err := client.History(ctx, conn, channel, time.Now()); err != nil {
			return fmt.Errorf("check that the bot can read %s: %w%s", channel, err, slackHint(err))
		}
		note("checked: the bot can read %s", channel)
	} else {
		note("no bot token (%s or --token-file), so Slack was not checked: the bot's access to %s is untested", botTokenKey, channel)
	}

	if err := writeInitFiles(files, o.force); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Wrote %s.\n\n%s", joinNames(paths), nextSteps(o, ver))
	return nil
}

// paths returns the files init writes: the values file, and on custom resources the team's file, named after the team's
// namespace, in its directory. The values file may not be the team's.
func (o initOptions) paths() ([]string, error) {
	if o.source == sourceFiles {
		return []string{o.out}, nil
	}
	// The team's file is in the values file's directory, so only the names can collide. The comparison ignores case, since
	// the file system may (macOS's does by default).
	if name := teamFileName(o.teamNamespace); strings.EqualFold(filepath.Base(o.out), name) {
		return nil, usagef("--out %s is where init writes the team's file, %s; name the values file otherwise", o.out, name)
	}
	return []string{o.out, o.teamFile()}, nil
}

// teamFileAdvice is how to go on when the team's file is in the way: --out names its directory, not its name.
const teamFileAdvice = "remove it, or give --out a path in another directory"

// teamFile is the path of the team's file on custom resources: named after the team's namespace, beside the values.
func (o initOptions) teamFile() string {
	return filepath.Join(filepath.Dir(o.out), teamFileName(o.teamNamespace))
}

// teamFileName is the name of the team's file for the team's namespace.
func teamFileName(team string) string { return team + ".yaml" }

// refuseExisting is the error for anything that exists at path, a symbolic link included, unless force says to overwrite
// it; elsewhere says how else to go on. Even with force only a regular file is overwritten, so that a run with several files does not stop after replacing
// some of them.
func refuseExisting(path string, force bool, elsewhere string) error {
	fi, err := os.Lstat(path) //nolint:gosec // G703: the operator names the files, in a directory of their choosing
	switch {
	case err != nil:
		return nil
	case !force:
		return usagef("%s exists; %s, or overwrite it with --force", path, elsewhere)
	case !fi.Mode().IsRegular():
		return usagef("%s is not a regular file, and --force overwrites only a file; %s", path, elsewhere)
	}
	return nil
}

// teamConnectionName is the name of the team's ChatConnection and of its Secret: the team-app guide's.
func teamConnectionName(team string) string { return team + "-slack" }

// joinNames returns the names as a list in a sentence: "a", "a and b", or "a, b and c".
func joinNames(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// prompter asks for values on a reader.
type prompter struct {
	in    *bufio.Reader
	out   io.Writer // the questions
	asked bool      // whether any question was asked
}

// ask writes the question, with the default in brackets if there is one, and returns the first answer that is not
// empty. An empty answer is the default if there is one, and else is asked again. At the end of the input it returns the
// default; ok is false if there is none and the value is not optional.
func (p *prompter) ask(label, def string, optional bool) (answer string, ok bool) {
	p.asked = true
	for {
		if def != "" {
			_, _ = fmt.Fprintf(p.out, "%s [%s]: ", label, def)
		} else {
			_, _ = fmt.Fprintf(p.out, "%s: ", label)
		}
		line, err := p.in.ReadString('\n')
		line = strings.TrimSpace(line)
		switch {
		case line != "":
			return line, true
		case def != "" || optional:
			return def, true
		case err != nil: // the end of the input, with nothing read
			_, _ = fmt.Fprintln(p.out)
			return "", false
		}
	}
}

// fill asks for each value that no option gave. A value that is still missing at the end of the input is a usage error
// that names its option. The question about the agent's token is asked only when other questions were, so that a command
// line that gives everything never waits on the terminal.
func (o *initOptions) fill(p *prompter, haveToken bool) error {
	channelLabel := "Slack channel ID (starts with C)"
	if haveToken {
		channelLabel = "Slack channel, by ID or by name"
	}
	type question struct {
		option, label string
		def           *string // the default, read when the question is asked: an earlier answer may give it
		dst           *string
	}
	dialect, none := "a2a", ""
	questions := []question{
		{"dialect", "Agent dialect (a2a or kagent-v1)", &dialect, &o.dialect},
		{"agent-url", "Agent URL", &none, &o.agentURL},
		{"agent-namespace", "Agent namespace", &none, &o.agentNamespace},
	}
	if o.source == sourceCRDs {
		questions = append(questions, question{"team-namespace", "Team namespace, where its Slack app and Binding go", &o.agentNamespace, &o.teamNamespace})
	}
	questions = append(questions,
		question{"agent-name", "Agent name", &none, &o.agentName},
		question{"channel", channelLabel, &none, &o.channel},
	)
	for _, f := range questions {
		if strings.TrimSpace(*f.dst) != "" {
			continue
		}
		answer, ok := p.ask(f.label, strings.TrimSpace(*f.def), false)
		if !ok {
			return usagef("--%s is required: give it, or answer when it is asked (the input ended)", f.option)
		}
		if tokenShape.MatchString(answer) { // refused before a later question can show it as a default
			return tokenRefusal(f.option)
		}
		*f.dst = answer
	}
	if p.asked && !o.agentTokenSet {
		answer, _ := p.ask("Does the agent verify tokens? (y/N)", "", true)
		o.agentToken = strings.HasPrefix(strings.ToLower(answer), "y")
	}
	return nil
}

// validate trims the values and refuses the ones that cannot be right. What the API types check (the URL, the names) is
// left to the check of the whole file; here are only the values that decide which file is written.
func (o *initOptions) validate() error {
	for _, v := range []*string{&o.dialect, &o.agentURL, &o.agentNamespace, &o.teamNamespace, &o.agentName, &o.channel, &o.namespace, &o.out} {
		*v = strings.TrimSpace(*v)
	}
	o.channel = strings.TrimPrefix(o.channel, "#")
	// A token in a value is refused before any value is echoed, here or by the check of the file.
	if err := o.refuseTokens(); err != nil {
		return err
	}
	switch {
	case !slices.Contains([]string{string(v1alpha1.DialectA2A), string(v1alpha1.DialectKagentV1)}, o.dialect):
		return usagef("--dialect %q is not %s or %s", o.dialect, v1alpha1.DialectA2A, v1alpha1.DialectKagentV1)
	case o.channel == "":
		return usagef("--channel is empty")
	case !isNamespaceName(o.namespace):
		return usagef("--namespace %q is not a namespace name: lower case letters, digits and dashes, at most 63", o.namespace)
	case o.source == sourceCRDs && !isNamespaceName(o.teamNamespace):
		return usagef("--team-namespace %q is not a namespace name: lower case letters, digits and dashes, at most 63", o.teamNamespace)
	}
	return nil
}

// refuseTokens refuses a value that looks like a Slack token, naming its option and not the value.
func (o *initOptions) refuseTokens() error {
	for _, v := range []struct{ option, value string }{
		{"dialect", o.dialect}, {"agent-url", o.agentURL}, {"agent-namespace", o.agentNamespace}, {"team-namespace", o.teamNamespace}, {"agent-name", o.agentName},
		{"channel", o.channel}, {"namespace", o.namespace}, {"out", o.out},
	} {
		if tokenShape.MatchString(v.value) {
			return tokenRefusal(v.option)
		}
	}
	return nil
}

// tokenRefusal is the usage error for a token given as the option's value or answer.
func tokenRefusal(option string) error {
	return usagef("--%s looks like a Slack token; a token is never an option or an answer", option)
}

// isNamespaceName reports whether s is a DNS-1123 label, what a namespace is called.
func isNamespaceName(s string) bool { return len(validation.IsDNS1123Label(s)) == 0 }

// optionalBotToken is loadBotToken for a command that works without a token: it reports whether there is one. A token
// file that was named, or a variable that is set, and is wrong is an error.
func optionalBotToken(file string, getenv func(string) string) (token secret.Value, ok bool, err error) {
	if file == "" && getenv(botTokenKey) == "" {
		return secret.Value{}, false, nil
	}
	token, err = loadBotToken(file, getenv)
	if err != nil {
		return secret.Value{}, false, err
	}
	return token, true, nil
}

// lookupHint is what to do about the error codes of listing channels that people meet, as a sentence that starts with a
// space, or nothing.
func lookupHint(err error) string {
	var slackErr slackgo.SlackErrorResponse
	if errors.As(err, &slackErr) && firstWord(slackErr.Err) == "missing_scope" {
		return " (the Slack app needs the scopes channels:read and groups:read to look a channel up by name; add them and reinstall the app, or give the channel's ID)"
	}
	return ""
}

// initValues are what the values file holds that a person chose.
type initValues struct {
	Dialect, AgentURL, AgentNamespace, AgentName, Channel string
	AgentToken                                            bool
	// TeamNamespace is the team's on custom resources, where its resources go; the ConfigMap route's values name none.
	TeamNamespace string
}

// initValuesTemplate is the values file: the starter file (examples/helm/my-values.yaml) with the person's values in it. A
// test holds the two together.
const initValuesTemplate = `# A first install of the Agent Kourier Helm chart: one Slack connection, one agent and one channel.
#
# Written by "agent-kourier init". The steps around it (the Secrets, a dry run that checks this file, the install) are in
# https://agentkourier.dev/how-to/install-with-helm/.
# Keep the file: an upgrade passes it again.
#
# This file is the quick route for a proof of concept: the chart writes the resources below into a ConfigMap. The chart's default,
# and the route to run on Kubernetes, reads them as custom resources instead (config.source crds), which other
# teams can add to; https://agentkourier.dev/explanation/config-routes/ says which to choose.
config:
  source: files
  # How Agent Kourier talks to Slack. The Secret holds the bot token and the app-level token under the keys botToken
  # and appToken, in the release's namespace.
  chatConnections:
    slack:
      spec:
        platform: slack
        credentialsSecretRef: {name: slack}

  # {{ .BackendComment }}
  agentBackends:
    agent:
      spec:
        dialect: {{ q .Dialect }}
        url: {{ q .AgentURL }}

  # Which channel talks to which agent.
  bindings:
    {{ .BindingName }}:
      spec:
        agent:
          backendRef: {name: agent}
          namespace: {{ q .AgentNamespace }}
          name: {{ q .AgentName }}
        identity:
{{- if .AgentToken }}
          # The identity Agent Kourier calls the agent as. The Secret holds the agent's token under the key token, in the
          # release's namespace.
          userId: agent-kourier
          tokenSecretRef: {name: {{ .TokenSecret }}}
{{- else }}
          # The identity Agent Kourier calls the agent as. If the agent verifies tokens, also set
          # tokenSecretRef: {name: {{ .TokenSecret }}}, a Secret with the token under the key token.
          userId: agent-kourier
{{- end }}
        chat:
          connectionRef: {name: slack}
          channel: {{ q .Channel }}
          output: live
        interactions: {askUser: false}
`

// The files that init writes on the chart's default route, custom resources, for a broker with no Slack app of its own:
// the values admit the team's namespace, and the team's file holds the team's ChatConnection, AgentBackend and Binding,
// all in that namespace. They are the files of the team-app guide's recorded path with the person's values in them; a
// test holds them together.
const (
	initCRValuesTemplate = `# A first install of the Agent Kourier Helm chart on custom resources, the chart's default route, for a broker
# with no Slack app of its own: it serves the ChatConnection that the team makes in its namespace with the team's app.
#
# Written by "agent-kourier init", with {{ .TeamFile }} beside it, which the team applies once the chart is installed. The
# steps are in https://agentkourier.dev/how-to/team-slack-app/#the-whole-path-recorded.
# Keep the file: an upgrade passes it again.
config:
  # The namespaces whose ChatConnections the broker serves. It has none of its own, so it names no config.chatConnection.
  connectionNamespaces: [{{ q .TeamNamespace }}]
`

	initTeamTemplate = `# The team's resources for Agent Kourier, all in the team's namespace: the team's Slack app, its agent, and which
# channel talks to which. Written by "agent-kourier init". Apply it once the chart is installed, after the Secret and the
# Role that grants it to the broker; the ChatConnection comes first.
apiVersion: {{ .APIVersion }}
kind: ChatConnection
metadata:
  name: {{ .ConnectionName }}
  namespace: {{ q .TeamNamespace }}
spec:
  platform: slack
  # The Secret holds the team's bot token and app-level token under the keys botToken and appToken, in this namespace.
  credentialsSecretRef: {name: {{ .ConnectionName }}}
---
apiVersion: {{ .APIVersion }}
kind: AgentBackend
metadata:
  name: {{ .BackendName }}
  namespace: {{ q .TeamNamespace }}
spec:
  dialect: {{ q .Dialect }}
  # {{ .BackendComment }}
  url: {{ q .AgentURL }}
---
apiVersion: {{ .APIVersion }}
kind: Binding
metadata:
  name: {{ .BindingName }}
  namespace: {{ q .TeamNamespace }}
spec:
  agent:
    backendRef: {name: {{ .BackendName }}}
    namespace: {{ q .AgentNamespace }}
    name: {{ q .AgentName }}
  identity:
{{- if .AgentToken }}
    # The identity Agent Kourier calls the agent as. The Secret holds the agent's token under the key token, in this
    # namespace.
    userId: agent-kourier
    tokenSecretRef: {name: {{ .TokenSecret }}}
{{- else }}
    # The identity Agent Kourier calls the agent as. If the agent verifies tokens, also set
    # tokenSecretRef: {name: {{ .TokenSecret }}}, a Secret in this namespace with the token under the key token.
    userId: agent-kourier
{{- end }}
  chat:
    connectionRef: {name: {{ .ConnectionName }}}
    channel: {{ q .Channel }}
    output: live
  interactions: {askUser: false}
`
)

// initBackendName is the name of the AgentBackend that init writes on either route.
const initBackendName = "agent"

// initFile is a file that init writes, and what it holds.
type initFile struct {
	path string
	data []byte
}

// renderInitValues writes the values file of the ConfigMap route for v.
func renderInitValues(v initValues) ([]byte, error) {
	return renderInitTemplate("values", initValuesTemplate, v)
}

// renderInitResources writes the two files of the custom-resource route for v, at paths: the values and the team's file.
func renderInitResources(v initValues, paths []string) ([]initFile, error) {
	var files []initFile
	for i, tmpl := range []string{initCRValuesTemplate, initTeamTemplate} {
		data, err := renderInitTemplate(filepath.Base(paths[i]), tmpl, v)
		if err != nil {
			return nil, err
		}
		files = append(files, initFile{paths[i], data})
	}
	return files, nil
}

// renderInitTemplate executes the template text of the file name with v and the names that every file of init shares.
func renderInitTemplate(name, text string, v initValues) ([]byte, error) {
	t, err := template.New(name).Funcs(template.FuncMap{"q": yamlScalar}).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parse the template of %s: %w", name, err)
	}
	comment := "Where the agent is: its A2A endpoint, called exactly as written (keep a trailing slash if the route has one)."
	if v.Dialect != string(v1alpha1.DialectA2A) {
		comment = "Where the agent is: the URL of the kagent front door (https://agentkourier.dev/how-to/connect-kagent/)."
	}
	data := struct {
		initValues
		BindingName, TokenSecret, BackendComment, APIVersion, ConnectionName, BackendName, TeamFile string
	}{
		v, initBindingName, agentTokenSecretName, comment, resourceAPIVersion, teamConnectionName(v.TeamNamespace), initBackendName,
		teamFileName(v.TeamNamespace),
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("write %s: %w", name, err)
	}
	return b.Bytes(), nil
}

// checkInitResources loads the team's file with the loader of the broker, which defaults and validates each object, and
// resolves the references between its objects as the broker does, with every Secret taken as present. The error names the
// file, the resource and the field.
func checkInitResources(ctx context.Context, f initFile) error {
	dir, err := os.MkdirTemp("", "agent-kourier-init-")
	if err != nil {
		return fmt.Errorf("make a directory for the check: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(f.path)), f.data, 0o600); err != nil {
		return fmt.Errorf("write %s for the check: %w", filepath.Base(f.path), err)
	}
	set, err := config.Load(ctx, dir)
	if err != nil {
		return err
	}
	_, err = config.Resolve(ctx, set, config.PresentSecrets(set))
	return err
}

// plainScalar is what may be written in a YAML file without quotes, when YAML also reads it back as the same string.
var plainScalar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*$`)

// yamlScalar returns s as a YAML scalar that reads back as the string s: as it is if it is plain and no other type (a
// number, true, null), else as a double-quoted string, in which a newline, a quote or a comment sign are only text.
func yamlScalar(s string) string {
	if plainScalar.MatchString(s) {
		// Read into any, not into a string: reading a number or true into a string gives its text.
		var back any
		if err := yaml.Unmarshal([]byte(s), &back); err == nil && back == any(s) {
			return s
		}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes; JSON's double-quoted strings are YAML's
	return strings.TrimSuffix(b.String(), "\n")
}

// loadInitValues does for the values what the chart and the broker do with them: each entry of config.agentBackends,
// config.chatConnections and config.bindings becomes a resource file as the chart's agentkourier.resource writes it,
// in namespace unless the entry has one, and the files are loaded with the loader of the broker, which defaults and
// validates them. The error is the loader's, which names the file, the resource and the field.
func loadInitValues(ctx context.Context, values []byte, namespace string) (*config.Set, error) {
	type entries map[string]struct {
		Namespace string         `json:"namespace"`
		Spec      map[string]any `json:"spec"`
	}
	// config.source, and the other keys of the section that are no resource, are the chart's to read.
	var doc struct {
		Config struct {
			AgentBackends   entries `json:"agentBackends"`
			ChatConnections entries `json:"chatConnections"`
			Bindings        entries `json:"bindings"`
		} `json:"config"`
	}
	if err := yaml.Unmarshal(values, &doc); err != nil {
		return nil, fmt.Errorf("read the values: %w", err)
	}
	dir, err := os.MkdirTemp("", "agent-kourier-init-")
	if err != nil {
		return nil, fmt.Errorf("make a directory for the check: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for _, k := range []struct {
		entries entries
		kind    string
	}{{doc.Config.AgentBackends, "AgentBackend"}, {doc.Config.ChatConnections, "ChatConnection"}, {doc.Config.Bindings, "Binding"}} {
		for name, entry := range k.entries {
			obj := map[string]any{
				"apiVersion": resourceAPIVersion, "kind": k.kind,
				"metadata": map[string]any{"name": name, "namespace": cmp.Or(entry.Namespace, namespace)},
				"spec":     entry.Spec,
			}
			b, err := yaml.Marshal(obj)
			if err != nil {
				return nil, fmt.Errorf("write %s %s: %w", k.kind, name, err)
			}
			if err := os.WriteFile(filepath.Join(dir, strings.ToLower(k.kind)+"-"+name+".yaml"), b, 0o600); err != nil {
				return nil, fmt.Errorf("write %s %s: %w", k.kind, name, err)
			}
		}
	}
	return config.Load(ctx, dir)
}

// writeInitFiles writes each file as writeInitFile does. Without force, a file that cannot be written removes the files
// written before it, so that a failed run leaves none of them; with force, a file it replaced stays replaced.
func writeInitFiles(files []initFile, force bool) error {
	for i, f := range files {
		if err := writeInitFile(f.path, f.data, force); err != nil {
			if !force {
				for _, done := range files[:i] {
					_ = os.Remove(done.path)
				}
			}
			return err
		}
	}
	return nil
}

// writeInitFile writes data to path. Without force it creates the file and fails if there is one; with force it
// replaces one. A symbolic link is never followed.
func writeInitFile(path string, data []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL | syscall.O_NOFOLLOW
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC | syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0o644) //nolint:gosec // the operator names the file; it holds no secret
	if errors.Is(err, os.ErrExist) {
		return usagef("%s exists; remove it, or overwrite it with --force", path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if !force { // the file is this run's: leave no half of it
			_ = os.Remove(path)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// plainShellWord is what a POSIX shell reads as one word, unchanged.
var plainShellWord = regexp.MustCompile(`^[A-Za-z0-9_./=:@%+-]+$`)

// shellWord returns s as one word of a POSIX shell command: as it is if it is plain, else in single quotes.
func shellWord(s string) string {
	if plainShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// nextSteps is the text of the commands to run after the file is written: the namespace, the Secrets and the install.
// The install names the chart version of the release that ver is. A build that is no release (dev) names none, and
// says so, and Helm then takes the newest chart.
func nextSteps(o initOptions, ver string) string {
	if o.source == sourceCRDs {
		return teamAppNextSteps(o, ver)
	}
	var b strings.Builder
	b.WriteString("Next, with SLACK_BOT_TOKEN and SLACK_APP_TOKEN set (see https://agentkourier.dev/how-to/configure-slack/):\n\n")
	fmt.Fprintf(&b, "kubectl create namespace %s\n", o.namespace)
	fmt.Fprintf(&b, "kubectl -n %s create secret generic %s \\\n  --from-literal=botToken=\"$SLACK_BOT_TOKEN\" \\\n  --from-literal=appToken=\"$SLACK_APP_TOKEN\"\n", o.namespace, slackSecretName)
	if o.agentToken {
		fmt.Fprintf(&b, "\n# AGENT_TOKEN is the bearer token the agent accepts for the Binding's identity\nkubectl -n %s create secret generic %s --from-literal=token=\"$AGENT_TOKEN\"\n", o.namespace, agentTokenSecretName)
	}
	b.WriteString("\n")
	writeHelmInstall(&b, o, ver)
	return b.String()
}

// teamAppNextSteps is nextSteps on custom resources, as the team-app guide's recorded path runs them: the platform team
// installs the broker, and the team makes its Slack app's Secret, grants it (and the agent's token, if there is one) to the
// broker's ServiceAccount, and applies its file. The ServiceAccount is the chart's for the release agent-kourier.
func teamAppNextSteps(o initOptions, ver string) string {
	team, conn := o.teamNamespace, teamConnectionName(o.teamNamespace)
	role := "agent-kourier-secrets-" + team
	var b strings.Builder
	fmt.Fprintf(&b, "Next, as the platform team, the namespace and the broker, which serves the ChatConnections of %s:\n\n", team)
	fmt.Fprintf(&b, "kubectl create namespace %s\n", o.namespace)
	writeHelmInstall(&b, o, ver)
	fmt.Fprintf(&b, "\nThen, as the team, in its namespace %s, which must exist (see https://agentkourier.dev/how-to/team-slack-app/):\n\n", team)
	grant := " --resource-name=" + conn
	if o.agentToken {
		fmt.Fprintf(&b, "# AGENT_TOKEN is the bearer token the agent accepts for the Binding's identity; the Secret goes in the Binding's namespace\nkubectl -n %s create secret generic %s --from-literal=token=\"$AGENT_TOKEN\"\n\n", team, agentTokenSecretName)
		grant += " --resource-name=" + agentTokenSecretName
	}
	b.WriteString("# The team's Slack app: a file that only you can read, with the lines botToken=<bot token> and appToken=<app-level token>\n")
	fmt.Fprintf(&b, "kubectl -n %s create secret generic %s \\\n  --from-env-file=\"$HOME/.config/%s/slack-secret.env\"\n", team, conn, team)
	fmt.Fprintf(&b, "kubectl -n %s create role %s --verb=list,watch \\\n  --resource=secrets%s\n", team, role, grant)
	fmt.Fprintf(&b, "kubectl -n %s create rolebinding %s --role=%s \\\n  --serviceaccount=%s:%s\n", team, role, role, o.namespace, initReleaseName)
	file := shellWord(o.teamFile())
	fmt.Fprintf(&b, "\nkubectl apply --dry-run=server -f %s\nkubectl apply -f %s\nkubectl -n %s get akbinding\n", file, file, team)
	return b.String()
}

// writeHelmInstall writes the helm install command of the release. It names the chart version of the release that ver is;
// a build that is no release (dev) names none, and says so, and Helm then takes the newest chart.
func writeHelmInstall(b *strings.Builder, o initOptions, ver string) {
	version := ""
	if m := releaseVersion.FindStringSubmatch(ver); m != nil {
		version = " --version " + m[1]
	} else {
		fmt.Fprintf(b, "# This build is %q, not a release, so no chart version is named: Helm takes the newest chart.\n", ver)
	}
	fmt.Fprintf(b, "helm install %s %s%s \\\n  -n %s -f %s\n", initReleaseName, chartReference, version, o.namespace, shellWord(o.out))
}
