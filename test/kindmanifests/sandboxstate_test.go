package kindmanifests_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// up.sh decides whether the sandbox's Agent Kourier came up from agentkourier_state, which reads the newest of the pods it lists. The
// release's token refresh CronJob makes pods with the release's instance label and a name label of their own
// (charts/agent-kourier/templates/tokenrefresh.yaml), and every release of the chart in the namespace has the broker's name label, so the
// broker's own pods are the ones that carry both. These tests run the function as up.sh defines it against a stub kubectl.

// stateStub is a kubectl that answers the one call agentkourier_state makes as the API server would, from the pods in its directory:
// it keeps the pods with every key=value of the selector, and a term of any other form matches no pod. It fails any other call.
const stateStub = `#!/bin/sh
case "$*" in
  "--context kind-agent-kourier-full -n agent-kourier get pods -l "*" -o json") ;;
  *) echo "stub kubectl: unexpected call: $*" >&2; exit 99 ;;
esac
exec jq --arg selector "$8" '.items |= map(select(.metadata.labels as $labels
  | all($selector | split(",")[] | split("="); length == 2 and $labels[.[0]] == .[1])))' "$STATE_STUB/pods.json"
`

const (
	brokerName      = "agent-kourier"
	refreshName     = "agent-kourier-token-refresh"
	sandboxInstance = "agent-kourier-sandbox"
)

var (
	stateFunction  = regexp.MustCompile(`(?ms)^agentkourier_state\(\) \{.*?^\}$`)
	sandboxRelease = regexp.MustCompile(`(?m)^SANDBOX_RELEASE=.*$`)
)

// statePod is one pod of a kubectl answer: its name and instance labels, created age ago, and its first container's status (JSON), if any.
type statePod struct {
	name, instance string
	age            time.Duration
	status         string
	terminating    bool
}

func (p statePod) json() string {
	meta := fmt.Sprintf(`"creationTimestamp":%q,"labels":{"app.kubernetes.io/name":%q,"app.kubernetes.io/instance":%q}`,
		time.Now().Add(-p.age).UTC().Format(time.RFC3339), p.name, p.instance)
	if p.terminating {
		meta += `,"deletionTimestamp":"2026-01-01T00:00:00Z"`
	}
	status := "{}"
	if p.status != "" {
		status = `{"containerStatuses":[` + p.status + `]}`
	}
	return `{"metadata":{` + meta + `},"status":` + status + `}`
}

// runState runs agentkourier_state and SANDBOX_RELEASE's assignment, cut from up.sh, under up.sh's shell options against the stub kubectl.
func runState(t *testing.T, list []statePod) string {
	t.Helper()
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	function, release := stateFunction.Find(raw), sandboxRelease.Find(raw)
	if function == nil || release == nil {
		t.Fatalf("up.sh: agentkourier_state %q, SANDBOX_RELEASE %q; want both", function, release)
	}
	items := make([]string, 0, len(list))
	for _, p := range list {
		items = append(items, p.json())
	}
	stub, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "pods.json"), []byte(`{"items":[`+strings.Join(items, ",")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stateStub), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "set -eEuo pipefail\nK=(kubectl --context kind-agent-kourier-full)\n" + string(release) + "\n" + string(function) + "\nagentkourier_state\n"
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
	cmd.Env = append(os.Environ(), "STATE_STUB="+stub, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("agentkourier_state: %v\n%s", err, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

// TestAgentKourierStateReadsTheBrokersOwnPods: whatever else is in the namespace, and however new, the state is the newest broker pod's.
// Without the name label a refresh pod made after the broker's is read: a completed one fails the bring-up with "exit 0", a running one
// holds it at "starting". Without the instance label another release's broker is read.
func TestAgentKourierStateReadsTheBrokersOwnPods(t *testing.T) {
	const (
		ready     = `{"ready":true,"state":{"running":{}}}`
		running   = `{"ready":false,"state":{"running":{}}}`
		fatalAuth = `{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1}}}`
		completed = `{"ready":false,"state":{"terminated":{"exitCode":0}}}`
	)
	broker := func(age time.Duration, status string) statePod {
		return statePod{name: brokerName, instance: sandboxInstance, age: age, status: status}
	}
	refresh := func(age time.Duration, status string) statePod {
		return statePod{name: refreshName, instance: sandboxInstance, age: age, status: status}
	}
	for desc, tc := range map[string]struct {
		pods []statePod
		want string
	}{
		"a ready broker": {[]statePod{broker(time.Hour, ready)}, "ready"},
		"a ready broker, a newer completed refresh":    {[]statePod{broker(time.Hour, ready), refresh(time.Minute, completed)}, "ready"},
		"a ready broker, a newer running refresh":      {[]statePod{broker(time.Hour, ready), refresh(time.Minute, running)}, "ready"},
		"a broker rejected by Slack, a newer refresh":  {[]statePod{broker(time.Hour, fatalAuth), refresh(time.Minute, completed)}, "fatal-auth"},
		"a starting broker, a newer completed refresh": {[]statePod{broker(time.Hour, running), refresh(time.Minute, completed)}, "starting"},
		"no broker yet, a completed refresh":           {[]statePod{refresh(time.Minute, completed)}, ""},
		"a newer broker of another release": {[]statePod{
			broker(time.Hour, fatalAuth), {name: brokerName, instance: "agent-kourier", age: time.Minute, status: ready},
		}, "fatal-auth"},
		"a rollout, the newest broker still starting": {[]statePod{broker(time.Hour, ready), broker(time.Minute, running)}, "starting"},
		"a rollout, the newest broker terminating": {[]statePod{
			broker(time.Hour, ready), {name: brokerName, instance: sandboxInstance, age: time.Minute, status: running, terminating: true},
		}, "ready"},
	} {
		t.Run(desc, func(t *testing.T) {
			if got := runState(t, tc.pods); got != tc.want {
				t.Errorf("agentkourier_state = %q; want %q", got, tc.want)
			}
		})
	}
}
