package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// starterFile is the first-install values file that the install guide offers to download.
const starterFile = "../../examples/helm/my-values.yaml"

// starterURL is the agent URL of the starter file.
const starterURL = "http://my-agent.my-namespace.svc.cluster.local:8083/a2a/"

// initSlack is a Slack that knows who the bot is, lists channels and reads the history of any channel, unless a
// field makes one of them fail. Each error may quote the token, as a hostile or broken server could.
type initSlack struct {
	channels                         []map[string]string
	authErr, listErr, historyErr     string
	mu                               sync.Mutex
	calls, historyChannels, historyO []string // the methods called, and the channel and oldest of each history request
}

func (s *initSlack) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		s.mu.Lock()
		s.calls = append(s.calls, method)
		s.mu.Unlock()
		fail := func(code string) {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
		}
		switch method {
		case "auth.test":
			if s.authErr != "" {
				fail(s.authErr)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"UFAKEBOT","bot_id":"BFAKEBOT"}`))
		case "conversations.list":
			if s.listErr != "" {
				fail(s.listErr)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": s.channels, "response_metadata": map[string]string{"next_cursor": ""}})
		case "conversations.history":
			s.mu.Lock()
			s.historyChannels = append(s.historyChannels, r.Form.Get("channel"))
			s.historyO = append(s.historyO, r.Form.Get("oldest"))
			s.mu.Unlock()
			if s.historyErr != "" {
				fail(s.historyErr)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"messages":[],"has_more":false}`))
		default:
			fail("unknown_method")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

// initRun runs `agent-kourier init args` for a binary of version 0.1.1, against the Slack at slackURL (none if empty),
// with stdin as the input and the environment getenv, and returns what it printed and its exit code.
func initRun(t *testing.T, slackURL string, getenv func(string) string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	opts, err := parseInit(args)
	if err != nil {
		return "", err.Error(), exitUsage
	}
	opts.apiURL = slackURL
	var out, errb bytes.Buffer
	code = execInit(t.Context(), opts, "v0.1.1", strings.NewReader(stdin), getenv, &out, &errb)
	return out.String(), errb.String(), code
}

// starterArgs are the options that give init the starter file's values, and write to out.
func starterArgs(out string, extra ...string) []string {
	return append([]string{
		"--out", out, "--dialect", "a2a", "--agent-url", starterURL, "--agent-namespace", "my-namespace",
		"--agent-name", "my-agent", "--channel", "C0123456789",
	}, extra...)
}

func outFile(t *testing.T) string { return filepath.Join(t.TempDir(), "my-values.yaml") }

// loadWritten loads the values file as the chart and the broker would.
func loadWritten(t *testing.T, path string) *config.Set {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	set, err := loadInitValues(t.Context(), data, "agent-kourier")
	if err != nil {
		t.Fatalf("%s does not load: %v\n%s", path, err, data)
	}
	return set
}

// With the starter's values, init writes the starter: the two files are the same data, so that they cannot drift apart.
func TestInitWithTheStartersValuesWritesTheStarter(t *testing.T) {
	out := outFile(t)
	stdout, stderr, code := initRun(t, "", env{}.get, "", starterArgs(out)...)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(starterFile)
	if err != nil {
		t.Fatal(err)
	}
	var gotData, wantData any
	if err := yaml.Unmarshal(got, &gotData); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(want, &wantData); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotData, wantData) {
		t.Errorf("init's file is not the starter:\ninit:    %v\nstarter: %v", gotData, wantData)
	}
	if !strings.Contains(string(got), "\n  # ") {
		t.Errorf("the file has no comments:\n%s", got)
	}
	for _, want := range []string{"Wrote " + out, "kubectl create namespace agent-kourier", "helm install agent-kourier " + chartReference + " --version 0.1.1 \\\n  -n agent-kourier -f " + out} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "Slack was not checked") {
		t.Errorf("stderr %q does not say that Slack was skipped", stderr)
	}
	if strings.Contains(stdout, agentTokenSecretName+" --from-literal") {
		t.Errorf("stdout creates the agent token Secret without --agent-token:\n%s", stdout)
	}
}

