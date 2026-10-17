package kindmanifests_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// These tests pin the sandbox's alert source (hack/kind/sandbox/alertmanager, webhook.sh, alert.sh): the full tier's
// Alertmanager posting to the real alert channel through a copy of the production receiver, and the tool that fires
// the seeded faults' alerts. Nothing here touches a cluster or the real webhook file: every secret below is a fake constant.
const (
	alertDir = sandboxDir + "alertmanager/"

	// fakeWebhook is shaped as the webhook file demands, and its parts are found by name in any output that holds them.
	fakeWebhook = "https://hooks.slack.com/services/T0LEAKCHECK/B0LEAKCHECK/LEAKCHECKwebhook0007"
	leakMarker  = "LEAKCHECK"
)

// productionSample is a production Alertmanager template's receivers, Helm escapes and all. The comparison runs against it, and it is
// what a changed copy is shown to be caught on.
const productionSample = `            receivers:
              - name: "null"
              - name: slack-heartbeat
                slack_configs:
                  - api_url_file: /etc/alertmanager/secrets/alertmanager-slack-secret/webhook-url
                    channel: "#your-alert-channel"
                    send_resolved: false
                    title: 'Prometheus heartbeat'
                    text: 'Watchdog firing — Prometheus + Alertmanager are alive.'
              - name: slack-critical
                slack_configs:
                  - api_url_file: /etc/alertmanager/secrets/alertmanager-slack-secret/webhook-url
                    channel: "#your-alert-channel"
                    send_resolved: true
                    title: '{{ "{{" }} .Status | toUpper {{ "}}" }}{{ "{{" }} if eq .Status "firing" {{ "}}" }} ({{ "{{" }} .Alerts.Firing | len {{ "}}" }}){{ "{{" }} end {{ "}}" }} - {{ "{{" }} .CommonLabels.alertname {{ "}}" }}'
                    text: >-
                      {{ "{{" }} range .Alerts {{ "}}" }}
                      *{{ "{{" }} .Labels.alertname {{ "}}" }}* - {{ "{{" }} .Labels.severity {{ "}}" }}
                      {{ "{{" }} .Annotations.summary {{ "}}" }}
                      {{ "{{" }} .Annotations.description {{ "}}" }}
                      {{ "{{" }} end {{ "}}" }}
          route:
            main:
              enabled: true
`

// productionReceiver returns the receiver called name from the owner's Helm template src, as a map, with Helm's escapes of
// Alertmanager's own braces undone. It finds the receiver's list item by its name and takes the lines indented below it.
func productionReceiver(t *testing.T, src, name string) map[string]any {
	t.Helper()
	lines := strings.Split(src, "\n")
	indentOf := func(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "- name: "+name {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("the production template has no receiver %q", name)
	}
	indent := indentOf(lines[start])
	block := []string{lines[start][indent:]}
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) != "" && indentOf(l) <= indent {
			break
		}
		if len(l) >= indent {
			l = l[indent:]
		}
		block = append(block, l)
	}
	text := strings.NewReplacer(`{{ "{{" }}`, "{{", `{{ "}}" }}`, "}}").Replace(strings.Join(block, "\n"))
	var items []map[string]any
	if err := yaml.Unmarshal([]byte(text), &items); err != nil || len(items) != 1 {
		t.Fatalf("the production receiver %q: %d items, %v\n%s", name, len(items), err, text)
	}
	return items[0]
}

// kindReceiver returns the sandbox's receiver slack-critical-kind from its Alertmanager configuration.
func kindReceiver(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(alertDir + "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Receivers []map[string]any `json:"receivers"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, r := range cfg.Receivers {
		if r["name"] == "slack-critical-kind" {
			return r
		}
	}
	t.Fatal("the sandbox's Alertmanager configuration has no receiver slack-critical-kind")
	return nil
}

// expectedKindReceiver is what the production receiver becomes for the sandbox: renamed, and each title prefixed `[kind] `.
func expectedKindReceiver(t *testing.T, owner map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(owner) // a deep copy
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	want["name"] = "slack-critical-kind"
	configs, ok := want["slack_configs"].([]any)
	if !ok || len(configs) == 0 {
		t.Fatal("the production receiver has no slack_configs")
	}
	for _, c := range configs {
		m := c.(map[string]any)
		m["title"] = "[kind] " + m["title"].(string)
	}
	return want
}

