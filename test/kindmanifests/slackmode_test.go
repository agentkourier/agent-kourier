package kindmanifests_test

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// libVar runs lib.sh and prints what script writes, with env (NAME=VALUE) added to the environment, and fails the test if it fails.
func libVar(t *testing.T, script string, env ...string) string {
	t.Helper()
	out, err := libRun(t, script, env...)
	if err != nil {
		t.Fatalf("%s = %v\n%s", script, err, out)
	}
	return out
}

// libRun runs script with lib.sh sourced and returns its combined output and error.
func libRun(t *testing.T, script string, env ...string) (string, error) {
	t.Helper()
	lib, err := filepath.Abs("../../hack/kind/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -eu; . "$1"; `+script, "bash", lib)
	cmd.Env = append(os.Environ(), env...)
	raw, err := cmd.CombinedOutput()
	return string(raw), err
}

// TestTheFakeSlackSendsHTTPModeToTheTiersChatRoute: in http mode the patch of the Deployment fakeslack gives the default app the URL of the tier's
// chat Service, on the port that http-values.yaml gives it, at the route of the tier's ChatConnection, and the secret that deploy_agentkourier puts in
// that connection's Secret; in socket mode it takes the args away. doubles.yaml sets no args, so that kubectl apply leaves them to the patch.
func TestTheFakeSlackSendsHTTPModeToTheTiersChatRoute(t *testing.T) {
	type patch struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string    `json:"name"`
						Args *[]string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	decode := func(mode string) patch {
		var p patch
		raw := libVar(t, "fake_slack_mode_patch", "E2E_SLACK_MODE="+mode)
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("the %s patch %q is not JSON: %v", mode, raw, err)
		}
		if c := p.Spec.Template.Spec.Containers; len(c) != 1 || c[0].Name != "fakeslack" {
			t.Fatalf("the %s patch %q does not name the container fakeslack alone", mode, raw)
		}
		return p
	}

	if args := decode("socket").Spec.Template.Spec.Containers[0].Args; args != nil {
		t.Errorf("the socket patch sets args %v; want null, which takes away those of http mode", *args)
	}
	args := decode("http").Spec.Template.Spec.Containers[0].Args
	if args == nil || len(*args) != 4 || (*args)[0] != "-request-url" || (*args)[2] != "-signing-secret" {
		t.Fatalf("the http patch sets args %v; want -request-url URL -signing-secret SECRET", args)
	}
	requestURL, secret := (*args)[1], (*args)[3]

	// The URL: the chat Service of release agent-kourier in namespace agent-kourier, on its port, at the route of the tier's own ChatConnection.
	var values struct {
		ChatListener struct {
			Enabled bool `json:"enabled"`
			Port    int  `json:"port"`
			Service struct {
				Port int `json:"port"`
			} `json:"service"`
		} `json:"chatListener"`
	}
	loadValues(t, &values)
	var conn v1alpha1.ChatConnection
	load(t, agentKourierDir+"config/chatconnection.yaml", "ChatConnection", &conn)
	u, err := url.Parse(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/chat/slack/" + conn.Namespace + "/" + conn.Name; u.Scheme != "http" || u.Hostname() != "agent-kourier-chat.agent-kourier.svc.cluster.local" ||
		u.Port() != strconv.Itoa(values.ChatListener.Service.Port) || u.Path != want {
		t.Errorf("the fake Slack sends to %s; want http://agent-kourier-chat.agent-kourier.svc.cluster.local:%d%s, the chat Service of http-values.yaml and the tier's ChatConnection",
			requestURL, values.ChatListener.Service.Port, want)
	}
	if !values.ChatListener.Enabled || values.ChatListener.Port != 8081 || values.ChatListener.Service.Port != 8081 {
		t.Errorf("http-values.yaml's chatListener is %+v; want it enabled on 8081, with its Service on 8081", values.ChatListener)
	}
	// The secret is the one deploy_agentkourier puts in the Secret slack-fast.
	if got := libVar(t, `printf %s "${FAKE_SLACK_SIGNING_SECRET}"`); got != secret {
		t.Errorf("the fake Slack signs with %q, and FAKE_SLACK_SIGNING_SECRET is %q", secret, got)
	}
	lib, err := os.ReadFile("../../hack/kind/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lib), `--from-literal=signingSecret="${FAKE_SLACK_SIGNING_SECRET}"`) {
		t.Error("deploy_agentkourier does not put FAKE_SLACK_SIGNING_SECRET in the Secret slack-fast as signingSecret")
	}

	if c := fakeSlackDeployment(t).Spec.Template.Spec.Containers; len(c) != 1 || c[0].Args != nil {
		t.Errorf("doubles.yaml's fakeslack sets args %v; want none, so that the patch alone decides them", c)
	}
}

// loadValues reads hack/kind/http-values.yaml into into.
func loadValues(t *testing.T, into any) {
	t.Helper()
	docs := objects(t, "../../hack/kind/http-values.yaml")
	if len(docs) != 1 {
		t.Fatalf("http-values.yaml holds %d documents; want 1", len(docs))
	}
	b, err := json.Marshal(docs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

// fakeSlackDeployment returns the Deployment fakeslack of doubles.yaml.
func fakeSlackDeployment(t *testing.T) appsv1.Deployment {
	t.Helper()
	for _, doc := range objects(t, "../../hack/kind/fast/doubles.yaml") {
		if meta, ok := doc["metadata"].(map[string]any); ok && doc["kind"] == "Deployment" && meta["name"] == "fakeslack" {
			b, _ := json.Marshal(doc)
			var d appsv1.Deployment
			if err := json.Unmarshal(b, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatal("doubles.yaml has no Deployment fakeslack")
	return appsv1.Deployment{}
}

// TestTheChatFromOfHTTPModeAdmitsTheFakeSlack: chatFrom selects the fake Slack's pods, in its namespace, by the labels doubles.yaml gives them.
func TestTheChatFromOfHTTPModeAdmitsTheFakeSlack(t *testing.T) {
	var values struct {
		NetworkPolicy struct {
			ChatFrom []struct {
				NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`
				PodSelector       metav1.LabelSelector `json:"podSelector"`
			} `json:"chatFrom"`
		} `json:"networkPolicy"`
	}
	loadValues(t, &values)
	fake := fakeSlackDeployment(t)
	from := values.NetworkPolicy.ChatFrom
	if len(from) != 1 || !reflect.DeepEqual(from[0].NamespaceSelector.MatchLabels, map[string]string{"kubernetes.io/metadata.name": fake.Namespace}) ||
		!reflect.DeepEqual(from[0].PodSelector.MatchLabels, fake.Spec.Template.Labels) {
		t.Errorf("chatFrom is %+v; want the namespace %s and the pod labels %v of the fake Slack", from, fake.Namespace, fake.Spec.Template.Labels)
	}
}

// TestTheTiersPutTheirConnectionInHTTPMode: in http mode the tier's ChatConnection, as deploy_agentkourier assembles it (the same file is the
// ConfigMap's and the custom resource's), is in HTTP mode and resolves with the Secret it makes; in socket mode the file is left as it is.
func TestTheTiersPutTheirConnectionInHTTPMode(t *testing.T) {
	original, err := os.ReadFile(agentKourierDir + "config/chatconnection.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"socket", "http"} {
		dir := fastConfig(t)
		libVar(t, `slack_mode_config "$DIR"`, "E2E_SLACK_MODE="+mode, "DIR="+dir)
		got, err := os.ReadFile(filepath.Join(dir, "chatconnection.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if mode == "socket" && string(got) != string(original) {
			t.Errorf("socket mode changed the ChatConnection:\n%s", got)
		}
		set, err := config.Load(t.Context(), dir)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		snap, err := config.Resolve(t.Context(), set, upFastSecrets)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		var conn v1alpha1.ChatConnection
		load(t, filepath.Join(dir, "chatconnection.yaml"), "ChatConnection", &conn)
		if want := v1alpha1.SlackMode(mode); conn.Spec.SlackMode() != want {
			t.Errorf("E2E_SLACK_MODE=%s: the ChatConnection's mode is %s", mode, conn.Spec.SlackMode())
		}
		if _, ok := snap.BindingForChannel("agent-kourier", "slack-fast", "C1"); !ok {
			t.Errorf("%s: channel C1 does not resolve on slack-fast", mode)
		}
		// HTTP mode reads signingSecret: without it in the Secret, the config does not resolve.
		if mode == "http" {
			partial := secrets{}
			for k, v := range upFastSecrets {
				partial[k] = v
			}
			partial["agent-kourier/slack-fast"] = []string{"botToken", "appToken"}
			if _, err := config.Resolve(t.Context(), set, partial); err == nil {
				t.Error("the connection in HTTP mode resolved without signingSecret in its Secret")
			}
		}
	}
}

// TestTheTiersAddTheChatListenerInHTTPMode: in http mode every helm call of deploy_agentkourier, on either route, carries http-values.yaml; in
// socket mode none does, so that a tier brought up again in socket mode loses the listener.
func TestTheTiersAddTheChatListenerInHTTPMode(t *testing.T) {
	for _, tc := range []struct{ source, owner string }{{"files", ""}, {"files", "agent-kourier"}, {"crds", ""}, {"crds", "agent-kourier"}} {
		for _, mode := range []string{"socket", "http"} {
			calls, out, err := deploy(t, tc.source, tc.owner, "E2E_SLACK_MODE="+mode)
			if err != nil {
				t.Fatalf("%s, CRDs held by %q, %s: %v\n%s", tc.source, tc.owner, mode, err, out)
			}
			helm := helmCalls(calls)
			if len(helm) == 0 {
				t.Fatalf("%s, %s: no helm call", tc.source, mode)
			}
			for _, h := range helm {
				if has := strings.Contains(h, "-f "+filepath.Join(hackDir(t), "http-values.yaml")); has != (mode == "http") {
					t.Errorf("%s, CRDs held by %q, %s: the helm call %q carries http-values.yaml: %v, want %v", tc.source, tc.owner, mode, h, has, mode == "http")
				}
			}
		}
	}
}

func hackDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../../hack/kind")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestTheTiersRefuseASlackModeTheyDoNotKnow: the value is checked before anything is changed, by the helper and by the scripts that bring a tier up;
// the AX tier, whose Agent Kourier has no chat listener, refuses http, and so does the tier of two Slack apps, whose MC_ tests run in socket mode only.
func TestTheTiersRefuseASlackModeTheyDoNotKnow(t *testing.T) {
	calls, out, err := deploy(t, "files", "", "E2E_SLACK_MODE=websocket")
	if err == nil || !strings.Contains(out, "E2E_SLACK_MODE must be socket or http") || len(calls) != 0 {
		t.Errorf("deploy_agentkourier with an unknown mode = %v, %d calls\n%s\nwant it to refuse before it touches the cluster", err, len(calls), out)
	}
	if out, err := libRun(t, "K=(kubectl); HERE=/nowhere; apply_doubles", "E2E_SLACK_MODE=websocket"); err == nil || !strings.Contains(out, "E2E_SLACK_MODE must be socket or http") {
		t.Errorf("apply_doubles with an unknown mode = %v\n%s", err, out)
	}
	bin, logs := t.TempDir(), t.TempDir()
	for _, tool := range []string{"kind", "docker", "kubectl", "helm", "go", "curl", "openssl", "jq"} {
		stub := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> \"$STUB_LOG/called\"\nexit 99\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		script string
		env    []string
		want   string
	}{
		{"up-fast.sh", []string{"E2E_SLACK_MODE=websocket"}, "E2E_SLACK_MODE must be socket or http"},
		{"up-full.sh", []string{"E2E_SLACK_MODE=websocket"}, "E2E_SLACK_MODE must be socket or http"},
		{"up-fast.sh", []string{"E2E_SLACK_MODE=http", "E2E_TWO_APPS=1", "E2E_CONFIG_SOURCE=crds"}, "E2E_TWO_APPS=1 needs E2E_SLACK_MODE=socket"},
		{"up-ax.sh", []string{"E2E_SLACK_MODE=http"}, "the AX tier runs Socket Mode"},
	} {
		cmd := exec.CommandContext(t.Context(), "bash", "../../hack/kind/"+tc.script)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+logs)
		cmd.Env = append(cmd.Env, tc.env...)
		raw, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(raw), tc.want) {
			t.Errorf("%s with %v = %v\n%s\nwant it to refuse: %s", tc.script, tc.env, err, raw, tc.want)
		}
		if called := readLog(t, logs, "called"); called != "" {
			t.Errorf("%s with %v called %q before it refused; want no tool called", tc.script, tc.env, called)
		}
	}
}

// TestRunnerJobGetsTheSlackMode: E2E_SLACK_MODE in the Job is a placeholder that e2e-run.sh fills in from the fake Slack's Deployment as it is when
// the Job is made (the args fake_slack_mode_patch sets), so that a tier moved between the modes in place is run as it is.
func TestRunnerJobGetsTheSlackMode(t *testing.T) {
	var job batchv1.Job
	load(t, "../../hack/kind/e2e/runner.yaml", "Job", &job)
	var mode string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "E2E_SLACK_MODE" {
			mode = e.Value
		}
	}
	script, err := os.ReadFile("../../hack/kind/e2e-run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if mode != "@SLACK_MODE@" || !strings.Contains(string(script), `-e "s/@SLACK_MODE@/${slack_mode}/"`) ||
		!strings.Contains(string(script), `get deploy/fakeslack -o 'jsonpath={.spec.template.spec.containers[0].args}'`) ||
		!strings.Contains(string(script), "*-request-url*) slack_mode=http") {
		t.Errorf("E2E_SLACK_MODE = %q in the Job; want the placeholder @SLACK_MODE@, which e2e-run.sh sets from the args of the Deployment fakeslack", mode)
	}
}