// For each dialect, the file loads with the broker's loader and says what was asked, with and without the agent token.
func TestInitWritesAFileThatLoads(t *testing.T) {
	for _, tc := range []struct {
		name, dialect string
		agentToken    bool
	}{
		{"a2a", "a2a", false},
		{"a2a with an agent token", "a2a", true},
		{"kagent-v1", "kagent-v1", false},
		{"kagent-v1 with an agent token", "kagent-v1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := outFile(t)
			args := []string{
				"--out", out, "--dialect", tc.dialect, "--agent-url", "http://kagent-frontdoor.kagent.svc.cluster.local:4180",
				"--agent-namespace", "payments", "--agent-name", "payments-agent", "--channel", "G0ABCDEF123", "--namespace", "kourier",
			}
			if tc.agentToken {
				args = append(args, "--agent-token")
			}
			stdout, stderr, code := initRun(t, "", env{}.get, "", args...)
			if code != exitOK {
				t.Fatalf("exit code %d, stderr %q", code, stderr)
			}
			set := loadWritten(t, out)
			if len(set.AgentBackends) != 1 || len(set.ChatConnections) != 1 || len(set.Bindings) != 1 {
				t.Fatalf("loaded %d AgentBackends, %d ChatConnections and %d Bindings; want one of each", len(set.AgentBackends), len(set.ChatConnections), len(set.Bindings))
			}
			if be := set.AgentBackends[0]; string(be.Spec.Dialect) != tc.dialect || be.Spec.URL != "http://kagent-frontdoor.kagent.svc.cluster.local:4180" {
				t.Errorf("AgentBackend = %+v; want dialect %s and the URL", be.Spec, tc.dialect)
			}
			b := set.Bindings[0]
			if b.Spec.Agent.Namespace != "payments" || b.Spec.Agent.Name != "payments-agent" || b.Spec.Chat.Channel != "G0ABCDEF123" {
				t.Errorf("Binding = %+v; want the agent payments/payments-agent and channel G0ABCDEF123", b.Spec)
			}
			ref := b.Spec.Identity.TokenSecretRef
			switch {
			case tc.agentToken && (ref == nil || ref.Name != agentTokenSecretName || ref.Key != "token"):
				t.Errorf("tokenSecretRef = %+v; want %s, key token", ref, agentTokenSecretName)
			case !tc.agentToken && ref != nil:
				t.Errorf("tokenSecretRef = %+v; want none", ref)
			}
			if got := strings.Contains(stdout, "create secret generic "+agentTokenSecretName); got != tc.agentToken {
				t.Errorf("stdout creates the agent token Secret = %v; want %v:\n%s", got, tc.agentToken, stdout)
			}
			if !strings.Contains(stdout, "kubectl -n kourier create secret generic "+slackSecretName) || !strings.Contains(stdout, "-n kourier -f "+out) {
				t.Errorf("stdout does not name the namespace kourier:\n%s", stdout)
			}
		})
	}
}

