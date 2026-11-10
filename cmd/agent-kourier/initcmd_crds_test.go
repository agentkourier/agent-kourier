package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// installPage is the how-to whose "Install on custom resources" section shows the three files that init writes by default.
const installPage = "../../docs/how-to/install-with-helm.md"

// The values of the page's custom-resource files.
const (
	pageTeamNamespace = "my-team"
	pageAgentURL      = "http://my-agent.my-team.svc.cluster.local:8083/a2a/"
)

// crArgs are the options that give init the values of the page's custom-resource files, and write the values to out.
func crArgs(out string, extra ...string) []string {
	return append([]string{
		"--out", out, "--dialect", "a2a", "--agent-url", pageAgentURL, "--agent-namespace", pageTeamNamespace,
		"--team-namespace", pageTeamNamespace, "--agent-name", "my-agent", "--channel", "C0123456789",
	}, extra...)
}

// crFiles are the three files of the custom-resource route for the values file out.
func crFiles(out string) (values, platform, team string) {
	dir := filepath.Dir(out)
	return out, filepath.Join(dir, "platform.yaml"), filepath.Join(dir, "team.yaml")
}

// pageSection returns the text of the page's section that starts with the heading, up to the next heading of its level.
func pageSection(t *testing.T, heading string) string {
	t.Helper()
	page, err := os.ReadFile(installPage)
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(page), "\n"+heading+"\n")
	if !ok {
		t.Fatalf("%s has no section %q", installPage, heading)
	}
	section, _, _ := strings.Cut(after, "\n"+heading[:strings.Index(heading, " ")+1])
	return section
}

// yamlBlock returns the YAML code block of section whose first line is opening, without its fences.
func yamlBlock(t *testing.T, section, opening string) string {
	t.Helper()
	_, after, ok := strings.Cut(section, "\n"+opening+"\n")
	if !ok {
		t.Fatalf("the section has no block that opens %q", opening)
	}
	block, _, ok := strings.Cut(after, "\n```\n")
	if !ok {
		t.Fatalf("the block %q is not closed", opening)
	}
	return block + "\n"
}

// yamlDocs reads each document of a YAML stream, so that two files with the same data and other comments compare equal.
func yamlDocs(t *testing.T, text string) []any {
	t.Helper()
	var docs []any
	for doc := range strings.SplitSeq("\n"+text, "\n---\n") {
		var v any
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("%v:\n%s", err, text)
		}
		if v != nil {
			docs = append(docs, v)
		}
	}
	return docs
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// loadCRFiles loads platform.yaml and team.yaml with the broker's loader and resolves the references between them, as
// the broker does with the custom resources, with every Secret present, and returns the Bindings it serves.
func loadCRFiles(t *testing.T, out string) []v1alpha1.Binding {
	t.Helper()
	_, platform, team := crFiles(out)
	dir := t.TempDir()
	for _, f := range []string{platform, team} {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f)), []byte(readFile(t, f)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set, err := config.Load(t.Context(), dir)
	if err != nil {
		t.Fatalf("the files do not load: %v", err)
	}
	snap, err := config.Resolve(t.Context(), set, config.PresentSecrets(set))
	if err != nil {
		t.Fatalf("the references do not resolve: %v", err)
	}
	var bindings []v1alpha1.Binding
	for _, rb := range snap.Bindings() {
		bindings = append(bindings, *rb.Binding)
	}
	return bindings
}

// By default init writes the page's three files, split by owner, with the page's values in them: the page and init
// cannot drift apart.
func TestInitWritesThePagesCustomResources(t *testing.T) {
	out := outFile(t)
	stdout, stderr, code := initRun(t, "", env{}.get, "", crArgs(out)...)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	section := pageSection(t, "## Install on custom resources")
	values, platform, team := crFiles(out)
	for _, f := range []struct{ path, opening string }{
		{values, "```yaml title=\"my-values.yaml\""},
		{platform, "```yaml title=\"platform.yaml\""},
		{team, "```yaml title=\"team.yaml\""},
	} {
		got := readFile(t, f.path)
		if want := yamlBlock(t, section, f.opening); !reflect.DeepEqual(yamlDocs(t, got), yamlDocs(t, want)) {
			t.Errorf("init's %s is not the page's:\ninit:\n%s\npage:\n%s", filepath.Base(f.path), got, want)
		}
		if !strings.Contains(got, "\n# ") && !strings.Contains(got, "\n  # ") {
			t.Errorf("%s has no comments:\n%s", filepath.Base(f.path), got)
		}
	}
	for _, want := range []string{
		"Wrote " + values + ", " + platform + " and " + team + ".",
		"kubectl create namespace agent-kourier\n",
		"kubectl -n agent-kourier create secret generic slack \\\n",
		"helm install agent-kourier " + chartReference + " --version 0.1.1 \\\n  -n agent-kourier -f " + values + "\n",
		"kubectl apply --dry-run=server -f " + platform + " -f " + team + "\n",
		"kubectl apply -f " + platform + "\nkubectl apply -f " + team + "\nkubectl get akbinding -A\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, agentTokenSecretName) {
		t.Errorf("stdout names the agent token Secret without --agent-token:\n%s", stdout)
	}
	if !strings.Contains(stderr, "checked: Agent Kourier loads platform.yaml and team.yaml") {
		t.Errorf("stderr %q does not say what was checked", stderr)
	}
	if snap := loadCRFiles(t, out); len(snap) != 1 {
		t.Errorf("resolved %d Bindings; want 1", len(snap))
	}
}