// jsonForm round-trips v through JSON, so that two values from different decoders compare by what they hold.
func jsonForm(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestKindReceiverIsACopyOfTheOwnersExceptTheTitlePrefix: slack_configs of slack-critical-kind are those of the owner's
// slack-critical receiver, with the one change of the `[kind] ` prefix on the title. It runs against a sample of the production
// template, so that a copy edited here is caught.
func TestKindReceiverIsACopyOfTheOwnersExceptTheTitlePrefix(t *testing.T) {
	kind := jsonForm(t, kindReceiver(t))
	t.Run("the sample production template", func(t *testing.T) {
		want := jsonForm(t, expectedKindReceiver(t, productionReceiver(t, productionSample, "slack-critical")))
		if !reflect.DeepEqual(kind, want) {
			t.Errorf("slack-critical-kind is not the production slack-critical with the [kind] prefix:\n got %v\nwant %v", kind, want)
		}
	})
}

// TestKindReceiverComparisonCatchesADrift: the comparison above is not vacuous. Each change to the copy that it must refuse
// is refused, and the prefix is the only change it accepts.
func TestKindReceiverComparisonCatchesADrift(t *testing.T) {
	want := jsonForm(t, expectedKindReceiver(t, productionReceiver(t, productionSample, "slack-critical"))).(map[string]any)
	cfg := want["slack_configs"].([]any)[0].(map[string]any)
	if cfg["title"] != `[kind] {{ .Status | toUpper }}{{ if eq .Status "firing" }} ({{ .Alerts.Firing | len }}){{ end }} - {{ .CommonLabels.alertname }}` ||
		cfg["send_resolved"] != true || !strings.HasPrefix(cfg["text"].(string), "{{ range .Alerts }} *{{ .Labels.alertname }}*") {
		t.Fatalf("the production receiver was read as %v", cfg)
	}
	for name, mutate := range map[string]func(map[string]any){
		"no prefix":         func(c map[string]any) { c["title"] = strings.TrimPrefix(c["title"].(string), "[kind] ") },
		"send_resolved off": func(c map[string]any) { c["send_resolved"] = false },
		"another text":      func(c map[string]any) { c["text"] = "changed" },
		"another channel":   func(c map[string]any) { c["channel"] = "#elsewhere" },
		"a webhook inline":  func(c map[string]any) { c["api_url"] = "x" },
	} {
		got := jsonForm(t, want).(map[string]any)
		mutate(got["slack_configs"].([]any)[0].(map[string]any))
		if reflect.DeepEqual(got, want) {
			t.Errorf("%s: the comparison does not see it", name)
		}
	}
}

// alertConfig is the sandbox's Alertmanager configuration, as much of it as the tests read.
type alertConfig struct {
	Route struct {
		Receiver       string   `json:"receiver"`
		GroupBy        []string `json:"group_by"`
		RepeatInterval string   `json:"repeat_interval"`
		Routes         []struct {
			Receiver string   `json:"receiver"`
			Matchers []string `json:"matchers"`
			Continue bool     `json:"continue"`
		} `json:"routes"`
	} `json:"route"`
	Receivers []struct {
		Name         string           `json:"name"`
		SlackConfigs []map[string]any `json:"slack_configs"`
	} `json:"receivers"`
}

func loadAlertConfig(t *testing.T) alertConfig {
	t.Helper()
	raw, err := os.ReadFile(alertDir + "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg alertConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// routeMatches reports whether labels satisfy every one of the route's matchers, which are of the forms name="value" and
// name!="value" (an absent label is the empty string, as Alertmanager has it).
func routeMatches(t *testing.T, matchers []string, labels map[string]string) bool {
	t.Helper()
	re := regexp.MustCompile(`^([a-z_]+)(!?=)"([^"]*)"$`)
	for _, m := range matchers {
		parts := re.FindStringSubmatch(m)
		if parts == nil {
			t.Fatalf("matcher %q is not of the form name=\"value\" or name!=\"value\"", m)
		}
		if equal := labels[parts[1]] == parts[3]; equal != (parts[2] == "=") {
			return false
		}
	}
	return true
}

// TestAlertRouting: every synthetic alert (severity critical, which every sample alert has) reaches slack-critical-kind, through
// a route in the base configuration, which the operator adds no namespace matcher to; the route continues, so that the
// sink, whose route the operator puts first, and any route after it see the alert too; the group and the repeat interval are
// the owner's; and title_link is not overridden, so Alertmanager's own default, which carries `receiver=<name>`, stands.
func TestAlertRouting(t *testing.T) {
	cfg := loadAlertConfig(t)
	if cfg.Route.Receiver != "null" || !reflect.DeepEqual(cfg.Route.GroupBy, []string{"alertname", "namespace"}) || cfg.Route.RepeatInterval != "4h" {
		t.Errorf("the root route is %+v; want receiver null, group_by [alertname namespace], repeat_interval 4h as production's", cfg.Route)
	}
	if len(cfg.Route.Routes) != 1 {
		t.Fatalf("routes %+v; want the one for slack-critical-kind", cfg.Route.Routes)
	}
	r := cfg.Route.Routes[0]
	wantMatchers := []string{`severity="critical"`, `agentkourier_trial="true"`, `alertname!="Watchdog"`}
	if r.Receiver != "slack-critical-kind" || !reflect.DeepEqual(r.Matchers, wantMatchers) || !r.Continue {
		t.Errorf("the route is %+v; want slack-critical-kind for %v, with continue", r, wantMatchers)
	}
	// What the route lets through: an alert that alert.sh posts, and nothing else. The production channel is behind it.
	trial := map[string]string{"alertname": "KubePodCrashLooping", "severity": "critical", "namespace": "shop", "agentkourier_trial": "true"}
	without := func(m map[string]string, k string) map[string]string {
		out := map[string]string{}
		for key, v := range m {
			if key != k {
				out[key] = v
			}
		}
		return out
	}
	for name, tc := range map[string]struct {
		labels map[string]string
		want   bool
	}{
		"a trial alert":                      {trial, true},
		"a critical alert of anyone's":       {without(trial, "agentkourier_trial"), false},
		"the trial label set to false":       {map[string]string{"alertname": "X", "severity": "critical", "agentkourier_trial": "false"}, false},
		"a trial alert that is a Watchdog":   {map[string]string{"alertname": "Watchdog", "severity": "critical", "agentkourier_trial": "true"}, false},
		"a trial alert that is not critical": {map[string]string{"alertname": "X", "severity": "warning", "agentkourier_trial": "true"}, false},
	} {
		if got := routeMatches(t, r.Matchers, tc.labels); got != tc.want {
			t.Errorf("%s: the route matches: %t, want %t", name, got, tc.want)
		}
	}
	for _, rc := range cfg.Receivers {
		for _, sc := range rc.SlackConfigs {
			if _, ok := sc["title_link"]; ok {
				t.Errorf("receiver %s sets title_link: the default, which carries the receiver's name, must stand", rc.Name)
			}
		}
	}
	var overlay struct {
		Spec struct {
			ConfigSecret string   `json:"configSecret"`
			Secrets      []string `json:"secrets"`
			ExternalURL  string   `json:"externalUrl"`
		} `json:"spec"`
	}
	load(t, alertDir+"overlay.yaml", "Alertmanager", &overlay)
	if overlay.Spec.ExternalURL != "http://alertmanager.kind.local" {
		t.Errorf("externalUrl %q; want one that is plainly kind's, http://alertmanager.kind.local", overlay.Spec.ExternalURL)
	}
	if overlay.Spec.ConfigSecret != "alertmanager-kind-config" || !reflect.DeepEqual(overlay.Spec.Secrets, []string{"alertmanager-slack-secret"}) {
		t.Errorf("the overlay names configSecret %q and secrets %v", overlay.Spec.ConfigSecret, overlay.Spec.Secrets)
	}
}

// TestAlertSourceHoldsNoWebhookURL: no file of the alert source, and no other manifest of the sandbox, holds a webhook URL or
// declares a Secret; the receiver reads the URL by api_url_file from the path where the overlay's `secrets` mount it; and up.sh
// makes both Secrets through apply_secret (stdin, server-side apply, never argv), under the names the overlay and the file say.
func TestAlertSourceHoldsNoWebhookURL(t *testing.T) {
	files, err := filepath.Glob(alertDir + "*")
	if err != nil || len(files) < 3 {
		t.Fatalf("files %v, %v", files, err)
	}
	more, _ := filepath.Glob(sandboxDir + "*.yaml")
	recipe, _ := filepath.Glob(agentDir + "*.yaml")
	for _, file := range slices.Concat(files, more, recipe) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "hooks.slack.com/services") && !strings.HasSuffix(file, "webhook.sh") {
			t.Errorf("%s holds an incoming-webhook URL", file)
		}
		if strings.HasSuffix(file, ".yaml") && !strings.HasSuffix(file, "slack-app-manifest.yaml") {
			for _, doc := range objects(t, file) {
				if doc["kind"] == "Secret" {
					t.Errorf("%s declares a Secret: they are made by up.sh through apply_secret", file)
				}
			}
		}
	}
	cfg := loadAlertConfig(t)
	var found int
	for _, rc := range cfg.Receivers {
		for _, sc := range rc.SlackConfigs {
			found++
			if _, inline := sc["api_url"]; inline || sc["api_url_file"] != "/etc/alertmanager/secrets/alertmanager-slack-secret/webhook-url" {
				t.Errorf("receiver %s: api_url_file %v, api_url %v; want the mounted Secret's key and no inline URL", rc.Name, sc["api_url_file"], sc["api_url"])
			}
		}
	}
	if found == 0 {
		t.Error("no slack_configs")
	}
	up, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apply_secret monitoring alertmanager-slack-secret webhook-url=ALERTMANAGER_WEBHOOK_URL\n",
		"apply_secret monitoring alertmanager-kind-config alertmanager.yaml=ALERTMANAGER_CONFIG\n",
		`webhook_url_load "${ALERTMANAGER_WEBHOOK_FILE}"`,
		`apply --server-side --force-conflicts --field-manager=agent-kourier-sandbox -f "${SBX}/alertmanager/overlay.yaml"`,
	} {
		if !strings.Contains(string(up), want) {
			t.Errorf("up.sh lacks %q", want)
		}
	}
}

