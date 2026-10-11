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

// heal.sh restarts the full tier's egress gateway (deploy/atenet-egress in ate-system) when its client certificate is, or is about
// to be, expired: the gateway reads it once, at start, and a Substrate pod certificate lives 24 hours. These tests run its
// function against a stub kubectl, and pin where up.sh calls it and what `make sandbox-heal` runs.

// healStub is a kubectl that answers the calls heal_egress_gateway makes from files in its directory, logs every call to argv,
// and fails any other, or any call that does not name the full tier's context first.
const healStub = `#!/bin/sh
echo "$*" >> "$HEAL_STUB/argv"
case "$1 $2" in
  "--context kind-agent-kourier-full") ;;
  *) echo "stub kubectl: a call without --context kind-agent-kourier-full first: $*" >&2; exit 98 ;;
esac
case "$*" in
  "--context kind-agent-kourier-full -n ate-system get pods -l app=atenet-egress -o json") cat "$HEAL_STUB/pods.json" ;;
  "--context kind-agent-kourier-full -n ate-system logs deploy/atenet-egress "*) cat "$HEAL_STUB/gateway.log" ;;
  "--context kind-agent-kourier-full -n kagent logs deploy/kagent-default "*) cat "$HEAL_STUB/pool.log" ;;
  "--context kind-agent-kourier-full -n ate-system rollout restart deploy/atenet-egress") ;;
  "--context kind-agent-kourier-full -n ate-system rollout status deploy/atenet-egress --timeout=180s") exit "${HEAL_STATUS_EXIT:-0}" ;;
  *) echo "stub kubectl: unexpected call: $*" >&2; exit 99 ;;
esac
`

const (
	certExpiredLine = `{"level":"WARN","msg":"atunnel failed to open egress tunnel","err":"atunnel: egress gateway rejected CONNECT with 403 Forbidden: ... received fatal alert: CertificateExpired"}`
	healthyLine     = `{"level":"INFO","msg":"Checking health"}`
)

// pod is one egress pod of a kubectl answer, created age ago, and terminating if deleted is not empty.
func pod(age time.Duration, deleted string) string {
	meta := fmt.Sprintf(`"creationTimestamp":%q`, time.Now().Add(-age).UTC().Format(time.RFC3339))
	if deleted != "" {
		meta += fmt.Sprintf(`,"deletionTimestamp":%q`, deleted)
	}
	return `{"metadata":{` + meta + `}}`
}

// pods is what kubectl returns for the egress pods given.
func pods(items ...string) string { return `{"items":[` + strings.Join(items, ",") + `]}` }

