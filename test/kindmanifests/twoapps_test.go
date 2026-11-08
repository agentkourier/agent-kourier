package kindmanifests_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/agentkourier/agent-kourier/test/e2e"
)

// The fast tier's second Slack app (E2E_TWO_APPS=1; hack/kind/fast/two-apps and test/e2e's MC_ tests): the fake Slack hosts the apps the tests name, the manifests
// bind the channels the tests use to the connections they expect, and the tier's scripts agree with the Roles of the runner on which namespaces and Secrets.

const (
	twoAppsFile  = "../../hack/kind/fast/two-apps/two-apps.yaml"
	twoAppsLib   = "../../hack/kind/lib.sh"
	runnerFile   = "../../hack/kind/e2e/runner.yaml"
	doublesFile  = "../../hack/kind/fast/doubles.yaml"
	twoAppsExtra = "../../hack/kind/two-apps-values.yaml"
)

// fakeSlackApps returns the -app values of the fake Slack's command in the doubles, each as its key=value pairs.
func fakeSlackApps(t *testing.T) []map[string]string {
	t.Helper()
	var d appsv1.Deployment
	for _, doc := range objects(t, doublesFile) {
		if doc["kind"] == "Deployment" && fmt.Sprint(doc["metadata"].(map[string]any)["name"]) == "fakeslack" {
			load(t, doublesFile, "Deployment", &d) // the first Deployment is the fake Slack's: checked below
			break
		}
	}
	if d.Name != "fakeslack" {
		t.Fatalf("the first Deployment of %s is %q; want fakeslack", doublesFile, d.Name)
	}
	cmd := d.Spec.Template.Spec.Containers[0].Command
	var apps []map[string]string
	for i, a := range cmd {
		if a != "-app" || i+1 == len(cmd) {
			continue
		}
		pairs := map[string]string{}
		for _, kv := range strings.Split(cmd[i+1], ",") {
			k, v, _ := strings.Cut(kv, "=")
			pairs[k] = v
		}
		apps = append(apps, pairs)
	}
	return apps
}

// TestTheFakeSlackHostsTheAppsTheScenariosName: team-a's app and team-b's, with the bot, bot user, app ID and the fake tokens that test/e2e's constants give them,
// and the default app unchanged: no -app takes the default's IDs, and no flag makes tokens strict, so every scenario of the one-app tier is as it was.
func TestTheFakeSlackHostsTheAppsTheScenariosName(t *testing.T) {
	apps := fakeSlackApps(t)
	for _, want := range []e2e.FakeApp{e2e.AppTeamA, e2e.AppTeamB} {
		i := slices.IndexFunc(apps, func(a map[string]string) bool { return a["app_id"] == want.AppID })
		if i < 0 {
			t.Errorf("the fake Slack hosts no app %s; its -app values are %v", want.AppID, apps)
			continue
		}
		got := apps[i]
		if got["bot_user"] != want.BotUser || got["bot_id"] != want.BotID || got["bot_token"] != want.BotToken || got["app_token"] != want.AppToken {
			t.Errorf("the fake Slack hosts %s as bot user %s, bot %s with other tokens than test/e2e's constants (%s, %s)", want.AppID, got["bot_user"], got["bot_id"], want.BotUser, want.BotID)
		}
	}
	for _, a := range apps {
		if a["app_id"] == e2e.AppPlatform.AppID || a["bot_user"] == e2e.AppPlatform.BotUser || a["bot_id"] == e2e.AppPlatform.BotID {
			t.Errorf("an -app takes the default app's identity: %v", a)
		}
	}
	var d appsv1.Deployment
	load(t, doublesFile, "Deployment", &d)
	for _, arg := range d.Spec.Template.Spec.Containers[0].Command {
		if strings.HasPrefix(arg, "-strict") || arg == "-socket-spread" {
			t.Errorf("the fake Slack runs with %s: the scenarios of one app and the platform's connection rely on the default app owning every unowned token and every channel", arg)
		}
	}
}