// TestWebhookVariableIsOnlyEverNamedForApplySecret: the variable that holds the URL is never expanded in up.sh and down.sh, only
// named in apply_secret's argument; in webhook.sh it is only assigned and unset.
func TestWebhookVariableIsOnlyEverNamedForApplySecret(t *testing.T) {
	for _, name := range []string{"up.sh", "down.sh", "alert.sh", "webhook.sh"} {
		raw, err := os.ReadFile(sandboxDir + name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		if regexp.MustCompile(`\$\{?!?ALERTMANAGER_WEBHOOK_URL\b`).MatchString(src) {
			t.Errorf("%s expands the URL's variable", name)
		}
		for i, line := range strings.Split(src, "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "#") || !strings.Contains(trim, "ALERTMANAGER_WEBHOOK_URL") {
				continue
			}
			switch {
			case strings.HasPrefix(trim, "apply_secret ") && strings.HasSuffix(trim, "=ALERTMANAGER_WEBHOOK_URL"):
			case name == "webhook.sh" && (strings.HasPrefix(trim, "unset ALERTMANAGER_WEBHOOK_URL") || trim == "ALERTMANAGER_WEBHOOK_URL=${value}"):
			default:
				t.Errorf("%s:%d: %q names the URL's variable outside apply_secret's argument", name, i+1, trim)
			}
		}
	}
}

// webhookCheck runs webhook_url_load on path, with env.sh sourced as up.sh has it, and returns the exit status and outputs.
// The script goes on to show what the load left in the shell: the URL is in its variable, which a child does not inherit.
func webhookCheck(t *testing.T, path string, env ...string) (code int, stdout, stderr string) {
	t.Helper()
	script := `set -eu
. "$1"
. "$2"
webhook_url_load "$3"
[ "${ALERTMANAGER_WEBHOOK_URL}" = "$4" ] || { echo "variable not set" >&2; exit 9; }
if env | grep -q -e LEAKCHECK -e FROMENV; then echo "exported" >&2; exit 8; fi
echo loaded`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", sandboxDir+"env.sh", sandboxDir+"webhook.sh", path, fakeWebhook)
	cmd.Env = append(os.Environ(), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

// webhookFile writes content to a new file of the given mode in a fresh temp directory and returns its path.
func webhookFile(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alertmanager-webhook-url")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWebhookFileAcceptsAGoodFile(t *testing.T) {
	for name, content := range map[string]string{
		"a line":                     fakeWebhook + "\n",
		"no final newline":           fakeWebhook,
		"windows line ending":        fakeWebhook + "\r\n",
		"trailing blanks":            fakeWebhook + "  \n",
		"blank lines around it":      "\n" + fakeWebhook + "\n\n",
		"the production usual shape": "https://hooks.slack.com/services/T0LEAKCHECK/B0LEAKCHECK/abcDEF123\n",
	} {
		t.Run(name, func(t *testing.T) {
			want := strings.TrimSpace(content)
			path := webhookFile(t, 0o600, content)
			script := `set -eu
. "$1"
. "$2"
webhook_url_load "$3"
[ "${ALERTMANAGER_WEBHOOK_URL}" = "$4" ]`
			out, err := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", sandboxDir+"env.sh", sandboxDir+"webhook.sh", path, want).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if leak := strings.Contains(string(out), leakMarker); leak {
				t.Errorf("the load printed the URL: %q", out)
			}
		})
	}
	// The URL is in the shell's variable and in no child's environment, and one that was in the environment before is not
	// taken for the file's (and is gone from the children's, too).
	for _, env := range [][]string{nil, {"ALERTMANAGER_WEBHOOK_URL=https://hooks.slack.com/services/TFROMENV/BFROMENV/fromenv"}} {
		if code, stdout, stderr := webhookCheck(t, webhookFile(t, 0o600, fakeWebhook+"\n"), env...); code != 0 || stdout != "loaded\n" {
			t.Errorf("env %v: exit %d, stdout %q, stderr %q; want it loaded, the file's URL in its variable and in no child's environment", env, code, stdout, stderr)
		}
	}
}

// TestWebhookFileRefuses: each way the file can be wrong is refused with a non-zero status and a message that names the file
// and never holds any of the file's content.
func TestWebhookFileRefuses(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(webhookFile(t, 0o600, fakeWebhook+"\n"), link); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		want string
	}{
		"missing":                 {filepath.Join(dir, "nowhere"), "does not exist"},
		"a symbolic link":         {link, "is a symbolic link"},
		"a directory":             {dir, "is not a regular file"},
		"readable by others":      {webhookFile(t, 0o644, fakeWebhook+"\n"), "has mode 644"},
		"readable by the group":   {webhookFile(t, 0o640, fakeWebhook+"\n"), "has mode 640"},
		"writable by the group":   {webhookFile(t, 0o620, fakeWebhook+"\n"), "has mode 620"},
		"empty":                   {webhookFile(t, 0o600, ""), "is empty"},
		"blank":                   {webhookFile(t, 0o600, "\n  \n"), "is empty"},
		"two lines":               {webhookFile(t, 0o600, fakeWebhook+"\n"+fakeWebhook+"\n"), "more than one line"},
		"a comment first":         {webhookFile(t, 0o600, "# the webhook\n"+fakeWebhook+"\n"), "more than one line"},
		"http, not https":         {webhookFile(t, 0o600, strings.Replace(fakeWebhook, "https", "http", 1)+"\n"), "does not hold a Slack incoming-webhook URL"},
		"another host":            {webhookFile(t, 0o600, strings.Replace(fakeWebhook, "hooks.slack.com", "hooks.example.com", 1)+"\n"), "does not hold a Slack incoming-webhook URL"},
		"a host that ends in it":  {webhookFile(t, 0o600, strings.Replace(fakeWebhook, "hooks.slack.com", "hooks.slack.com.evil.test", 1)+"\n"), "does not hold a Slack incoming-webhook URL"},
		"a user in front of host": {webhookFile(t, 0o600, strings.Replace(fakeWebhook, "hooks.slack.com", "evil.test@hooks.slack.com", 1)+"\n"), "does not hold a Slack incoming-webhook URL"},
		"a bot token":             {webhookFile(t, 0o600, "xoxb-LEAKCHECK-0001-0002\n"), "does not hold a Slack incoming-webhook URL"},
		"a missing secret part":   {webhookFile(t, 0o600, "https://hooks.slack.com/services/T0LEAKCHECK/B0LEAKCHECK/\n"), "does not hold a Slack incoming-webhook URL"},
		"lower-case ids":          {webhookFile(t, 0o600, "https://hooks.slack.com/services/t0leakcheck/b0leakcheck/LEAKCHECKwebhook0007\n"), "does not hold a Slack incoming-webhook URL"},
		"a query":                 {webhookFile(t, 0o600, fakeWebhook+"?x=1\n"), "does not hold a Slack incoming-webhook URL"},
		"a path after":            {webhookFile(t, 0o600, fakeWebhook+"/more\n"), "does not hold a Slack incoming-webhook URL"},
		"a space inside":          {webhookFile(t, 0o600, strings.Replace(fakeWebhook, "B0", "B 0", 1)+"\n"), "does not hold a Slack incoming-webhook URL"},
		"quotes round it":         {webhookFile(t, 0o600, `"`+fakeWebhook+`"`+"\n"), "does not hold a Slack incoming-webhook URL"},
		"a KEY= in front":         {webhookFile(t, 0o600, "WEBHOOK="+fakeWebhook+"\n"), "does not hold a Slack incoming-webhook URL"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := webhookCheck(t, tc.path)
			if code != 1 {
				t.Fatalf("exit %d, stdout %q, stderr %q; want it refused with 1", code, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not say %q", stderr, tc.want)
			}
			if !strings.Contains(stderr, "error: "+tc.path+":") {
				t.Errorf("stderr %q does not name the file %s", stderr, tc.path)
			}
			for what, out := range map[string]string{"stdout": stdout, "stderr": stderr} {
				if strings.Contains(strings.ToLower(out), strings.ToLower(leakMarker)) {
					t.Errorf("%s holds part of the file: %q", what, out)
				}
			}
		})
	}
}