// What the options leave out is asked on stdin, with the default shown, and the answers make the file. The question about
// the agent's token is asked only when another was.
func TestInitAsksForWhatIsMissing(t *testing.T) {
	out := outFile(t)
	// the dialect takes its default (an empty line), the channel is the last
	stdin := "\nhttp://agent.agents.svc:8083/a2a/\nagents\nsre-agent\n#C0123456789\ny\n"
	stdout, stderr, code := initRun(t, "", env{}.get, stdin, "--out", out)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	for _, q := range []string{"Agent dialect (a2a or kagent-v1) [a2a]: ", "Agent URL: ", "Agent namespace: ", "Agent name: ", "Slack channel ID (starts with C): ", "Does the agent verify tokens?"} {
		if !strings.Contains(stderr, q) {
			t.Errorf("stderr lacks the question %q:\n%s", q, stderr)
		}
	}
	b := loadWritten(t, out).Bindings[0]
	if b.Spec.Agent.Namespace != "agents" || b.Spec.Agent.Name != "sre-agent" || b.Spec.Chat.Channel != "C0123456789" {
		t.Errorf("Binding = %+v", b.Spec)
	}
	if b.Spec.Identity.TokenSecretRef == nil {
		t.Error("the answer y to the token question did not set tokenSecretRef")
	}
	if !strings.Contains(stdout, "create secret generic "+agentTokenSecretName) {
		t.Errorf("stdout lacks the agent token Secret:\n%s", stdout)
	}

	// Only the missing value is asked, and the end of the input answers the question about the token with no.
	out = outFile(t)
	_, stderr, code = initRun(t, "", env{}.get, "C0123456789\n", starterArgs(out, "--channel", "")...)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if strings.Contains(stderr, "Agent URL") || !strings.Contains(stderr, "Slack channel ID") {
		t.Errorf("stderr %q asks for more or less than the channel", stderr)
	}
	if loadWritten(t, out).Bindings[0].Spec.Identity.TokenSecretRef != nil {
		t.Error("the end of the input set tokenSecretRef")
	}

	// An option that gives everything asks nothing.
	_, stderr, code = initRun(t, "", env{}.get, "", starterArgs(outFile(t))...)
	if code != exitOK || strings.Contains(stderr, "?") || strings.Contains(stderr, ": \n") {
		t.Errorf("exit code %d, stderr %q; want no questions", code, stderr)
	}
}

// A value that is missing at the end of the input is a usage error that names its option, and nothing is written.
func TestInitAtTheEndOfTheInputNamesTheMissingOption(t *testing.T) {
	for _, tc := range []struct {
		name, stdin string
		args        []string
		want        string
	}{
		{"nothing given", "", nil, "--agent-url"},
		{"only the dialect and the URL", "a2a\nhttp://a.example.com\n", nil, "--agent-namespace"},
		{"all but the channel", "", []string{"--dialect", "a2a", "--agent-url", starterURL, "--agent-namespace", "n", "--agent-name", "a"}, "--channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := outFile(t)
			stdout, stderr, code := initRun(t, "", env{}.get, tc.stdin, append([]string{"--out", out}, tc.args...)...)
			if code != exitUsage || !strings.Contains(stderr, tc.want+" is required") || stdout != "" {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d and %q is required", code, stdout, stderr, exitUsage, tc.want)
			}
			if _, err := os.Stat(out); err == nil {
				t.Error("a file was written")
			}
		})
	}
}

// A wrong value is a usage error that says which option, and nothing is written.
func TestInitRefusesWrongValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"a dialect that does not exist", []string{"--dialect", "grpc"}, `--dialect "grpc" is not a2a or kagent-v1`},
		{"an empty channel", []string{"--channel", "#"}, "--channel is empty"},
		{"a namespace that is no name", []string{"--namespace", "Not_A_Name"}, "--namespace"},
		{"an agent URL that is no URL", []string{"--agent-url", "my-agent"}, "the values would not load"},
		{"an agent URL with credentials", []string{"--agent-url", "http://user:pass@agent.example.com/"}, "credentials"},
		{"an agent namespace that is no name", []string{"--agent-namespace", "My Namespace"}, "spec.agent.namespace"},
		{"an agent URL that holds a line break", []string{"--agent-url", "http://agent.example.com/\nevil: 1"}, "the values would not load"},
		{"a stray argument", []string{"extra"}, "unexpected argument at position 1"},
		{"an empty --out", []string{"--out", ""}, "--out is empty"},
		{"an option that does not exist", []string{"--bot-token", "x"}, "not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := outFile(t)
			stdout, stderr, code := initRun(t, "", env{}.get, "", starterArgs(out, tc.extra...)...)
			if code != exitUsage || !strings.Contains(stderr, tc.want) || stdout != "" {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d and %q", code, stdout, stderr, exitUsage, tc.want)
			}
			if _, err := os.Stat(out); err == nil {
				t.Error("a file was written")
			}
		})
	}
}