// With --agent-token, the Binding reads its token from a Secret in the team's namespace, the values grant that Secret as
// the page's rbac block does, and the commands create the Secret there.
func TestInitWithAnAgentTokenPutsTheSecretInTheTeamsNamespace(t *testing.T) {
	out := outFile(t)
	stdout, stderr, code := initRun(t, "", env{}.get, "", crArgs(out, "--agent-token")...)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	values, _, _ := crFiles(out)
	section := pageSection(t, "## Install on custom resources")
	var gotValues struct {
		Config map[string]any `json:"config"`
		RBAC   any            `json:"rbac"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, values)), &gotValues); err != nil {
		t.Fatal(err)
	}
	var wantRBAC struct {
		RBAC any `json:"rbac"`
	}
	if err := yaml.Unmarshal([]byte("rbac:\n"+yamlBlock(t, section, "```yaml\nrbac:")), &wantRBAC); err != nil {
		t.Fatal(err)
	}
	if wantRBAC.RBAC == nil || !reflect.DeepEqual(gotValues.RBAC, wantRBAC.RBAC) {
		t.Errorf("rbac = %v; want the page's %v", gotValues.RBAC, wantRBAC.RBAC)
	}
	if gotValues.Config["chatConnection"] != "agent-kourier/slack" {
		t.Errorf("config = %v", gotValues.Config)
	}
	b := loadCRFiles(t, out)[0]
	if ref := b.Spec.Identity.TokenSecretRef; b.Namespace != pageTeamNamespace || ref == nil || ref.Name != agentTokenSecretName || ref.Key != "token" {
		t.Errorf("Binding %s/%s tokenSecretRef = %+v; want %s, key token, in %s", b.Namespace, b.Name, ref, agentTokenSecretName, pageTeamNamespace)
	}
	if want := "kubectl -n my-team create secret generic " + agentTokenSecretName + " --from-literal=token=\"$AGENT_TOKEN\"\n"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
	if strings.Contains(stdout, "-n agent-kourier create secret generic "+agentTokenSecretName) {
		t.Errorf("stdout creates the agent token Secret in the release's namespace:\n%s", stdout)
	}
}

// Each dialect, with and without the token, in a release namespace and a team namespace of its own, makes files whose
// references resolve: the backend and the connection allow the team's namespace, and the Binding names them there.
func TestInitCustomResourcesResolve(t *testing.T) {
	for _, tc := range []struct {
		name, dialect, team string
		agentToken          bool
	}{
		{"a2a", "a2a", "payments", false},
		{"kagent-v1 with an agent token", "kagent-v1", "payments", true},
		{"the team in the release's namespace", "a2a", "kourier", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := outFile(t)
			args := []string{
				"--out", out, "--dialect", tc.dialect, "--agent-url", "http://kagent-frontdoor.kagent.svc.cluster.local:4180",
				"--agent-namespace", "agents", "--agent-name", "payments-agent", "--channel", "G0ABCDEF123", "--namespace", "kourier",
				"--team-namespace", tc.team,
			}
			if tc.agentToken {
				args = append(args, "--agent-token")
			}
			stdout, stderr, code := initRun(t, "", env{}.get, "", args...)
			if code != exitOK {
				t.Fatalf("exit code %d, stderr %q", code, stderr)
			}
			snap := loadCRFiles(t, out)
			if len(snap) != 1 {
				t.Fatalf("resolved %d Bindings; want 1", len(snap))
			}
			b := snap[0]
			if b.Namespace != tc.team || b.Spec.Agent.Namespace != "agents" || b.Spec.Agent.Name != "payments-agent" || b.Spec.Chat.Channel != "G0ABCDEF123" {
				t.Errorf("Binding %s/%s = %+v", b.Namespace, b.Name, b.Spec)
			}
			if b.Spec.Agent.BackendRef.Namespace != "kourier" || b.Spec.Chat.ConnectionRef.Namespace != "kourier" {
				t.Errorf("the Binding names %+v and %+v; want both in kourier", b.Spec.Agent.BackendRef, b.Spec.Chat.ConnectionRef)
			}
			if (b.Spec.Identity.TokenSecretRef != nil) != tc.agentToken {
				t.Errorf("tokenSecretRef = %+v; want one: %v", b.Spec.Identity.TokenSecretRef, tc.agentToken)
			}
			values := readFile(t, out)
			if !strings.Contains(values, "chatConnection: kourier/slack\n") {
				t.Errorf("the values do not name kourier/slack:\n%s", values)
			}
			if got := strings.Contains(values, tc.team+": ["+agentTokenSecretName+"]"); got != tc.agentToken {
				t.Errorf("the values grant the token Secret = %v; want %v:\n%s", got, tc.agentToken, values)
			}
			if !strings.Contains(stdout, "kubectl -n kourier create secret generic slack") || !strings.Contains(stdout, "-n kourier -f "+out) {
				t.Errorf("stdout does not name the namespace kourier:\n%s", stdout)
			}
		})
	}
}