// TestTheTwoAppsManifestsBindTheChannelsOfTheTests: team-a's connection reads the Secret that deploy_two_apps makes, with the tokens of team-a's app; team-a's Role
// names that Secret and the agent token and is bound to the broker's ServiceAccount; and each Binding is on the channel and connection the tests expect, in the
// namespace of the connection it names (team-a's own, the platform's slack-fast).
func TestTheTwoAppsManifestsBindTheChannelsOfTheTests(t *testing.T) {
	bindings := map[string]map[string]any{}
	var role rbacv1.Role
	var rb rbacv1.RoleBinding
	for _, doc := range objects(t, twoAppsFile) {
		meta := doc["metadata"].(map[string]any)
		key := fmt.Sprintf("%s/%s", meta["namespace"], meta["name"])
		switch doc["kind"] {
		case "Binding":
			bindings[key] = doc["spec"].(map[string]any)
		case "ChatConnection":
			if key != e2e.ConnTeamA || fmt.Sprint(doc["spec"].(map[string]any)["credentialsSecretRef"]) != "map[name:slack-team-a]" {
				t.Errorf("the ChatConnection %s reads %v; want team-a/team-a to read the Secret slack-team-a", key, doc["spec"])
			}
		}
	}
	load(t, twoAppsFile, "Role", &role)
	load(t, twoAppsFile, "RoleBinding", &rb)
	chat := func(binding string) (channel, connection string) {
		spec, ok := bindings[binding]
		if !ok {
			t.Fatalf("%s has no Binding %s; it has %v", twoAppsFile, binding, bindings)
		}
		c := spec["chat"].(map[string]any)
		ref := c["connectionRef"].(map[string]any)
		ns, _ := ref["namespace"].(string)
		if ns == "" {
			ns = strings.SplitN(binding, "/", 2)[0]
		}
		return fmt.Sprint(c["channel"]), ns + "/" + fmt.Sprint(ref["name"])
	}
	if ch, conn := chat("team-a/team-a"); ch != e2e.ChannelMCTeamA || conn != e2e.ConnTeamA {
		t.Errorf("Binding team-a/team-a is on channel %s and connection %s; want %s and %s", ch, conn, e2e.ChannelMCTeamA, e2e.ConnTeamA)
	}
	if ch, conn := chat("agent-kourier/mc-platform"); ch != e2e.ChannelMCPlatform || conn != e2e.ConnPlatform {
		t.Errorf("Binding agent-kourier/mc-platform is on channel %s and connection %s; want %s and %s", ch, conn, e2e.ChannelMCPlatform, e2e.ConnPlatform)
	}
	if ch, conn := chat("team-a/team-a-final"); ch != e2e.ChannelMCTeamAFinal || conn != e2e.ConnTeamA {
		t.Errorf("Binding team-a/team-a-final is on channel %s and connection %s; want %s and %s", ch, conn, e2e.ChannelMCTeamAFinal, e2e.ConnTeamA)
	}
	if ch, conn := chat("agent-kourier/mc-platform-final"); ch != e2e.ChannelMCPlatformFinal || conn != e2e.ConnPlatform {
		t.Errorf("Binding agent-kourier/mc-platform-final is on channel %s and connection %s; want %s and %s", ch, conn, e2e.ChannelMCPlatformFinal, e2e.ConnPlatform)
	}
	for binding, conn := range map[string]string{"team-a/team-a-loop": e2e.ConnTeamA, "agent-kourier/mc-platform-loop": e2e.ConnPlatform} {
		if ch, got := chat(binding); ch != e2e.ChannelMCLoop || got != conn {
			t.Errorf("Binding %s is on channel %s and connection %s; want %s and %s: the two Bindings of the bot-loop case share a channel", binding, ch, got, e2e.ChannelMCLoop, conn)
		}
	}
	for name, want := range map[string]string{"team-a/team-a": "live", "agent-kourier/mc-platform": "live", "team-a/team-a-final": "final", "agent-kourier/mc-platform-final": "final"} {
		if got := bindings[name]["chat"].(map[string]any)["output"]; got != want {
			t.Errorf("Binding %s has output %v; the tests expect %s", name, got, want)
		}
	}
	for name, spec := range bindings {
		if agent := spec["agent"].(map[string]any)["name"]; agent != "scenario-gated" {
			t.Errorf("Binding %s is answered by %v; the tests expect the stub's scenario-gated, which answers any text and holds on a gate the text names", name, agent)
		}
	}
	if role.Namespace != "team-a" || len(role.Rules) != 1 || !slices.Equal(role.Rules[0].ResourceNames, []string{"slack-team-a", "team-a-agent-token"}) ||
		!slices.Equal(role.Rules[0].Verbs, []string{"list", "watch"}) {
		t.Errorf("team-a's Role = %s %+v; want list and watch of the Secrets slack-team-a and team-a-agent-token by name", role.Namespace, role.Rules)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != "agent-kourier" || rb.Subjects[0].Namespace != "agent-kourier" || rb.RoleRef.Name != role.Name {
		t.Errorf("team-a's RoleBinding = %+v; want its Role bound to the ServiceAccount agent-kourier/agent-kourier", rb)
	}
	lib, err := os.ReadFile(twoAppsLib)
	if err != nil {
		t.Fatal(err)
	}
	// The bots are in the channels of the Bindings before the Bindings are made (seat_two_apps_bots), whose names are test/e2e's.
	for _, ch := range []string{e2e.ChannelMCPlatform, e2e.ChannelMCPlatformFinal, e2e.ChannelMCLoop, e2e.ChannelMCTeamA, e2e.ChannelMCTeamAFinal} {
		if !strings.Contains(string(lib), `\"channel\":\"`+ch+`\"`) {
			t.Errorf("lib.sh's seat_two_apps_bots does not put a bot in the channel %s", ch)
		}
	}
	for _, want := range []string{"create secret generic slack-team-a --from-literal=botToken=" + e2e.AppTeamA.BotToken, "--from-literal=appToken=" + e2e.AppTeamA.AppToken, "create secret generic team-a-agent-token"} {
		if !strings.Contains(string(lib), want) {
			t.Errorf("lib.sh's deploy_two_apps does not make %q", want)
		}
	}
}

// TestTheAdmissionListIsTheNamespacesTheRunnerPlaysIn: the list the tier is given names the three namespaces the MC_ tests use, the runner has a Role in each
// of them (and e2e-run.sh makes them, so that the Roles apply), and the Job learns the list from the Deployment through a variable the tests read.
func TestTheAdmissionListIsTheNamespacesTheRunnerPlaysIn(t *testing.T) {
	raw, err := os.ReadFile(twoAppsExtra)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "connectionNamespaces: [team-a, team-b, team-c]") {
		t.Errorf("%s does not list team-a, team-b and team-c:\n%s", twoAppsExtra, raw)
	}
	roles := map[string]bool{}
	for _, doc := range objects(t, runnerFile) {
		if doc["kind"] == "Role" {
			meta := doc["metadata"].(map[string]any)
			roles[fmt.Sprint(meta["namespace"])+"/"+fmt.Sprint(meta["name"])] = true
		}
	}
	run, err := os.ReadFile("../../hack/kind/e2e-run.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"team-a", "team-b", "team-c"} {
		if !roles[ns+"/e2e-runner-"+ns] {
			t.Errorf("the runner has no Role e2e-runner-%s in %s", ns, ns)
		}
		if !strings.Contains(string(run), "for ns in team-a team-b team-c") {
			t.Errorf("e2e-run.sh does not make the namespace %s that the runner's Role is in", ns)
		}
	}
	var job batchv1.Job
	load(t, runnerFile, "Job", &job)
	var found bool
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "E2E_CONNECTION_NAMESPACES" {
			found = e.Value == "@CONNECTION_NAMESPACES@"
		}
	}
	if !found || !strings.Contains(string(run), `-e "s/@CONNECTION_NAMESPACES@/${connection_namespaces}/"`) || !strings.Contains(string(run), "AGENTKOURIER_CONNECTION_NAMESPACES") {
		t.Errorf("the Job's E2E_CONNECTION_NAMESPACES is not the Deployment's AGENTKOURIER_CONNECTION_NAMESPACES, which e2e-run.sh reads and puts in with @CONNECTION_NAMESPACES@")
	}
}