// An existing file is neither overwritten nor questioned about, unless --force says so.
func TestInitDoesNotOverwriteWithoutForce(t *testing.T) {
	out := outFile(t)
	if err := os.WriteFile(out, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := initRun(t, "", env{}.get, "", "--out", out)
	if code != exitUsage || !strings.Contains(stderr, "--force") || stdout != "" || strings.Contains(stderr, "Agent URL") {
		t.Errorf("exit code %d, stdout %q, stderr %q; want %d, --force named and no question", code, stdout, stderr, exitUsage)
	}
	if got, _ := os.ReadFile(out); string(got) != "mine\n" {
		t.Errorf("the file is now %q", got)
	}
	if _, stderr, code := initRun(t, "", env{}.get, "", starterArgs(out)...); code != exitUsage || !strings.Contains(stderr, "--force") {
		t.Errorf("exit code %d, stderr %q; want %d and --force named", code, stderr, exitUsage)
	}
	if got, _ := os.ReadFile(out); string(got) != "mine\n" {
		t.Errorf("the file is now %q", got)
	}

	if _, stderr, code := initRun(t, "", env{}.get, "", starterArgs(out, "--force")...); code != exitOK {
		t.Fatalf("--force: exit code %d, stderr %q", code, stderr)
	}
	if set := loadWritten(t, out); len(set.Bindings) != 1 {
		t.Errorf("--force wrote %d Bindings", len(set.Bindings))
	}
}

// A symbolic link is not followed, even with --force.
func TestInitDoesNotFollowASymbolicLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if _, _, code := initRun(t, "", env{}.get, "", starterArgs(link, "--force")...); code == exitOK {
		t.Error("init wrote through a symbolic link")
	}
	if got, _ := os.ReadFile(target); string(got) != "mine\n" {
		t.Errorf("the target is now %q", got)
	}
}

// A token is no option and no answer: it is refused without being shown, wherever it was typed.
func TestInitNeverEchoesAToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{"as an option that does not exist", []string{"--token", canaryToken}, ""},
		{"as a stray argument", []string{canaryToken}, ""},
		{"as the agent URL", starterArgs(outFile(t), "--agent-url", canaryToken), ""},
		{"as the agent name", starterArgs(outFile(t), "--agent-name", canaryToken), ""},
		{"as the channel", starterArgs(outFile(t), "--channel", "#"+canaryToken), ""},
		{"as the value of a flag that takes none", []string{"--force=" + canaryToken}, ""},
		{"as an answer", []string{"--out", outFile(t)}, "a2a\n" + canaryToken + "\nn\nn\nC0123456789\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := initRun(t, "", env{}.get, tc.stdin, tc.args...)
			if code != exitUsage {
				t.Errorf("exit code %d, stderr %q; want %d", code, stderr, exitUsage)
			}
			if strings.Contains(stdout+stderr, "CANARY") {
				t.Errorf("the token is in the output: %q", stdout+stderr)
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--token", canaryToken}, "dev", &stdout, &stderr, env{}.get); code != exitUsage || !strings.Contains(stderr.String(), "not defined") || strings.Contains(stderr.String(), "CANARY") {
		t.Errorf("run init --token = %d, %q; want %d, an error and no token", code, stderr.String(), exitUsage)
	}
}

// The command is reached as `agent-kourier init`, and --help prints the usage and exits 0.
func TestInitHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--help"}, "dev", &stdout, &stderr, env{}.get); code != exitOK || stdout.Len() != 0 {
		t.Errorf("--help = %d, stdout %q; want 0 and no stdout", code, stdout.String())
	}
	for _, flag := range []string{"--out", "--force", "--dialect", "--agent-url", "--agent-namespace", "--agent-name", "--channel", "--agent-token", "--namespace", "--token-file"} {
		if !strings.Contains(stderr.String(), flag) {
			t.Errorf("the usage lacks %s", flag)
		}
	}
}