// The team's namespace is asked after the agent's, with the agent's as its default, and only on custom resources.
func TestInitAsksForTheTeamsNamespace(t *testing.T) {
	out := outFile(t)
	// the dialect takes its default, the team's namespace the agent's
	stdin := "\nhttp://agent.agents.svc:8083/a2a/\nagents\n\nsre-agent\nC0123456789\nn\n"
	_, stderr, code := initRun(t, "", env{}.get, stdin, "--out", out)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "Agent namespace: Team namespace, where the Binding goes [agents]: ") {
		t.Errorf("stderr does not ask the team's namespace after the agent's, with the agent's as the default:\n%s", stderr)
	}
	if b := loadCRFiles(t, out)[0]; b.Namespace != "agents" || b.Spec.Agent.Name != "sre-agent" {
		t.Errorf("Binding %s/%s = %+v; want it in agents", b.Namespace, b.Name, b.Spec)
	}

	out = outFile(t)
	_, stderr, code = initRun(t, "", env{}.get, "payments\n", crArgs(out, "--team-namespace", "")...)
	if code != exitOK || strings.Contains(stderr, "Agent URL") {
		t.Fatalf("exit code %d, stderr %q; want only the team's namespace asked", code, stderr)
	}
	if b := loadCRFiles(t, out)[0]; b.Namespace != "payments" {
		t.Errorf("the Binding is in %s; want payments", b.Namespace)
	}

	// The files route has no team: it neither asks nor takes the option.
	_, stderr, code = initRun(t, "", env{}.get, "", starterArgs(outFile(t))...)
	if code != exitOK || strings.Contains(stderr, "Team namespace") {
		t.Errorf("exit code %d, stderr %q; want no question about the team", code, stderr)
	}
	if _, stderr, code := initRun(t, "", env{}.get, "", starterArgs(outFile(t), "--team-namespace", "payments")...); code != exitUsage || !strings.Contains(stderr, "--team-namespace") {
		t.Errorf("exit code %d, stderr %q; want %d and --team-namespace named", code, stderr, exitUsage)
	}
}

// Wrong values of the route's own options are usage errors, and nothing is written.
func TestInitRefusesWrongRouteValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"a source that does not exist", []string{"--source", "configmap"}, `--source "configmap" is not crds or files`},
		{"a team namespace that is no name", []string{"--team-namespace", "My Team"}, "--team-namespace"},
		{"a values file named like platform.yaml", []string{"--out", "platform.yaml"}, "--out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := outFile(t)
			args := append(crArgs(out), tc.extra...)
			if tc.extra[0] == "--out" {
				args = crArgs(filepath.Join(filepath.Dir(out), tc.extra[1]))
			}
			stdout, stderr, code := initRun(t, "", env{}.get, "", args...)
			if code != exitUsage || !strings.Contains(stderr, tc.want) || stdout != "" {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d and %q", code, stdout, stderr, exitUsage, tc.want)
			}
			for _, f := range []string{out, filepath.Join(filepath.Dir(out), "platform.yaml"), filepath.Join(filepath.Dir(out), "team.yaml")} {
				if _, err := os.Lstat(f); err == nil {
					t.Errorf("%s was written", f)
				}
			}
		})
	}
}