// TestUpFastRefusesTwoAppsItCannotRun: a team's ChatConnection is a custom resource, so the second app needs the custom resources route, and the value is 0 or 1; the
// script says so before it calls any tool.
func TestUpFastRefusesTwoAppsItCannotRun(t *testing.T) {
	bin, logs := t.TempDir(), t.TempDir()
	for _, tool := range []string{"kind", "docker", "kubectl", "helm", "go", "curl", "openssl", "jq"} {
		stub := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> \"$STUB_LOG/called\"\nexit 99\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"the ConfigMap route":      {[]string{"E2E_TWO_APPS=1", "E2E_CONFIG_SOURCE=files"}, "E2E_TWO_APPS=1 needs E2E_CONFIG_SOURCE=crds"},
		"the default route":        {[]string{"E2E_TWO_APPS=1"}, "E2E_TWO_APPS=1 needs E2E_CONFIG_SOURCE=crds"},
		"a value it does not know": {[]string{"E2E_TWO_APPS=2", "E2E_CONFIG_SOURCE=crds"}, "E2E_TWO_APPS must be 0 or 1"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "../../hack/kind/up-fast.sh")
			cmd.Env = append(os.Environ(), append(tc.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+logs)...)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Errorf("up-fast.sh with %v = %v\n%s\nwant it to refuse: %s", tc.env, err, out, tc.want)
			}
			if called := readLog(t, logs, "called"); called != "" {
				t.Errorf("up-fast.sh called %q before it refused; want no tool called", called)
			}
		})
	}
}

