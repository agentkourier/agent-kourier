package kindmanifests_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// toolStub is a kubectl or helm that appends its name and arguments to one log, so that the order of the calls is kept, and answers the reads
// deploy_agentkourier makes: `get crd ... -o jsonpath=` prints STUB_OWNER (the Helm release that holds the CRDs, "" for none) and `get ... -o name` prints nothing.
const toolStub = `#!/bin/sh
echo "$(basename "$0") $*" >> "$STUB_LOG/calls"
case "$*" in
  *"get "*"-o jsonpath="*) printf '%s' "$STUB_OWNER" ;;
esac
`

// deploy runs lib.sh's deploy_agentkourier against stubs with E2E_CONFIG_SOURCE source, as the fast tier calls it, and returns the calls one to a line
// and the error, if any, with the script's output.
func deploy(t *testing.T, source, crdOwner string) (calls []string, out string, err error) {
	t.Helper()
	bin, logs := t.TempDir(), t.TempDir()
	for _, tool := range []string{"kubectl", "helm"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(toolStub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hack, err := filepath.Abs("../../hack/kind")
	if err != nil {
		t.Fatal(err)
	}
	script := `set -eu; . "$1"; HERE="$2"; ROOT=/repo; K=(kubectl --context c); H=(helm --kube-context c); deploy_agentkourier "$2/fast/agent-kourier/config/agentbackend.yaml"`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", filepath.Join(hack, "lib.sh"), hack)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+logs, "STUB_OWNER="+crdOwner, "E2E_CONFIG_SOURCE="+source)
	raw, err := cmd.CombinedOutput()
	for _, l := range strings.Split(strings.TrimSpace(readLog(t, logs, "calls")), "\n") {
		if l != "" {
			calls = append(calls, l)
		}
	}
	return calls, string(raw), err
}

// index returns the position of the first call that contains every part, or -1.
func index(calls []string, parts ...string) int {
	for i, c := range calls {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(c, p)
		}
		if ok {
			return i
		}
	}
	return -1
}

// TestTheTiersGoToCustomResourcesTheWayAUserDoes: with E2E_CONFIG_SOURCE=crds the tier follows the move the chart README gives, in its order: a Helm
// upgrade that installs the CRDs with the route still files; the CRDs served; the tier's config applied as custom resources, after a dry run by the API
// server; then a Helm upgrade with the overlay that sets config.source. A release that holds the CRDs already skips the first upgrade, and the
// default, files, never touches the CRDs.
func TestTheTiersGoToCustomResourcesTheWayAUserDoes(t *testing.T) {
	const crdsOn = "--set crds.install=true"
	const overlay = "-f " // the overlay path is the script's own directory, checked below
	calls, out, err := deploy(t, "crds", "")
	if err != nil {
		t.Fatalf("deploy_agentkourier = %v\n%s", err, out)
	}
	first := index(calls, "helm ", "upgrade --install agent-kourier", crdsOn)
	wait := index(calls, "kubectl ", "wait --for=condition=Established", "crd/bindings.agentkourier.dev", "crd/agentbackends.agentkourier.dev", "crd/chatconnections.agentkourier.dev")
	dry := index(calls, "kubectl ", "apply --dry-run=server -f /")
	apply := index(calls, "kubectl ", "apply -f /")
	second := index(calls, "helm ", "upgrade --install agent-kourier", "hack/kind/crds-values.yaml")
	if first < 0 || wait <= first || dry <= wait || apply <= dry || second <= apply {
		t.Fatalf("calls in order: CRDs upgrade %d, established %d, dry run %d, apply %d, route upgrade %d; want each after the one before:\n%s", first, wait, dry, apply, second, strings.Join(calls, "\n"))
	}
	if strings.Contains(calls[first], "crds-values.yaml") {
		t.Errorf("the first upgrade is %q: it must leave the route files, so that the CRDs are served before anything renders a custom resource", calls[first])
	}
	if strings.Contains(calls[second], crdsOn) || !strings.Contains(calls[second], overlay) {
		t.Errorf("the route upgrade is %q: want the overlay, which holds crds.install itself", calls[second])
	}
	if restart := index(calls, "rollout restart deploy/agent-kourier"); restart <= second {
		t.Errorf("rollout restart at %d, after the route upgrade at %d wanted: a rebuilt image is picked up", restart, second)
	}

	// A tier that is already on custom resources, brought up again: the release holds the CRDs, so there is no first upgrade, which would
	// put the pod back on the ConfigMap for the length of the step.
	calls, out, err = deploy(t, "crds", "agent-kourier")
	if err != nil {
		t.Fatalf("deploy_agentkourier on a release that holds the CRDs = %v\n%s", err, out)
	}
	if n := len(helmCalls(calls)); n != 1 || index(calls, "helm ", "hack/kind/crds-values.yaml") < 0 {
		t.Errorf("helm was run %d times, want once, with the overlay:\n%s", n, strings.Join(calls, "\n"))
	}

	// The ConfigMap route, the default: one upgrade, nothing about the CRDs, no custom resource applied.
	for _, source := range []string{"", "files"} {
		calls, out, err = deploy(t, source, "")
		if err != nil {
			t.Fatalf("deploy_agentkourier with source %q = %v\n%s", source, err, out)
		}
		if n := len(helmCalls(calls)); n != 1 {
			t.Fatalf("source %q: helm was run %d times, want once:\n%s", source, n, strings.Join(calls, "\n"))
		}
		if h := helmCalls(calls)[0]; strings.Contains(h, "crds") {
			t.Errorf("source %q: the upgrade is %q: the ConfigMap route renders what it always has", source, h)
		}
		if index(calls, "kubectl ", "apply --dry-run=server") >= 0 {
			t.Errorf("source %q applied custom resources:\n%s", source, strings.Join(calls, "\n"))
		}
	}

	// Back from custom resources: the CRDs stay under the release, as the README says to keep them, and nothing is applied.
	calls, out, err = deploy(t, "files", "agent-kourier")
	if err != nil {
		t.Fatalf("deploy_agentkourier going back = %v\n%s", err, out)
	}
	if h := helmCalls(calls); len(h) != 1 || !strings.Contains(h[0], crdsOn) || strings.Contains(h[0], "crds-values.yaml") {
		t.Errorf("going back to files the upgrades are %v, want one, with crds.install=true and no overlay", h)
	}
}

func helmCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "helm ") {
			out = append(out, c)
		}
	}
	return out
}