// runHeal runs heal_egress_gateway as up.sh does (errexit, errtrace, pipefail, K naming the context) against the stub kubectl,
// with the pods, the gateway's log and the worker pool's log as given. It returns what it printed, kubectl's calls and its error.
func runHeal(t *testing.T, podList, gatewayLog, poolLog string, env ...string) (output, argv string, err error) {
	t.Helper()
	stub := t.TempDir()
	for name, content := range map[string]string{"pods.json": podList, "gateway.log": gatewayLog, "pool.log": poolLog} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(healStub), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -eEuo pipefail
. "$1"
. "$2"
K=(kubectl --context kind-agent-kourier-full)
heal_egress_gateway`, "bash", "../../hack/kind/lib.sh", sandboxDir+"heal.sh")
	cmd.Env = append(os.Environ(), "HEAL_STUB="+stub, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), readLog(t, stub, "argv"), err
}

// TestHealRestartsTheEgressGatewayWhenItsCertificateIsStale: an old pod restarts; a young pod with no expired-certificate line in
// either log does not; a young pod with one in the worker pool's log, or in its own, does. The restart is waited for.
func TestHealRestartsTheEgressGatewayWhenItsCertificateIsStale(t *testing.T) {
	const (
		restart = "--context kind-agent-kourier-full -n ate-system rollout restart deploy/atenet-egress"
		status  = "--context kind-agent-kourier-full -n ate-system rollout status deploy/atenet-egress --timeout=180s"
		fine    = "ok: the egress gateway's certificate is fine"
		deleted = "2026-10-04T00:00:00Z"
	)
	clean := healthyLine + "\n"
	for name, tc := range map[string]struct {
		pods, gatewayLog, poolLog string
		restarts                  bool
		say                       string
	}{
		"an old pod":                          {pods(pod(30*time.Hour, "")), clean, clean, true, "is 30h old"},
		"a pod just past the threshold":       {pods(pod(20*time.Hour+5*time.Minute, "")), clean, clean, true, "is 20h old"},
		"a young pod and clean logs":          {pods(pod(2*time.Hour, "")), clean, clean, false, fine},
		"a pod just under the threshold":      {pods(pod(19*time.Hour+55*time.Minute, "")), clean, clean, false, fine},
		"a young pod, worker pool log has it": {pods(pod(2*time.Hour, "")), clean, clean + certExpiredLine + "\n", true, "CertificateExpired"},
		"a young pod, its own log has it":     {pods(pod(2*time.Hour, "")), clean + certExpiredLine + "\n", clean, true, "CertificateExpired"},
		"the newer of two pods is young":      {pods(pod(30*time.Hour, ""), pod(time.Hour, "")), clean, clean, false, fine},
		"an old pod is terminating":           {pods(pod(30*time.Hour, deleted), pod(time.Hour, "")), clean, clean, false, fine},
		"a young pod is terminating":          {pods(pod(30*time.Hour, ""), pod(time.Hour, deleted)), clean, clean, true, "is 30h old"},
	} {
		t.Run(name, func(t *testing.T) {
			out, argv, err := runHeal(t, tc.pods, tc.gatewayLog, tc.poolLog)
			if err != nil {
				t.Fatalf("heal_egress_gateway: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.say) {
				t.Errorf("output %q; want it to say %q", out, tc.say)
			}
			if got := strings.Contains(argv, restart); got != tc.restarts {
				t.Errorf("restarted: %t, want %t; kubectl was asked:\n%s", got, tc.restarts, argv)
			}
			// The restart is waited for, and is the last thing done.
			if tc.restarts && !strings.HasSuffix(argv, restart+"\n"+status+"\n") {
				t.Errorf("kubectl was asked:\n%s\nwant the restart and then its rollout status last", argv)
			}
			if !tc.restarts && strings.Contains(argv, "rollout") {
				t.Errorf("a gateway that needs no restart was asked about a rollout:\n%s", argv)
			}
		})
	}
}

// TestHealReadsOnlyTheLogsSinceThePodStarted: a line from before the gateway's own start is the last restart's, and a restart
// does not clear the worker pool's log, so reading it whole would restart a healthy gateway on every run. Both logs are read
// from the pod's creation on, and no further back than the last lines of a noisy log, and an old pod is not asked at all.
func TestHealReadsOnlyTheLogsSinceThePodStarted(t *testing.T) {
	created := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	_, argv, err := runHeal(t, fmt.Sprintf(`{"items":[{"metadata":{"creationTimestamp":%q}}]}`, created), healthyLine+"\n", healthyLine+"\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []struct{ ns, deploy string }{{"ate-system", "atenet-egress"}, {"kagent", "kagent-default"}} {
		call := regexp.MustCompile(`(?m)^--context kind-agent-kourier-full -n ` + l.ns + ` logs deploy/` + l.deploy + ` .*$`).FindString(argv)
		if !strings.Contains(call+" ", " --since-time="+created+" ") || !regexp.MustCompile(` --tail=\d+`).MatchString(call) {
			t.Errorf("the log of %s was read as %q; want it since the pod's creation, %s, and with a --tail", l.deploy, call, created)
		}
	}
	// An old pod is restarted without a look at any log.
	_, argv, _ = runHeal(t, pods(pod(30*time.Hour, "")), healthyLine, healthyLine)
	if strings.Contains(argv, " logs ") {
		t.Errorf("an old pod's restart read a log:\n%s", argv)
	}
}

// TestHealAsksOnlyThroughTheContextAndStopsWhereItCannotGo: every call names the full tier's context (the stub refuses one that
// does not); no egress pod to look at is an error and no restart; a restart whose rollout does not finish is an error.
func TestHealAsksOnlyThroughTheContextAndStopsWhereItCannotGo(t *testing.T) {
	out, argv, err := runHeal(t, pods(), "", "")
	if err == nil || !strings.Contains(out, "no atenet-egress pod") || strings.Contains(argv, "rollout") {
		t.Errorf("with no pod: err %v, output %q, calls %q; want an error that says there is no pod, and no restart", err, out, argv)
	}
	out, argv, err = runHeal(t, pods(pod(30*time.Hour, "")), "", "", "HEAL_STATUS_EXIT=1")
	if err == nil || !strings.Contains(out, "did not finish") || !strings.Contains(argv, "rollout restart") {
		t.Errorf("with a rollout that fails: err %v, output %q; want an error that says it did not finish", err, out)
	}
	for _, call := range strings.Split(strings.TrimSpace(argv), "\n") {
		if !strings.HasPrefix(call, "--context kind-agent-kourier-full ") {
			t.Errorf("kubectl was called as %q; every call must name --context kind-agent-kourier-full", call)
		}
	}
}

// TestHealThresholdIsANamedConstant: the age limit is one named value of 20 hours, with a comment above it that says why, below
// the 24 hours a Substrate pod certificate lives (maxExpirationSeconds 86400 on the gateway's PodCertificateRequests).
func TestHealThresholdIsANamedConstant(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "heal.sh")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	at := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "EGRESS_MAX_AGE_SECONDS=") {
			at = i
		}
	}
	if at < 1 || !strings.HasPrefix(lines[at-1], "#") || !strings.Contains(lines[at-1], "24") {
		t.Fatalf("EGRESS_MAX_AGE_SECONDS is not one assignment under a comment that says why (the certificate's 24 hours); line %d", at)
	}
	// Sourcing the file prints and runs nothing else.
	out, err := exec.CommandContext(t.Context(), "bash", "-c", `. "$1"; echo "${EGRESS_MAX_AGE_SECONDS}"`, "bash", sandboxDir+"heal.sh").CombinedOutput()
	if err != nil || string(out) != "72000\n" {
		t.Errorf("sourcing heal.sh and printing EGRESS_MAX_AGE_SECONDS gave %q, %v; want 72000 (20 hours) and nothing else", out, err)
	}
}

// TestHealRefusesOutsideTheFullTier: run as `make sandbox-heal` does, it checks that the full tier's cluster exists before it
// asks kubectl anything.
func TestHealRefusesOutsideTheFullTier(t *testing.T) {
	bin := t.TempDir()
	mark := filepath.Join(bin, "kubectl.ran")
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\necho \"$*\" >> "+mark+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "kind"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { // kind get clusters: none
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", sandboxDir+"heal.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "does not exist") {
		t.Errorf("heal.sh with no full tier: err %v, output %q; want it to say the cluster does not exist", err, out)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("heal.sh asked kubectl something before it knew the cluster is the full tier's")
	}
}

// TestHealOnlyUsesTheExplicitContext: in heal.sh, kubectl is named in one place only, the K array that carries the context,
// besides the check that it is installed.
func TestHealOnlyUsesTheExplicitContext(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "heal.sh")
	if err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, line := range strings.Split(string(raw), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") || !strings.Contains(code, "kubectl") {
			continue
		}
		named++
		if code != `K=(kubectl --context "${CTX}")` && code != "for tool in kind kubectl jq; do" {
			t.Errorf("heal.sh names kubectl in %q; it may only do so in K, which names the context", code)
		}
	}
	if named < 2 {
		t.Errorf("heal.sh names kubectl on %d code lines; want K and the check that it is installed", named)
	}
	for _, want := range []string{"assert_context full", "assert_owned"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("heal.sh does not call %s", want)
		}
	}
	if !regexp.MustCompile(`CTX=kind-\$\{CLUSTER\}`).Match(raw) {
		t.Error("heal.sh does not derive the context from the full tier's cluster name")
	}
}

// TestSandboxUpHealsTheGatewayBeforeItVerifies: up.sh sources heal.sh and calls the heal after it has asserted the full tier's
// context and before the "verify" step.
func TestSandboxUpHealsTheGatewayBeforeItVerifies(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	up := string(raw)
	if !strings.Contains(up, `. "${SBX}/heal.sh"`) {
		t.Error(`up.sh does not source "${SBX}/heal.sh"`)
	}
	call := regexp.MustCompile(`(?m)^heal_egress_gateway$`).FindStringIndex(up)
	asserted := strings.Index(up, "\nassert_context full\n")
	verify := strings.Index(up, `step "verify"`)
	if call == nil || asserted < 0 || verify < 0 || call[0] < asserted || call[0] > verify {
		t.Errorf("heal_egress_gateway is called at %v; want it after assert_context full (%d) and before the verify step (%d)", call, asserted, verify)
	}
}

// TestMakeSandboxHealRunsTheHealScript: the target's recipe is heal.sh and nothing else, and the script is executable.
func TestMakeSandboxHealRunsTheHealScript(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^sandbox-heal: .*\n\thack/kind/sandbox/heal\.sh\n`).Match(mk) {
		t.Error("the sandbox-heal target does not run hack/kind/sandbox/heal.sh and nothing else")
	}
	info, err := os.Stat(sandboxDir + "heal.sh")
	if err != nil || info.Mode()&0o100 == 0 {
		t.Errorf("heal.sh is not executable: %v, %v", info, err)
	}
}