// TestTheAdmissionListGoesInTheRouteUpgradeOnly: deploy_agentkourier gives the arguments the caller set in AGENTKOURIER_CRDS_ARGS to the upgrade that moves the pod to
// the custom resources, after the overlay, and not to the first upgrade (the CRDs, the route still the ConfigMap's, which the chart refuses a list for).
func TestTheAdmissionListGoesInTheRouteUpgradeOnly(t *testing.T) {
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
	script := `set -eu; . "$1"; HERE="$2"; ROOT=/repo; K=(kubectl --context c); H=(helm --kube-context c); AGENTKOURIER_CRDS_ARGS=(-f /v/two-apps.yaml); deploy_agentkourier "$2/fast/agent-kourier/config/agentbackend.yaml"`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", filepath.Join(hack, "lib.sh"), hack)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+logs, "STUB_OWNER=", "E2E_CONFIG_SOURCE=crds")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy_agentkourier = %v\n%s", err, out)
	}
	var upgrades []string
	for _, l := range strings.Split(readLog(t, logs, "calls"), "\n") {
		if strings.HasPrefix(l, "helm ") && strings.Contains(l, " upgrade --install ") {
			upgrades = append(upgrades, l)
		}
	}
	if len(upgrades) != 2 {
		t.Fatalf("helm upgrades = %v; want the CRDs, then the route", upgrades)
	}
	if strings.Contains(upgrades[0], "two-apps.yaml") {
		t.Errorf("the CRDs upgrade %q carries the admission list, which the chart refuses on the ConfigMap route", upgrades[0])
	}
	if !strings.Contains(upgrades[1], "/hack/kind/crds-values.yaml -f /v/two-apps.yaml") {
		t.Errorf("the route upgrade %q does not take the admission list after the overlay", upgrades[1])
	}
}