// TestTheTiersRefuseASourceTheyDoNotKnow: the value is checked before anything is changed, by the script that is run and by the helper.
func TestTheTiersRefuseASourceTheyDoNotKnow(t *testing.T) {
	calls, out, err := deploy(t, "configmap", "")
	if err == nil || !strings.Contains(out, "E2E_CONFIG_SOURCE must be files or crds") || len(calls) != 0 {
		t.Errorf("deploy_agentkourier with an unknown source = %v, %d calls\n%s\nwant it to refuse before it touches the cluster", err, len(calls), out)
	}
	for _, script := range []string{"up-fast.sh", "up-full.sh"} {
		cmd := exec.CommandContext(t.Context(), "bash", "../../hack/kind/"+script)
		cmd.Env = append(os.Environ(), "E2E_CONFIG_SOURCE=configmap")
		raw, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(raw), "E2E_CONFIG_SOURCE must be files or crds") {
			t.Errorf("%s with E2E_CONFIG_SOURCE=configmap = %v\n%s\nwant it to refuse the value", script, err, raw)
		}
	}
}

// TestTheCustomResourceOverlayRoutesTheTiersConfig: the overlay turns the route on, says which connection the broker serves (the tier's, in its
// namespace), keeps the CRDs under the release and grants each Secret of the tier by name, with no namespace as a whole, so that what is read is authorized by
// resourceNames alone. The connection it names is the one in the tier's files, and every Secret the tier's config refers to is granted.
func TestTheCustomResourceOverlayRoutesTheTiersConfig(t *testing.T) {
	raw, err := os.ReadFile("../../hack/kind/crds-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"source: crds", `existingConfigMap: ""`, "chatConnection: agent-kourier/slack-fast", "install: true", "secretNamespaces: []", "agent-kourier: [slack-fast, agent-kourier-agent-token]"} {
		if !strings.Contains(text, want) {
			t.Errorf("crds-values.yaml lacks %q", want)
		}
	}
	// Every Secret a Binding or the connection of the tier reads is in the grant, in the tier's namespace.
	set, err := loadTierConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	granted := regexp.MustCompile(`agent-kourier: \[([^\]]*)\]`).FindStringSubmatch(text)
	if granted == nil {
		t.Fatal("no grant for namespace agent-kourier")
	}
	for _, name := range set {
		if !strings.Contains(granted[1], name) {
			t.Errorf("the tier's config reads the Secret %s, which crds-values.yaml does not grant", name)
		}
	}
}

// loadTierConfig returns the names of the Secrets that the fast tier's config reads, from the files that deploy_agentkourier applies.
func loadTierConfig(t *testing.T) ([]string, error) {
	t.Helper()
	files, err := filepath.Glob("../../hack/kind/fast/agent-kourier/*/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no config files: %v", err)
	}
	seen := map[string]bool{}
	ref := regexp.MustCompile(`(?:credentialsSecretRef|tokenSecretRef|valueSecretRef): \{name: ([a-z0-9-]+)`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range ref.FindAllStringSubmatch(string(raw), -1) {
			seen[m[1]] = true
		}
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	return out, nil
}