// upWithWebhook runs up.sh with a good secrets file, the given webhook file, and stand-ins for the tools, whose docker says the
// daemon is not reachable (up.sh's first step after its file checks), so that the run stops there. It returns the output, whether
// the run succeeded, and the tools that ran.
func upWithWebhook(t *testing.T, webhook string) (out string, ok bool, ran string) {
	t.Helper()
	return upWithEnv(t, webhook, goodEnv()...)
}

// upWithEnv is upWithWebhook with the given lines in the secrets file.
func upWithEnv(t *testing.T, webhook string, env ...string) (out string, ok bool, ran string) {
	t.Helper()
	bin := t.TempDir()
	mark := filepath.Join(bin, "ran")
	for _, tool := range []string{"docker", "kind", "kubectl", "helm", "jq"} {
		stub := "#!/bin/sh\necho " + tool + " >> " + mark + "\n"
		if tool == "docker" {
			stub += "exit 1\n"
		}
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "bash", sandboxDir+"up.sh")
	cmd.Env = append(os.Environ(), "SANDBOX_ENV="+envFile(t, 0o600, env...), "ALERTMANAGER_WEBHOOK_FILE="+webhook,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	raw, err := cmd.CombinedOutput()
	marks, _ := os.ReadFile(mark)
	return string(raw), err == nil, string(marks)
}

// TestUpRefusesAWebhookFileThatIsThereAndBad: the alert source is optional, but a webhook file that exists is held to every check,
// and one that fails stops up.sh before it asks Docker, kind, kubectl or helm anything. The message names the file and no content.
func TestUpRefusesAWebhookFileThatIsThereAndBad(t *testing.T) {
	good := webhookFile(t, 0o600, fakeWebhook+"\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(t.TempDir(), "dangling")
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	for name, webhook := range map[string]string{
		"world-readable":      webhookFile(t, 0o644, fakeWebhook+"\n"),
		"group-readable":      webhookFile(t, 0o640, fakeWebhook+"\n"),
		"not a webhook's URL": webhookFile(t, 0o600, "LEAKCHECK-not-a-url\n"),
		"empty":               webhookFile(t, 0o600, ""),
		"a symbolic link":     link,
		"a dangling link":     dangling,
	} {
		t.Run(name, func(t *testing.T) {
			out, ok, ran := upWithWebhook(t, webhook)
			if ok {
				t.Fatalf("up.sh accepted the webhook file:\n%s", out)
			}
			if !strings.Contains(out, "error: "+webhook+":") {
				t.Errorf("up.sh said %q; want the webhook file named", out)
			}
			noLeak(t, "up.sh output", out)
			if strings.Contains(out, leakMarker) || strings.Contains(out, "not configured") {
				t.Errorf("up.sh printed part of the webhook file, or called a file that is there not configured: %q", out)
			}
			if ran != "" {
				t.Errorf("up.sh ran %q before the webhook file was accepted", ran)
			}
		})
	}
}

// TestUpSkipsTheAlertSourceWithoutTheWebhookFile: with no webhook file, up.sh says so in one line, does not stop, and goes on to its
// next step (here Docker, which refuses, which is how the test stops it).
func TestUpSkipsTheAlertSourceWithoutTheWebhookFile(t *testing.T) {
	webhook := filepath.Join(t.TempDir(), "nowhere")
	out, ok, ran := upWithWebhook(t, webhook)
	if ok {
		t.Fatalf("up.sh succeeded with a docker that refuses:\n%s", out)
	}
	if n := strings.Count(out, "alert source: not configured; see README Alert trial\n"); n != 1 {
		t.Errorf("up.sh said the alert source is not configured %d times: %q", n, out)
	}
	if strings.Contains(out, "error: "+webhook) || !strings.Contains(out, "the Docker daemon is not reachable") || !strings.HasPrefix(ran, "docker") {
		t.Errorf("up.sh did not go on past the webhook file (ran %q): %q", ran, out)
	}
}

// alertSourceBlock is the step of up.sh that makes the alert source, which runs only when ALERT_SOURCE is 1.
func alertSourceBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	start := strings.Index(s, "if [ \"${ALERT_SOURCE}\" = 1 ]; then\n  step \"alert source:")
	if start < 0 {
		t.Fatal("up.sh has no guarded alert-source step")
	}
	end := strings.Index(s[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("the alert-source step has no fi")
	}
	return s[start : start+end+len("\nfi\n")]
}

// base64Of is the base64 of v, as apply_secret writes it.
func base64Of(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) }

// TestUpAlertSourceStepOnlyRunsWhenConfigured: the step, run as up.sh has it with stand-ins for kubectl, base64 and tr, makes nothing
// when the alert source is not configured, and when it is makes the two Secrets through apply_secret (the webhook as base64 on stdin,
// never on an argument list or in the environment) and applies the Alertmanager overlay, all by server-side apply. The verification
// is guarded the same way.
func TestUpAlertSourceStepOnlyRunsWhenConfigured(t *testing.T) {
	block := alertSourceBlock(t)
	overlay := strings.TrimSuffix(sandboxDir, "/") + "/alertmanager/overlay.yaml"
	for name, tc := range map[string]struct {
		alertSource string
		configured  bool
	}{"not configured": {"0", false}, "configured": {"1", true}} {
		t.Run(name, func(t *testing.T) {
			bin, logs := recorders(t)
			script := `set -eu
K=(kubectl --context kind-agent-kourier-full)
SBX=$1
step() { :; }
. "$2"
ALERT_SOURCE=$3
ALERTMANAGER_WEBHOOK_URL=$4
` + block
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", strings.TrimSuffix(sandboxDir, "/"), sandboxDir+"env.sh", tc.alertSource, fakeWebhook)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			var out, errOut bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errOut
			if err := cmd.Run(); err != nil {
				t.Fatalf("%v\nstdout %q\nstderr %q", err, out.String(), errOut.String())
			}
			argv := strings.TrimSpace(readLog(t, logs, "kubectl.argv"))
			if !tc.configured {
				if argv != "" || readLog(t, logs, "kubectl.stdin") != "" {
					t.Errorf("kubectl was run (%q) with no alert source configured", argv)
				}
				return
			}
			lines := strings.Split(argv, "\n")
			onStdin := "--context kind-agent-kourier-full apply --server-side --force-conflicts --field-manager=agent-kourier-sandbox -f -"
			if len(lines) != 3 || lines[0] != onStdin || lines[1] != onStdin ||
				lines[2] != "--context kind-agent-kourier-full apply --server-side --force-conflicts --field-manager=agent-kourier-sandbox -f "+overlay {
				t.Errorf("kubectl was run as %q; want two applies on stdin and then the overlay's", lines)
			}
			stdin := readLog(t, logs, "kubectl.stdin")
			for _, want := range []string{"  name: alertmanager-slack-secret\n  namespace: monitoring\n", "  webhook-url: " + base64Of(fakeWebhook) + "\n", "  name: alertmanager-kind-config\n", "  alertmanager.yaml: "} {
				if !strings.Contains(stdin, want) {
					t.Errorf("kubectl's stdin lacks %q:\n%s", want, redactBase64(stdin))
				}
			}
			for _, log := range []string{"kubectl.argv", "base64.argv", "tr.argv", "kubectl.env"} {
				if v := readLog(t, logs, log); strings.Contains(v, leakMarker) || strings.Contains(v, base64Of(fakeWebhook)) {
					t.Errorf("%s holds the webhook URL", log)
				}
			}
			if strings.Contains(stdin, leakMarker) || strings.Contains(out.String()+errOut.String(), leakMarker) {
				t.Error("the webhook URL is raw on stdin or in the output")
			}
		})
	}
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	verify := strings.Index(s, "check \"Alertmanager is the alert source:")
	if verify < 0 {
		t.Fatal("up.sh does not check the alert source")
	}
	guard := strings.LastIndex(s[:verify], "if [ \"${ALERT_SOURCE}\" = 1 ]; then")
	if guard < 0 || strings.Contains(s[guard:verify], "\nfi\n") {
		t.Error("up.sh's check of the alert source is not inside an `if ALERT_SOURCE = 1`")
	}
	if n := strings.Count(s, "ALERT_SOURCE=0") + strings.Count(s, "ALERT_SOURCE=1"); n != 2 {
		t.Errorf("up.sh sets ALERT_SOURCE %d times; want twice, 1 with the file and 0 without it", n)
	}
}

// sampleAlert is one entry of alertmanager/alerts.json.
type sampleAlert struct {
	App         string            `json:"app"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

func loadSamples(t *testing.T) map[string]sampleAlert {
	t.Helper()
	raw, err := os.ReadFile(alertDir + "alerts.json")
	if err != nil {
		t.Fatal(err)
	}
	var samples map[string]sampleAlert
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatal(err)
	}
	return samples
}

// TestSampleAlerts: there is a sample alert for each seeded fault that has a pod or a Service of its own to name, and every one
// carries the labels and annotations a kube-prometheus rule would, and the ones the route and the template read.
func TestSampleAlerts(t *testing.T) {
	samples := loadSamples(t)
	// The faults of faults.yaml: each sample alert names the app (or Service) its fault is seeded in.
	want := map[string]string{
		"KubePodCrashLooping":    "checkout",
		"KubePodNotReady":        "search",
		"KubeContainerWaiting":   "catalog",
		"KubeContainerOOMKilled": "recommender",
		"TargetDown":             "inventory",
	}
	if len(samples) != len(want) {
		t.Errorf("%d sample alerts; want %d", len(samples), len(want))
	}
	faults, err := os.ReadFile(agentDir + "faults.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, app := range want {
		p, ok := samples[name]
		if !ok {
			t.Errorf("no sample alert %s", name)
			continue
		}
		if p.Labels["alertname"] != name || p.Labels["severity"] != "critical" || p.Labels["namespace"] != "shop" {
			t.Errorf("%s: labels %v; want its alertname, severity=critical and namespace=shop (the route matches on the severity)", name, p.Labels)
		}
		if _, ok := p.Labels["agentkourier_trial"]; ok {
			t.Errorf("%s sets agentkourier_trial: alert.sh adds it, so that no sample alert can leave it out or set it otherwise", name)
		}
		if p.Annotations["summary"] == "" || p.Annotations["description"] == "" {
			t.Errorf("%s: annotations %v; want a summary and a description, which the receiver's text prints", name, p.Annotations)
		}
		// What the alert is about: a pod (named at fire time from the app's live pod) or, for a target, the job and Service.
		switch {
		case p.Labels["pod"] != "":
			if p.Labels["pod"] != "__POD__" || p.App != app || !strings.Contains(p.Annotations["description"], "__POD__") {
				t.Errorf("%s: pod %q, app %q; want the placeholder __POD__ and app %s", name, p.Labels["pod"], p.App, app)
			}
		case p.Labels["service"] != "" && p.Labels["job"] != "":
			if p.Labels["service"] != app || p.App != "" {
				t.Errorf("%s: service %q, app %q; want the Service %s and no app", name, p.Labels["service"], p.App, app)
			}
		default:
			t.Errorf("%s: labels %v name neither a pod nor a job and Service", name, p.Labels)
		}
		if !strings.Contains(string(faults), "metadata: {name: "+app+", namespace: shop") {
			t.Errorf("%s: %s is not a Deployment of faults.yaml", name, app)
		}
		for k, v := range p.Labels {
			if v == "" || strings.Contains(strings.ReplaceAll(v, "__POD__", ""), "__") {
				t.Errorf("%s: label %s is %q", name, k, v)
			}
		}
		if len(p.Labels["alertname"]) == 0 || regexp.MustCompile(`[^A-Za-z0-9]`).MatchString(p.Labels["alertname"]) {
			t.Errorf("%s: an alertname that is not a plain name", name)
		}
	}
}

// alertStubs makes a PATH of stand-ins for kubectl and kind and returns it with the directory of their records. The kubectl
// answers each question alert.sh asks the way a full tier with the sandbox up does, records every command line, and keeps what
// the probe pod is given on its stdin.
func alertStubs(t *testing.T, configSecret string) (bin, logs string) {
	t.Helper()
	bin, logs = t.TempDir(), t.TempDir()
	// The pod lists are PodList JSON as `kubectl get pods -o json` gives it, a terminating pod (it has a deletionTimestamp) first
	// and the live one after it. ALERT_STUB_PODS=none gives an empty list and =terminating a list of only the terminating pod.
	// ALERT_STUB_RUNFAILS=N makes the first N probe pods fail with no output, as a pod that never ran does.
	kubectl := `#!/bin/sh
printf '%s\n' "$*" | tr '\n' ' ' >> ` + logs + `/kubectl.argv
echo >> ` + logs + `/kubectl.argv
pods() { # APP LIVE-POD
  terminating='{"metadata":{"name":"'"$1"'-terminating-zzzzz","deletionTimestamp":"2026-10-04T00:00:00Z"}}'
  case "$ALERT_STUB_PODS" in
    none) echo '{"apiVersion":"v1","kind":"List","items":[]}' ;;
    terminating) echo '{"apiVersion":"v1","kind":"List","items":['"$terminating"']}' ;;
    *) echo '{"apiVersion":"v1","kind":"List","items":['"$terminating"',{"metadata":{"name":"'"$2"'","labels":{"app":"'"$1"'"}}}]}' ;;
  esac
}
case "$*" in
  *"contexts[0].name"*) echo kind-agent-kourier-full ;;
  *"clusters[0].cluster.server"*) echo https://127.0.0.1:6443 ;;
  *"get alertmanager agent-kourier"*) printf %s '` + configSecret + `' ;;
  *"get pods -l app=checkout -o json"*) pods checkout checkout-7d9f8c6b5-abcde ;;
  *"get pods -l app=search -o json"*) pods search search-5f6b9d-xyz12 ;;
  *"get pods -l app=catalog -o json"*) pods catalog catalog-6c7d8e-qwert ;;
  *"get pods -l app=recommender -o json"*) pods recommender recommender-8b9c-mnbvc ;;
  *" run "*)
    n=$(cat ` + logs + `/runs 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > ` + logs + `/runs
    cat >> ` + logs + `/kubectl.stdin
    [ "$n" -gt "${ALERT_STUB_RUNFAILS:-0}" ] || exit 1
    printf 200 ;;
  *" delete pod "*) ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(kubectl), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "kind"), []byte("#!/bin/sh\necho 'apiVersion: v1'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, logs
}