// A channel given by name is looked up in Slack, and the bot is made to read it. The ID goes in the file.
func TestInitLooksTheChannelUpByName(t *testing.T) {
	slackSrv := &initSlack{channels: []map[string]string{{"id": "C0AAAAAAAAA", "name": "general"}, {"id": "C0BBBBBBBBB", "name": "sre-alerts"}}}
	url := slackSrv.serve(t)
	for _, name := range []string{"sre-alerts", "#sre-alerts"} {
		out := outFile(t)
		_, stderr, code := initRun(t, url, env{"SLACK_BOT_TOKEN": canaryToken}.get, "", starterArgs(out, "--channel", name)...)
		if code != exitOK {
			t.Fatalf("--channel %s: exit code %d, stderr %q", name, code, stderr)
		}
		if got := loadWritten(t, out).Bindings[0].Spec.Chat.Channel; got != "C0BBBBBBBBB" {
			t.Errorf("--channel %s: the file has channel %q; want C0BBBBBBBBB", name, got)
		}
		if strings.Contains(stderr, "CANARY") || strings.Contains(stderr, "Slack was not checked") {
			t.Errorf("stderr %q", stderr)
		}
	}
	if got := slackSrv.historyChannels; len(got) != 2 || got[0] != "C0BBBBBBBBB" || got[1] != "C0BBBBBBBBB" {
		t.Errorf("the bot read %v; want the channel that was found, twice", got)
	}
}

// A channel given by ID is not looked up, and the bot is made to read it from now on: no history is fetched, but the
// request is made, so that a missing scope or an uninvited bot shows.
func TestInitChecksThatTheBotCanReadTheChannel(t *testing.T) {
	slackSrv := &initSlack{}
	out := outFile(t)
	_, stderr, code := initRun(t, slackSrv.serve(t), env{"SLACK_BOT_TOKEN": canaryToken}.get, "", starterArgs(out)...)
	if code != exitOK || !strings.Contains(stderr, "the bot can read C0123456789") {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if slices := strings.Join(slackSrv.calls, " "); strings.Contains(slices, "conversations.list") || !strings.Contains(slices, "conversations.history") {
		t.Errorf("Slack was asked %q; want the history and no list", slices)
	}
	if len(slackSrv.historyO) != 1 || slackSrv.historyO[0] == "" {
		t.Errorf("the history was asked with oldest %q; want a time, so that no message is fetched", slackSrv.historyO)
	}
}

// The token file is used as dry-run uses it.
func TestInitReadsTheTokenFromAFile(t *testing.T) {
	slackSrv := &initSlack{}
	file := writeSecretFile(t, 0o600, "SLACK_BOT_TOKEN="+canaryToken+"\n")
	_, stderr, code := initRun(t, slackSrv.serve(t), env{}.get, "", starterArgs(outFile(t), "--token-file", file)...)
	if code != exitOK || !strings.Contains(stderr, "the bot can read") {
		t.Errorf("exit code %d, stderr %q", code, stderr)
	}
	// A token file that is wrong is an error, not a silent skip; so is a variable that is no token.
	for name, args := range map[string][]string{"a missing token file": {"--token-file", filepath.Join(t.TempDir(), "nope")}, "a file others can read": {"--token-file", writeSecretFile(t, 0o644, canaryToken)}} {
		if _, stderr, code := initRun(t, "", env{}.get, "", starterArgs(outFile(t), args...)...); code != exitUsage || strings.Contains(stderr, "CANARY") {
			t.Errorf("%s: exit code %d, stderr %q; want %d", name, code, stderr, exitUsage)
		}
	}
	if _, stderr, code := initRun(t, "", env{"SLACK_BOT_TOKEN": "not-a-token"}.get, "", starterArgs(outFile(t))...); code != exitUsage || !strings.Contains(stderr, "not a bot token") {
		t.Errorf("exit code %d, stderr %q; want %d and not a bot token", code, stderr, exitUsage)
	}
}

// What Slack says goes with what to do about it and the exit code that dry-run gives it; the file is not written, and the
// token is not shown even if Slack's error quotes it.
func TestInitReportsWhatSlackRefuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		slack    *initSlack
		channel  string
		want     string
		wantCode int
	}{
		{"the channel has no such name", &initSlack{channels: []map[string]string{{"id": "C0AAAAAAAAA", "name": "general"}}}, "nope", `no channel named "nope"`, exitUsage},
		{"the bot is not in the channel", &initSlack{historyErr: "not_in_channel"}, "C0123456789", "invite the bot", exitFailure},
		{"the channel is not found", &initSlack{historyErr: "channel_not_found"}, "C0123456789", "channel_not_found", exitFailure},
		{"the bot cannot read history", &initSlack{historyErr: "missing_scope"}, "C0123456789", "channels:history", exitFailure},
		{"the bot cannot list channels", &initSlack{listErr: "missing_scope"}, "alerts", "channels:read", exitFailure},
		{"the token is revoked", &initSlack{authErr: "token_revoked"}, "C0123456789", "token_revoked", exitSlackAuth},
		{"the token is rejected when it looks a channel up", &initSlack{listErr: "invalid_auth for " + canaryToken}, "alerts", "invalid_auth", exitSlackAuth},
		{"the token is rejected, and Slack quotes it", &initSlack{authErr: "invalid_auth for " + canaryToken}, "C0123456789", "invalid_auth", exitSlackAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := outFile(t)
			stdout, stderr, code := initRun(t, tc.slack.serve(t), env{"SLACK_BOT_TOKEN": canaryToken}.get, "", starterArgs(out, "--channel", tc.channel)...)
			if code != tc.wantCode || !strings.Contains(stderr, tc.want) || stdout != "" {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d and %q", code, stdout, stderr, tc.wantCode, tc.want)
			}
			if strings.Contains(stdout+stderr, "CANARY") {
				t.Errorf("the token is in the output: %q", stderr)
			}
			if _, err := os.Stat(out); err == nil {
				t.Error("a file was written")
			}
		})
	}
}

