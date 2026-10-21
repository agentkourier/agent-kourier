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
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// initUsage is what `agent-kourier init --help` prints.
const initUsage = `usage: agent-kourier init [options]

Writes a first-install values file for the Helm chart (one Slack connection, one agent, one channel), checks it, and
prints the commands that create the Secrets and install the chart. It posts nothing and changes nothing in Slack or in
a cluster. A value that no option gives is asked for on standard input.

  --out <path>              the file to write (default: my-values.yaml); an existing file is not overwritten
  --force                   overwrite the file
  --dialect <name>          the agent's dialect: a2a or kagent-v1 (default when asked: a2a)
  --agent-url <url>         the agent's URL, called exactly as written
  --agent-namespace <ns>    the agent's namespace
  --agent-name <name>       the agent's name
  --channel <id|name>       the channel, by ID (C... or G...), or by name, which is looked up in Slack and needs a bot token
  --agent-token             the agent verifies tokens: the Binding reads one from the Secret agent-kourier-agent-token
  --namespace <ns>          the namespace of the release (default: agent-kourier): the resources without a namespace of their
                            own are checked in it, and the commands it prints name it
  --token-file <path>       a file that holds the bot token, alone or as SLACK_BOT_TOKEN=... (mode 600)

With a bot token, from --token-file or $SLACK_BOT_TOKEN, init also checks that the bot can read the channel (the bot needs
channels:history, or groups:history for a private channel, and must be a member of the channel), and a channel name is
looked up (channels:read and groups:read). The token is never an argument and never printed. Without one the Slack checks are
skipped, and a channel must be given by ID.
`

// defaultInitNamespace is the namespace of the release when --namespace is not given: the one the install guide uses.
const defaultInitNamespace = "agent-kourier"

// The names the generated values and the printed commands use; they are the ones the install guide and the starter file use.
const (
	slackSecretName      = "agent-kourier-slack"       //nolint:gosec // G101: the name of a Secret, not a credential
	agentTokenSecretName = "agent-kourier-agent-token" //nolint:gosec // G101: the name of a Secret, not a credential
	chartReference       = "oci://ghcr.io/agentkourier/charts/agent-kourier"
	initBindingName      = "my-binding"
)

// resourceAPIVersion is the apiVersion the chart gives each resource file (templates/_helpers.tpl, agentkourier.resource).
const resourceAPIVersion = "agentkourier.dev/v1alpha1"

// initChannelID is what api/v1alpha1 accepts as a channel ID in a Binding; anything else is a name.
var initChannelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

// namespaceName is a DNS-1123 label, what a namespace is.
var namespaceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// releaseVersion matches a version of a release, as the binary's --version prints it: v0.1.1 or 0.1.1-rc.1.
var releaseVersion = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$`)

// initOptions are the options of `agent-kourier init`.
type initOptions struct {
	out, dialect, agentURL, agentNamespace, agentName, channel, namespace, tokenFile string
	agentToken, force                                                                bool
	agentTokenSet                                                                    bool // --agent-token was on the command line

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
	fs.StringVar(&o.out, "out", "my-values.yaml", "")
	fs.StringVar(&o.dialect, "dialect", "", "")
	fs.StringVar(&o.agentURL, "agent-url", "", "")
	fs.StringVar(&o.agentNamespace, "agent-namespace", "", "")
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
	switch {
	case fs.NArg() > 0:
		return o, errors.New("unexpected argument at position 1; the command takes options only")
	case o.out == "":
		return o, errors.New("--out is empty")
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
	if !o.force {
		if _, err := os.Lstat(o.out); err == nil {
			return usagef("%s exists; name another file with --out, or overwrite it with --force", o.out)
		}
	}
	asker := &prompter{in: bufio.NewReader(stdin), out: stderr}
	if err := o.fill(asker, haveToken); err != nil {
		return err
	}
	if err := o.validate(); err != nil {
		return err
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
	if !initChannelID.MatchString(channel) {
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

	values, err := renderInitValues(initValues{
		Dialect: o.dialect, AgentURL: o.agentURL, AgentNamespace: o.agentNamespace, AgentName: o.agentName,
		Channel: channel, AgentToken: o.agentToken,
	})
	if err != nil {
		return err
	}
	if _, err := loadInitValues(ctx, values, o.namespace); err != nil {
		return usagef("the values would not load: %w", err)
	}
	note("checked: Agent Kourier loads the resources that the chart renders from these values")

	if client != nil {
		// Reading from now on reaches conversations.history, and so finds a missing scope or an uninvited bot.
		conn := v1alpha1.NamespacedName(o.namespace, "slack")
		if _, err := client.History(ctx, conn, channel, time.Now()); err != nil {
			return fmt.Errorf("check that the bot can read %s: %w%s", channel, err, slackHint(err))
		}
		note("checked: the bot can read %s", channel)
	} else {
		note("no bot token (%s or --token-file), so Slack was not checked: the bot's access to %s is untested", botTokenKey, channel)
	}

	if err := writeInitFile(o.out, values, o.force); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Wrote %s.\n\n%s", o.out, nextSteps(o, ver))
	return nil
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
	for _, f := range []struct {
		option, label, def string
		dst                *string
	}{
		{"dialect", "Agent dialect (a2a or kagent-v1)", "a2a", &o.dialect},
		{"agent-url", "Agent URL", "", &o.agentURL},
		{"agent-namespace", "Agent namespace", "", &o.agentNamespace},
		{"agent-name", "Agent name", "", &o.agentName},
		{"channel", channelLabel, "", &o.channel},
	} {
		if strings.TrimSpace(*f.dst) != "" {
			continue
		}
		answer, ok := p.ask(f.label, f.def, false)
		if !ok {
			return usagef("--%s is required: give it, or answer when it is asked (the input ended)", f.option)
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
	for _, v := range []*string{&o.dialect, &o.agentURL, &o.agentNamespace, &o.agentName, &o.channel, &o.namespace, &o.out} {
		*v = strings.TrimSpace(*v)
	}
	o.channel = strings.TrimPrefix(o.channel, "#")
	// A token in a value is refused before any value is echoed, here or by the check of the file.
	for _, v := range []struct{ option, value string }{
		{"dialect", o.dialect}, {"agent-url", o.agentURL}, {"agent-namespace", o.agentNamespace}, {"agent-name", o.agentName},
		{"channel", o.channel}, {"namespace", o.namespace}, {"out", o.out},
	} {
		if tokenShape.MatchString(v.value) {
			return usagef("--%s looks like a Slack token; a token is never an option or an answer", v.option)
		}
	}
	switch {
	case !slices.Contains([]string{string(v1alpha1.DialectA2A), string(v1alpha1.DialectKagentV1)}, o.dialect):
		return usagef("--dialect %q is not %s or %s", o.dialect, v1alpha1.DialectA2A, v1alpha1.DialectKagentV1)
	case o.channel == "":
		return usagef("--channel is empty")
	case !namespaceName.MatchString(o.namespace) || len(o.namespace) > 63:
		return usagef("--namespace %q is not a namespace name: lower case letters, digits and dashes, at most 63", o.namespace)
	}
	return nil
}

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
}

// initValuesTemplate is the values file: the starter file (examples/helm/my-values.yaml) with the person's values in it. A
// test holds the two together.
const initValuesTemplate = `# A first install of the Agent Kourier Helm chart: one Slack connection, one agent and one channel.
#
# Written by "agent-kourier init". The steps around it (the Secrets, a dry run that checks this file, the install) are in
# https://agentkourier.dev/how-to/install-with-helm/.
# Keep the file: an upgrade passes it again.
config:
  # How Agent Kourier talks to Slack. The Secret holds the bot token and the app-level token under the keys botToken
  # and appToken, in the release's namespace.
  chatConnections:
    slack:
      spec:
        platform: slack
        credentialsSecretRef: {name: agent-kourier-slack}

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