// fireAlert runs alert.sh with the stand-ins and returns its exit status and outputs.
func fireAlert(t *testing.T, bin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{sandboxDir + "alert.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

// postedAlert is what the probe pod was given for Alertmanager.
type postedAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
}

// TestAlertShFiresEachSample: each sample alert is posted to Alertmanager's v2 API from a probe pod in the cluster, as one alert
// with its labels, the live pod's name in place of the placeholder, a start that is not in the future and an end an hour on,
// and the script prints the alertname and the state, and nothing else.
func TestAlertShFiresEachSample(t *testing.T) {
	pods := map[string]string{"KubePodCrashLooping": "checkout-7d9f8c6b5-abcde", "KubePodNotReady": "search-5f6b9d-xyz12",
		"KubeContainerWaiting": "catalog-6c7d8e-qwert", "KubeContainerOOMKilled": "recommender-8b9c-mnbvc", "TargetDown": ""}
	samples := loadSamples(t)
	for name, pod := range pods {
		for _, args := range [][]string{{name}, {name, "firing"}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				bin, logs := alertStubs(t, "alertmanager-kind-config")
				before := time.Now().Add(-2 * time.Second)
				code, stdout, stderr := fireAlert(t, bin, args...)
				if code != 0 || stdout != "sent: "+name+" firing\n" || stderr != "" {
					t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and the alertname and the state only", code, stdout, stderr)
				}
				var posted []postedAlert
				if err := json.Unmarshal([]byte(readLog(t, logs, "kubectl.stdin")), &posted); err != nil || len(posted) != 1 {
					t.Fatalf("the probe pod was given %q: %d alerts, %v; want one", readLog(t, logs, "kubectl.stdin"), len(posted), err)
				}
				a := posted[0]
				// The sample alert's labels with the live pod's name in, and the one label alert.sh adds, which the route requires.
				wantLabels := map[string]string{"agentkourier_trial": "true"}
				for k, v := range samples[name].Labels {
					wantLabels[k] = strings.ReplaceAll(v, "__POD__", pod)
				}
				if !reflect.DeepEqual(a.Labels, wantLabels) {
					t.Errorf("labels %v; want %v", a.Labels, wantLabels)
				}
				if strings.Contains(a.Labels["pod"], "terminating") || strings.Contains(a.Annotations["description"], "terminating") {
					t.Errorf("the alert names a terminating pod: %v", a.Labels)
				}
				if !routeMatches(t, loadAlertConfig(t).Route.Routes[0].Matchers, a.Labels) {
					t.Errorf("the route of slack-critical-kind does not take %v: nothing would be posted", a.Labels)
				}
				if a.Annotations["summary"] == "" || strings.Contains(a.Annotations["description"], "__POD__") || (pod != "" && !strings.Contains(a.Annotations["description"], pod)) {
					t.Errorf("annotations %v", a.Annotations)
				}
				if a.StartsAt.Before(before) || a.StartsAt.After(time.Now().Add(2*time.Second)) {
					t.Errorf("startsAt %v is not now", a.StartsAt)
				}
				if d := a.EndsAt.Sub(a.StartsAt); d < 59*time.Minute || d > 61*time.Minute {
					t.Errorf("endsAt %v is %v after startsAt; want an hour, so that a fired alert resolves itself", a.EndsAt, d)
				}
			})
		}
	}
}