// A channel name needs a token to be looked up, and without one the error says how to go on.
func TestInitNeedsATokenToLookAChannelUp(t *testing.T) {
	out := outFile(t)
	stdout, stderr, code := initRun(t, "", env{}.get, "", starterArgs(out, "--channel", "alerts")...)
	if code != exitUsage || stdout != "" || !strings.Contains(stderr, botTokenKey) || !strings.Contains(stderr, "ID") {
		t.Errorf("exit code %d, stdout %q, stderr %q; want %d and the variable and the ID named", code, stdout, stderr, exitUsage)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a file was written")
	}
}

// Values that YAML would read as something other than the text, or as more than one value, are quoted and read back as
// they were written.
func TestYAMLScalarReadsBackAsTheString(t *testing.T) {
	for _, s := range []string{
		"plain", "http://agent.example.com:8083/a2a/", "C0123456789", "a2a", "kagent-v1",
		"true", "false", "null", "yes", "on", "~", "123", "1e3", "0x1f", "1.5", "", " lead", "trail ", "a: b", "a #b", "# c", "- x", "x:", "http://x:",
		"line\nbreak", "tab\there", `quote"d`, `back\slash`, "it's", "é", "{a: b}", "[a]", "&anchor", "*alias", "!tag", "%x", "@x", "`x",
		"http://agent.example.com:8083/a2a/?x=1&y=<2>#frag",
	} {
		var got map[string]any
		if err := yaml.Unmarshal([]byte("key: "+yamlScalar(s)+" # a comment\nother: 1\n"), &got); err != nil {
			t.Errorf("yamlScalar(%q) = %s, which does not parse: %v", s, yamlScalar(s), err)
			continue
		}
		if got["key"] != s || len(got) != 2 {
			t.Errorf("yamlScalar(%q) = %s, which reads back as %#v", s, yamlScalar(s), got)
		}
	}
}