// renderInitValues writes the values file for v.
func renderInitValues(v initValues) ([]byte, error) {
	t, err := template.New("values").Funcs(template.FuncMap{"q": yamlScalar}).Parse(initValuesTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse the values template: %w", err)
	}
	var b bytes.Buffer
	comment := "Where the agent is: its A2A endpoint, called exactly as written (keep a trailing slash if the route has one)."
	if v.Dialect != string(v1alpha1.DialectA2A) {
		comment = "Where the agent is: the URL of the kagent front door (https://agentkourier.dev/how-to/connect-kagent/)."
	}
	data := struct {
		initValues
		BindingName, TokenSecret, BackendComment string
	}{v, initBindingName, agentTokenSecretName, comment}
	if err := t.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("write the values: %w", err)
	}
	return b.Bytes(), nil
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
	var doc struct {
		Config map[string]map[string]struct {
			Namespace string         `json:"namespace"`
			Spec      map[string]any `json:"spec"`
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
	for _, k := range []struct{ key, kind string }{{"agentBackends", "AgentBackend"}, {"chatConnections", "ChatConnection"}, {"bindings", "Binding"}} {
		for name, entry := range doc.Config[k.key] {
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

// writeInitFile writes data to path. Without force it creates the file and fails if there is one; with force it
// replaces one. A symbolic link is never followed.
func writeInitFile(path string, data []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL | syscall.O_NOFOLLOW
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC | syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0o644) //nolint:gosec // the operator names the file; it holds no secret
	if errors.Is(err, os.ErrExist) {
		return usagef("%s exists; name another file with --out, or overwrite it with --force", path)
	}
	if err != nil {
		return fmt.Errorf("write the values: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
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
	var b strings.Builder
	b.WriteString("Next, with SLACK_BOT_TOKEN and SLACK_APP_TOKEN set (see https://agentkourier.dev/how-to/configure-slack/):\n\n")
	fmt.Fprintf(&b, "kubectl create namespace %s\n", o.namespace)
	fmt.Fprintf(&b, "kubectl -n %s create secret generic %s \\\n  --from-literal=botToken=\"$SLACK_BOT_TOKEN\" \\\n  --from-literal=appToken=\"$SLACK_APP_TOKEN\"\n", o.namespace, slackSecretName)
	if o.agentToken {
		fmt.Fprintf(&b, "\n# AGENT_TOKEN is the bearer token the agent accepts for the Binding's identity\nkubectl -n %s create secret generic %s --from-literal=token=\"$AGENT_TOKEN\"\n", o.namespace, agentTokenSecretName)
	}
	b.WriteString("\n")
	version := ""
	if m := releaseVersion.FindStringSubmatch(ver); m != nil {
		version = " --version " + m[1]
	} else {
		fmt.Fprintf(&b, "# This build is %q, not a release, so no chart version is named: Helm takes the newest chart.\n", ver)
	}
	fmt.Fprintf(&b, "helm install agent-kourier %s%s \\\n  -n %s -f %s\n", chartReference, version, o.namespace, shellWord(o.out))
	return b.String()
}