// TestAlertShResolves: the same alert, with an end in the past, so that Alertmanager resolves it.
func TestAlertShResolves(t *testing.T) {
	bin, logs := alertStubs(t, "alertmanager-kind-config")
	code, stdout, stderr := fireAlert(t, bin, "KubePodCrashLooping", "resolved")
	if code != 0 || stdout != "sent: KubePodCrashLooping resolved\n" || stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	var posted []postedAlert
	if err := json.Unmarshal([]byte(readLog(t, logs, "kubectl.stdin")), &posted); err != nil || len(posted) != 1 {
		t.Fatalf("the probe pod was given %q: %v", readLog(t, logs, "kubectl.stdin"), err)
	}
	a := posted[0]
	if !a.EndsAt.Before(time.Now()) || !a.StartsAt.Before(a.EndsAt) {
		t.Errorf("startsAt %v, endsAt %v; want an end in the past and after the start", a.StartsAt, a.EndsAt)
	}
	if a.Labels["pod"] != "checkout-7d9f8c6b5-abcde" || a.Labels["alertname"] != "KubePodCrashLooping" || a.Labels["agentkourier_trial"] != "true" {
		t.Errorf("labels %v: a resolve must carry the labels of the firing alert", a.Labels)
	}
}

// TestAlertShNamesTheContextAndTheProbe: every kubectl call names --context kind-agent-kourier-full; the probe pod runs the pinned
// curl image, in namespace monitoring, and reaches Alertmanager's v2 API by its Service name; the body is on the pod's
// stdin and on no argument list.
func TestAlertShNamesTheContextAndTheProbe(t *testing.T) {
	bin, logs := alertStubs(t, "alertmanager-kind-config")
	if code, _, stderr := fireAlert(t, bin, "TargetDown"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	argv := readLog(t, logs, "kubectl.argv")
	// The probe runs lib.sh's CURL_IMAGE, pinned by tag and digest; Renovate moves that pin, so it is read, not copied.
	lib, err := os.ReadFile("../../hack/kind/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^CURL_IMAGE=(curlimages/curl:[^@\s]+@sha256:[0-9a-f]{64})$`).FindSubmatch(lib)
	if pin == nil {
		t.Fatal("lib.sh does not pin CURL_IMAGE to curlimages/curl by tag and digest")
	}
	var runs int
	for _, line := range strings.Split(strings.TrimSpace(argv), "\n") {
		if strings.HasPrefix(line, "config view --kubeconfig ") {
			continue // the check of kind's own server, which names no context by design (lib.sh's assert_context)
		}
		if !strings.Contains(line, "--context kind-agent-kourier-full") {
			t.Errorf("kubectl was run without the context: %q", line)
		}
		if strings.Contains(line, " run ") {
			runs++
			for _, want := range []string{"-n monitoring run ", "--rm", "-i", "--restart=Never", "--image=" + string(pin[1]) + " ", "http://alertmanager.monitoring.svc.cluster.local:9093/api/v2/alerts", "Content-Type: application/json", "@-"} {
				if !strings.Contains(line, want) {
					t.Errorf("the probe pod's command %q lacks %q", line, want)
				}
			}
			if strings.Contains(line, "TargetDown") || strings.Contains(line, "labels") {
				t.Errorf("the alert is on the argument list: %q", line)
			}
		}
	}
	if runs != 1 {
		t.Errorf("%d probe pods; want one", runs)
	}
}

// TestAlertShRefuses: what is not a sample alert, not a state, or not the sandbox's Alertmanager posts nothing and says why.
func TestAlertShRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		args         []string
		configSecret string
		want         string
		askedCluster bool // whether kubectl may have been run at all
	}{
		"no alert":              {nil, "alertmanager-kind-config", "usage: make sandbox-alert ALERT=", false},
		"an empty alert":        {[]string{""}, "alertmanager-kind-config", "usage: make sandbox-alert ALERT=", false},
		"an unknown alert":      {[]string{"NodeFilesystemAlmostOutOfSpace"}, "alertmanager-kind-config", "KubePodCrashLooping", false},
		"a near miss":           {[]string{"kubepodcrashlooping"}, "alertmanager-kind-config", "not a sample alert", false},
		"a JSON key's cousin":   {[]string{`KubePodCrashLooping" or true or "`}, "alertmanager-kind-config", "not a sample alert", false},
		"an unknown state":      {[]string{"TargetDown", "pending"}, "alertmanager-kind-config", "STATE must be firing or resolved", false},
		"not the sandbox's one": {[]string{"TargetDown"}, "", "the alert source is not configured", true},
		"the tier's own one":    {[]string{"TargetDown"}, "alertmanager-agent-kourier", "the alert source is not configured", true},
	} {
		t.Run(name, func(t *testing.T) {
			bin, logs := alertStubs(t, tc.configSecret)
			code, stdout, stderr := fireAlert(t, bin, tc.args...)
			if code != 1 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stdout %q, stderr %q; want 1, nothing on stdout and %q on stderr", code, stdout, stderr, tc.want)
			}
			if readLog(t, logs, "kubectl.stdin") != "" || strings.Contains(readLog(t, logs, "kubectl.argv"), " run ") {
				t.Error("an alert was posted")
			}
			if !tc.askedCluster && readLog(t, logs, "kubectl.argv") != "" {
				t.Errorf("kubectl was run (%q) before the arguments were accepted", readLog(t, logs, "kubectl.argv"))
			}
		})
	}
}