// Any of the three files that exists stops init before its questions, and nothing is written, unless --force says so.
func TestInitDoesNotOverwriteAnyOfTheThreeFiles(t *testing.T) {
	for i, name := range []string{"values", "platform", "team"} {
		t.Run(name, func(t *testing.T) {
			out := outFile(t)
			values, platform, team := crFiles(out)
			paths := []string{values, platform, team}
			if err := os.WriteFile(paths[i], []byte("mine\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := initRun(t, "", env{}.get, "", "--out", out)
			if code != exitUsage || !strings.Contains(stderr, paths[i]+" exists") || !strings.Contains(stderr, "--force") || stdout != "" || strings.Contains(stderr, "Agent URL") {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d, %s and --force named, and no question", code, stdout, stderr, exitUsage, paths[i])
			}
			for j, p := range paths {
				_, err := os.Lstat(p)
				if j != i && err == nil {
					t.Errorf("%s was written", p)
				}
			}
			if got := readFile(t, paths[i]); got != "mine\n" {
				t.Errorf("%s is now %q", paths[i], got)
			}

			if _, stderr, code := initRun(t, "", env{}.get, "", crArgs(out, "--force")...); code != exitOK {
				t.Fatalf("--force: exit code %d, stderr %q", code, stderr)
			}
			if got := readFile(t, paths[i]); got == "mine\n" {
				t.Errorf("--force left %s as it was", paths[i])
			}
			loadCRFiles(t, out)
		})
	}
}

// When a file cannot be written, the files that init made before it are taken away again, and an existing file is kept.
func TestWriteInitFilesIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	first, second, third := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml"), filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(second, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeInitFiles([]initFile{{first, []byte("a\n")}, {second, []byte("b\n")}, {third, []byte("c\n")}}, false)
	if err == nil || !strings.Contains(err.Error(), second+" exists") {
		t.Fatalf("err = %v; want %s exists", err, second)
	}
	for _, p := range []string{first, third} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is left: %v", p, err)
		}
	}
	if got := readFile(t, second); got != "mine\n" {
		t.Errorf("%s is now %q", second, got)
	}
}

// What Slack refuses writes none of the three files, and a channel name is looked up into the team's Binding.
func TestInitOnCustomResourcesChecksSlack(t *testing.T) {
	out := outFile(t)
	slackSrv := &initSlack{historyErr: "not_in_channel"}
	_, stderr, code := initRun(t, slackSrv.serve(t), env{"SLACK_BOT_TOKEN": canaryToken}.get, "", crArgs(out)...)
	if code != exitFailure || !strings.Contains(stderr, "invite the bot") {
		t.Errorf("exit code %d, stderr %q; want %d and invite the bot", code, stderr, exitFailure)
	}
	values, platform, team := crFiles(out)
	for _, p := range []string{values, platform, team} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}

	slackSrv = &initSlack{channels: []map[string]string{{"id": "C0BBBBBBBBB", "name": "sre-alerts"}}}
	_, stderr, code = initRun(t, slackSrv.serve(t), env{"SLACK_BOT_TOKEN": canaryToken}.get, "", crArgs(out, "--channel", "#sre-alerts")...)
	if code != exitOK || !strings.Contains(stderr, "the bot can read C0BBBBBBBBB") {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if got := loadCRFiles(t, out)[0].Spec.Chat.Channel; got != "C0BBBBBBBBB" {
		t.Errorf("the Binding has channel %q; want C0BBBBBBBBB", got)
	}
}

// The commands for custom resources quote the paths, put the token Secret in the team's namespace, and apply the files
// only after the chart, which brings the CRDs.
func TestNextStepsOnCustomResources(t *testing.T) {
	got := nextSteps(initOptions{source: sourceCRDs, out: "my dir/v.yaml", namespace: "team-a", teamNamespace: "payments", agentToken: true}, "v0.1.1")
	want := `Next, with SLACK_BOT_TOKEN and SLACK_APP_TOKEN set (see https://agentkourier.dev/how-to/configure-slack/):

kubectl create namespace team-a
kubectl -n team-a create secret generic slack \
  --from-literal=botToken="$SLACK_BOT_TOKEN" \
  --from-literal=appToken="$SLACK_APP_TOKEN"

# AGENT_TOKEN is the bearer token the agent accepts for the Binding's identity; the Secret goes in the Binding's namespace
kubectl -n payments create secret generic agent-kourier-agent-token --from-literal=token="$AGENT_TOKEN"

helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.1.1 \
  -n team-a -f 'my dir/v.yaml'

# Once the chart is installed (it brings the CRDs): the platform team's file, then the team's
kubectl apply --dry-run=server -f 'my dir/platform.yaml' -f 'my dir/team.yaml'
kubectl apply -f 'my dir/platform.yaml'
kubectl apply -f 'my dir/team.yaml'
kubectl get akbinding -A
`
	if got != want {
		t.Errorf("nextSteps =\n%s\nwant\n%s", got, want)
	}
}