// A URL with characters that YAML gives a meaning to is written as it is and arrives as it was given.
func TestInitKeepsAnAwkwardURLWhole(t *testing.T) {
	const awkward = "http://agent.example.com:8083/a2a/?x=1&y=2#frag"
	out := outFile(t)
	if _, stderr, code := initRun(t, "", env{}.get, "", starterArgs(out, "--agent-url", awkward)...); code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if got := loadWritten(t, out).AgentBackends[0].Spec.URL; got != awkward {
		t.Errorf("the URL is %q; want %q", got, awkward)
	}
}

// The commands that follow name the chart's version for a release and none for a build that is no release, and quote a
// path that the shell would split.
func TestNextSteps(t *testing.T) {
	for _, tc := range []struct {
		name, ver, out string
		want, not      []string
	}{
		{"a release with a v", "v0.1.1", "my-values.yaml", []string{" --version 0.1.1 \\\n  -n agent-kourier -f my-values.yaml"}, []string{"not a release"}},
		{"a release without a v", "0.2.0", "my-values.yaml", []string{" --version 0.2.0 "}, []string{"not a release"}},
		{"a prerelease", "v0.2.0-rc.1", "my-values.yaml", []string{" --version 0.2.0-rc.1 "}, nil},
		{"a development build", "dev", "my-values.yaml", []string{`# This build is "dev", not a release, so no chart version is named`, chartReference + " \\\n  -n agent-kourier"}, []string{"--version"}},
		{"a version that is none", "1.2", "my-values.yaml", []string{"not a release"}, []string{"--version"}},
		{"a path with a space", "v0.1.1", "my values.yaml", []string{"-f 'my values.yaml'"}, nil},
		{"a path with a quote", "v0.1.1", "it's.yaml", []string{`-f 'it'\''s.yaml'`}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextSteps(initOptions{out: tc.out, namespace: "agent-kourier"}, tc.ver)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("nextSteps lacks %q:\n%s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("nextSteps holds %q:\n%s", n, got)
				}
			}
		})
	}
	got := nextSteps(initOptions{out: "v.yaml", namespace: "team-a", agentToken: true}, "v0.1.1")
	for _, w := range []string{
		"kubectl create namespace team-a\n",
		"kubectl -n team-a create secret generic agent-kourier-slack \\\n  --from-literal=botToken=\"$SLACK_BOT_TOKEN\" \\\n  --from-literal=appToken=\"$SLACK_APP_TOKEN\"\n",
		"kubectl -n team-a create secret generic agent-kourier-agent-token --from-literal=token=\"$AGENT_TOKEN\"\n",
		"helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.1.1 \\\n  -n team-a -f v.yaml\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("nextSteps lacks %q:\n%s", w, got)
		}
	}
}

// The AgentBackend that init writes for kagent is the one the page about kagent shows.
func TestInitKagentBackendIsTheDocumentedOne(t *testing.T) {
	out := outFile(t)
	args := []string{"--out", out, "--dialect", "kagent-v1", "--agent-url", "http://kagent-frontdoor.kagent.svc.cluster.local:4180",
		"--agent-namespace", "payments", "--agent-name", "payments-agent", "--channel", "C0123456789"}
	if _, stderr, code := initRun(t, "", env{}.get, "", args...); code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	page, err := os.ReadFile("../../docs/how-to/connect-kagent.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "dialect: "+string(v1alpha1.DialectKagentV1)) || !strings.Contains(string(page), "url: http://kagent-frontdoor.kagent.svc.cluster.local:4180") {
		t.Fatal("the kagent page no longer shows the AgentBackend this test compares with")
	}
	if be := loadWritten(t, out).AgentBackends[0]; be.Spec.Dialect != v1alpha1.DialectKagentV1 {
		t.Errorf("dialect = %q", be.Spec.Dialect)
	}
}