// TestAlertShFindsOnlyALivePod: a pod that is terminating is not the pod the alert is about, and with none that is live the script says
// so and posts nothing. (The live pod being the one posted, with a terminating one listed first, is TestAlertShFiresEachSample's.)
func TestAlertShFindsOnlyALivePod(t *testing.T) {
	for name, pods := range map[string]string{"the list is empty": "none", "every pod is terminating": "terminating"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ALERT_STUB_PODS", pods)
			bin, logs := alertStubs(t, "alertmanager-kind-config")
			code, stdout, stderr := fireAlert(t, bin, "KubePodCrashLooping")
			if code != 1 || stdout != "" || !strings.Contains(stderr, "there is no checkout pod in namespace shop") {
				t.Errorf("exit %d, stdout %q, stderr %q; want 1 and no pod found", code, stdout, stderr)
			}
			if readLog(t, logs, "kubectl.stdin") != "" || strings.Contains(readLog(t, logs, "kubectl.argv"), " run ") {
				t.Error("an alert was posted")
			}
		})
	}
}

// TestAlertShDeletesEachProbePod: every try's probe pod is deleted when that try is over, whether it answered or not, so that tries
// that fail leave nothing behind; none is left for the exit trap but one that is in flight.
func TestAlertShDeletesEachProbePod(t *testing.T) {
	run := regexp.MustCompile(` -n monitoring run (sandbox-alert-\d+) `)
	del := regexp.MustCompile(` -n monitoring delete pod (sandbox-alert-\d+) --ignore-not-found`)
	for name, tc := range map[string]struct {
		fails, tries int
		ok           bool
	}{"the first try answers": {0, 1, true}, "two tries get nothing": {2, 3, true}, "every try gets nothing": {3, 3, false}} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ALERT_STUB_RUNFAILS", strconv.Itoa(tc.fails))
			bin, logs := alertStubs(t, "alertmanager-kind-config")
			code, _, stderr := fireAlert(t, bin, "TargetDown")
			if (code == 0) != tc.ok || (!tc.ok && !strings.Contains(stderr, "HTTP none")) {
				t.Errorf("exit %d, stderr %q; want success: %t", code, stderr, tc.ok)
			}
			var runs, dels []string
			for _, m := range run.FindAllStringSubmatch(readLog(t, logs, "kubectl.argv"), -1) {
				runs = append(runs, m[1])
			}
			for _, m := range del.FindAllStringSubmatch(readLog(t, logs, "kubectl.argv"), -1) {
				dels = append(dels, m[1])
			}
			if len(runs) != tc.tries || !reflect.DeepEqual(runs, dels) {
				t.Errorf("probe pods run %v, deleted %v; want %d tries, each deleted once, in order", runs, dels, tc.tries)
			}
		})
	}
}