// TestSandboxReadmeNamesTheHealAndWhatToLookAt: the README names the heal target and the certificate error it answers, and
// names the log lines and metrics of the turn, the Slack connection and the stream, each of which Agent Kourier does log or export
// (so the README cannot name one that is gone). What dogfood confirmed is marked, and what it did not is still marked UNCONFIRMED.
func TestSandboxReadmeNamesTheHealAndWhatToLookAt(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	for _, want := range []string{"make sandbox-heal", "CertificateExpired", "deploy/atenet-egress", "isn't ready to take this message yet"} {
		if !strings.Contains(readme, want) {
			t.Errorf("the README does not name %q", want)
		}
	}
	// Each, with the source file that makes it.
	for want, source := range map[string]string{
		"session turn started":                           "../../internal/session/turnlog.go",
		"session turn ended":                             "../../internal/session/turnlog.go",
		"agentkourier_turn_outcomes_total":               "../../internal/broker/metrics.go",
		"agentkourier_chat_connected":                    "../../internal/broker/metrics.go",
		"slack: socket mode connected":                   "../../internal/chat/slack/connwatch.go",
		"slack: socket mode reconnecting":                "../../internal/chat/slack/connwatch.go",
		"slack: the chunks mode of a stream was refused": "../../internal/chat/slack/platform.go",
	} {
		src, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not hold %q, which the README names", source, want)
		}
		if !strings.Contains(readme, want) {
			t.Errorf("the README does not name %q", want)
		}
	}
	// The outcomes the README lists are the session's.
	for _, outcome := range sessionOutcomes(t) {
		if !strings.Contains(readme, "`"+outcome+"`") {
			t.Errorf("the README's list of outcomes lacks %q", outcome)
		}
	}
	// Three things were confirmed (stopStream, chunk streams with cards, Bedrock) and the rest is still marked.
	if n := strings.Count(readme, "a live sandbox run"); n < 4 {
		t.Errorf("the README says a live sandbox run confirmed something %d times; want the stop blocks, the chunk streams and the Bedrock turn, at least", n)
	}
	for _, stale := range []string{"Nothing has run against Bedrock", "has not been run", "UNCONFIRMED, to check in the first real Bedrock turn"} {
		if strings.Contains(readme, stale) {
			t.Errorf("the README still says %q, which dogfood settled", stale)
		}
	}
	if !strings.Contains(readme, "UNCONFIRMED") || !strings.Contains(readme, "UNCONFIRMED, still") {
		t.Error("the README has lost the UNCONFIRMED items dogfood did not settle")
	}
}

// sessionOutcomes are the turn outcomes in internal/session/turnlog.go, as the strings it logs.
func sessionOutcomes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../../internal/session/turnlog.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range regexp.MustCompile(`TurnOutcome = "([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	if len(out) < 10 {
		t.Fatalf("found %d outcomes in turnlog.go; the pattern has stopped finding them", len(out))
	}
	return out
}