// TestUpPreflightSetsAlertSource: the check of the webhook file sets ALERT_SOURCE to 1 for a good file and to 0, with the one line, when
// there is none; a bad file stops it before either. The step is run as up.sh has it, after the real env.sh and webhook.sh.
func TestUpPreflightSetsAlertSource(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	start := strings.Index(s, "if [ -e \"${ALERTMANAGER_WEBHOOK_FILE}\" ]")
	if start < 0 {
		t.Fatal("up.sh does not test for the webhook file")
	}
	end := strings.Index(s[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("the webhook file check has no fi")
	}
	block := s[start : start+end+len("\nfi\n")]
	for name, tc := range map[string]struct {
		path    string
		wantOut string
		refused bool
	}{
		"no file":  {filepath.Join(t.TempDir(), "nowhere"), "alert source: not configured; see README Alert trial\nALERT_SOURCE=0\n", false},
		"a file":   {webhookFile(t, 0o600, fakeWebhook+"\n"), "ALERT_SOURCE=1\n", false},
		"bad file": {webhookFile(t, 0o644, fakeWebhook+"\n"), "", true},
	} {
		t.Run(name, func(t *testing.T) {
			script := "set -eu\n. \"$1\"\n. \"$2\"\nstep() { :; }\nSLACK_CHANNEL_ID=C0SANDBOX01\nALERTMANAGER_WEBHOOK_FILE=$3\n" + block + "echo \"ALERT_SOURCE=${ALERT_SOURCE}\"\n"
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", sandboxDir+"env.sh", sandboxDir+"webhook.sh", tc.path)
			var out, errOut bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errOut
			err := cmd.Run()
			if tc.refused {
				if err == nil || strings.Contains(out.String(), "ALERT_SOURCE") {
					t.Errorf("a bad file was not refused: %v, %q", err, out.String())
				}
				return
			}
			// The step's own "ok: <file> holds ..." line is not what is compared: the lines after it are.
			got := out.String()
			if i := strings.Index(got, "ok: "); i >= 0 {
				got = got[strings.Index(got[i:], "\n")+i+1:]
			}
			if err != nil || got != tc.wantOut {
				t.Errorf("%v, stdout %q, stderr %q; want %q", err, out.String(), errOut.String(), tc.wantOut)
			}
		})
	}
}

// TestAlertShFailsWhenAlertmanagerRefuses: a status other than 200 is a failure that says the status and not the body.
func TestAlertShFailsWhenAlertmanagerRefuses(t *testing.T) {
	bin, _ := alertStubs(t, "alertmanager-kind-config")
	raw, err := os.ReadFile(filepath.Join(bin, "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(strings.Replace(string(raw), "printf 200", "printf 400", 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := fireAlert(t, bin, "TargetDown")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "HTTP 400") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1 and the status", code, stdout, stderr)
	}
}

// TestDownRemovesTheAlertSource: down.sh takes the three fields off the Alertmanager first, so that it is never left pointing at
// a Secret that is gone, and then deletes the two Secrets.
func TestDownRemovesTheAlertSource(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "down.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	patch := strings.Index(s, `patch alertmanager agent-kourier --type=merge -p '{"spec":{"configSecret":null,"secrets":null,"externalUrl":null}}'`)
	del := strings.Index(s, "delete secret alertmanager-slack-secret alertmanager-kind-config --ignore-not-found")
	guard := strings.Index(s, "get alertmanager agent-kourier --ignore-not-found")
	if guard < 0 || patch < guard || del < patch {
		t.Errorf("down.sh: the guard at %d, the patch at %d, the delete at %d; want them in that order (the patch is only made when the Alertmanager is there)", guard, patch, del)
	}
	// Run as down.sh has it, with a kubectl that answers what the Alertmanager's configSecret is: the Alertmanager is patched only when
	// it is the alert source, the Secrets are deleted either way (and ignore-not-found makes that safe to repeat), and nothing is
	// asked of a cluster that has no Alertmanager at all.
	start := strings.Index(s, "if [ \"$(\"${K[@]}\" -n monitoring get alertmanager agent-kourier")
	endMarker := `delete secret alertmanager-slack-secret alertmanager-kind-config --ignore-not-found >/dev/null` + "\n"
	end := strings.Index(s, endMarker)
	if start < 0 || end < start {
		t.Fatalf("down.sh: the alert-source step is at %d..%d", start, end)
	}
	block := s[start : end+len(endMarker)]
	// The pod's arguments, as the wait reads them: POD_MODE "" is a pod that has none of ours, "kind-then-gone" has the alert source's
	// external URL on the first look and not after (the operator's roll), and "other" has an external URL that is not ours, which is
	// what a revert looks like when the tier has set one of its own: it is not the alert source, so the wait is over.
	for name, tc := range map[string]struct {
		configSecret string
		patched      bool
		podMode      string
		podLooks     int // how many times the pod's arguments are read
	}{
		"the alert source is up":                         {"alertmanager-kind-config", true, "", 1},
		"the alert source is up and the pod rolls":       {"alertmanager-kind-config", true, "kind-then-gone", 2},
		"the pod has another external URL after reverts": {"alertmanager-kind-config", true, "other", 1},
		"the Alertmanager is the tier's":                 {"", false, "", 0},
		"another configuration":                          {"alertmanager-other", false, "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			bin, logs := t.TempDir(), t.TempDir()
			stub := "#!/bin/sh\necho \"$*\" >> " + logs + "/argv\ncase \"$*\" in\n  *\"get alertmanager agent-kourier\"*) printf %s \"$CONFIG_SECRET\" ;;\n" +
				"  *\"get pod alertmanager-agent-kourier-0\"*)\n    case \"$POD_MODE\" in\n" +
				"      kind-then-gone) n=$(cat " + logs + "/n 2>/dev/null || echo 0); n=$((n + 1)); echo \"$n\" > " + logs + "/n\n" +
				"        [ \"$n\" -gt 1 ] || printf '%s' '[\"--web.external-url=http://alertmanager.kind.local\"]' ;;\n" +
				"      other) printf '%s' '[\"--web.external-url=http://other.example\"]' ;;\n    esac ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			script := "set -eu\nK=(kubectl --context kind-agent-kourier-full)\ndie() { echo \"error: $*\" >&2; exit 1; }\n" + block
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CONFIG_SECRET="+tc.configSecret, "POD_MODE="+tc.podMode)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			argv := readLog(t, logs, "argv")
			if got := strings.Count(argv, " get pod alertmanager-agent-kourier-0 "); got != tc.podLooks {
				t.Errorf("the pod's arguments were read %d times, want %d\n%s", got, tc.podLooks, argv)
			}
			if got := strings.Contains(argv, " patch alertmanager agent-kourier "); got != tc.patched {
				t.Errorf("patched: %t, want %t\n%s", got, tc.patched, argv)
			}
			if got := strings.Contains(argv, " wait "); got != tc.patched {
				t.Errorf("waited for the Alertmanager: %t, want %t\n%s", got, tc.patched, argv)
			}
			if !strings.Contains(argv, "-n monitoring delete secret alertmanager-slack-secret alertmanager-kind-config --ignore-not-found") {
				t.Errorf("the Secrets were not deleted (with ignore-not-found):\n%s", argv)
			}
			for _, line := range strings.Split(strings.TrimSpace(argv), "\n") {
				if !strings.HasPrefix(line, "--context kind-agent-kourier-full ") {
					t.Errorf("kubectl was run without the context: %q", line)
				}
			}
		})
	}
}

// TestSandboxAlertTarget: the Makefile target, with its help line and in .PHONY, and the README's section.
func TestSandboxAlertTarget(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^sandbox-alert: .*## \S`).Match(mk) || !regexp.MustCompile(`(?m)^\.PHONY:.*\bsandbox-alert\b`).Match(mk) ||
		!strings.Contains(string(mk), `hack/kind/sandbox/alert.sh "$(ALERT)" "$(STATE)"`) || !regexp.MustCompile(`(?m)^STATE\s+\?= firing$`).Match(mk) {
		t.Error("the Makefile has no complete sandbox-alert target (help line, .PHONY, STATE defaulting to firing, the script with ALERT and STATE)")
	}
	if info, err := os.Stat(sandboxDir + "alert.sh"); err != nil || info.Mode()&0o111 != 0o111 {
		t.Errorf("alert.sh is not executable (%v, %v), and make runs it directly", info, err)
	}
	readme, err := os.ReadFile(sandboxDir + "README.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(readme)
	i := strings.Index(section, "\n## Alert trial\n")
	if i < 0 {
		t.Fatal("the README has no \"Alert trial\" section")
	}
	section = section[i+1:]
	if j := strings.Index(section[3:], "\n## "); j >= 0 {
		section = section[:j+3]
	}
	for _, want := range []string{
		"real alert channel", "[kind]", "make sandbox-alert ALERT=", "STATE=resolved",
		"receiver=slack-critical-kind", "make sandbox-up", "api/v2/status", "alertmanager-webhook-url", "chmod 600",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the Alert trial section does not mention %q", want)
		}
	}
	for name := range loadSamples(t) {
		if !strings.Contains(section, name) {
			t.Errorf("the Alert trial section does not list the sample alert %s", name)
		}
	}
}

// TestSandboxScriptsPassShellcheck: shellcheck is clean on every sandbox script. It skips when shellcheck is not installed.
func TestSandboxScriptsPassShellcheck(t *testing.T) {
	path, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck is not installed")
	}
	scripts, err := filepath.Glob(sandboxDir + "*.sh")
	if err != nil || len(scripts) < 6 {
		t.Fatalf("scripts %v, %v", scripts, err)
	}
	out, err := exec.CommandContext(t.Context(), path, append([]string{"-x"}, scripts...)...).CombinedOutput()
	if err != nil {
		t.Errorf("shellcheck:\n%s", out)
	}
}
