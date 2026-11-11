package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// teamAppPage is the how-to whose recorded path ("The whole path, recorded") shows the files and the commands that init
// writes and prints by default: a broker with no Slack app of its own, and a team that brings its own.
const teamAppPage = "../../docs/how-to/team-slack-app.md"

// The values of the page's recorded path.
const (
	pageTeam     = "team-a"
	pageAgentURL = "http://my-agent.team-a.svc.cluster.local:8083/a2a/"
)

// crArgs are the options that give init the values of the page's recorded path, and write the values to out.
func crArgs(out string, extra ...string) []string {
	return append([]string{
		"--out", out, "--dialect", "a2a", "--agent-url", pageAgentURL, "--agent-namespace", pageTeam,
		"--team-namespace", pageTeam, "--agent-name", "my-agent", "--channel", "C0123456789",
	}, extra...)
}

// teamFile is the team's file that init writes beside the values file out, for the team's namespace team.
func teamFile(out, team string) string { return filepath.Join(filepath.Dir(out), team+".yaml") }

// pageSection returns the text of the page's section that starts with the heading, up to the next heading of any level, so
// without its subsections. A line in a code fence (a YAML or shell comment) is never a heading.
func pageSection(t *testing.T, page, heading string) string {
	t.Helper()
	text, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(text), "\n"+heading+"\n")
	if !ok {
		t.Fatalf("%s has no section %q", page, heading)
	}
	var section strings.Builder
	fence := false
	for line := range strings.Lines(after) {
		if strings.HasPrefix(line, "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(strings.TrimLeft(line, "#"), " ") && strings.HasPrefix(line, "#") {
			break
		}
		section.WriteString(line)
	}
	return section.String()
}

// codeBlocks returns the code blocks of section whose opening fence is opening, without their fences.
func codeBlocks(t *testing.T, section, opening string) []string {
	t.Helper()
	var blocks []string
	rest := section
	for {
		_, after, ok := strings.Cut(rest, "\n"+opening+"\n")
		if !ok {
			break
		}
		block, tail, ok := strings.Cut(after, "\n```\n")
		if !ok {
			t.Fatalf("a block %q is not closed", opening)
		}
		blocks = append(blocks, block+"\n")
		rest = "\n" + tail
	}
	if len(blocks) == 0 {
		t.Fatalf("the section has no block that opens %q", opening)
	}
	return blocks
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

// loadTeamFile loads the team's file with the broker's loader and resolves the references between its objects, as the
// broker does with the custom resources, with every Secret present, and returns the Bindings it serves.
func loadTeamFile(t *testing.T, path string) []v1alpha1.Binding {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(readFile(t, path)), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := config.Load(t.Context(), dir)
	if err != nil {
		t.Fatalf("%s does not load: %v", path, err)
	}
	snap, err := config.Resolve(t.Context(), set, config.PresentSecrets(set))
	if err != nil {
		t.Fatalf("the references of %s do not resolve: %v", path, err)
	}
	var bindings []v1alpha1.Binding
	for _, rb := range snap.Bindings() {
		bindings = append(bindings, *rb.Binding)
	}
	return bindings
}

// chartVersion is the chart's version, which the pages name in their helm commands.
func chartVersion(t *testing.T) string {
	t.Helper()
	for line := range strings.SplitSeq(readFile(t, "../../charts/agent-kourier/Chart.yaml"), "\n") {
		if v, ok := strings.CutPrefix(line, "version:"); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("the chart has no version")
	return ""
}

// By default init writes the page's two files, with the page's values in them, and prints the page's commands: the page
// and init cannot drift apart. It runs in a directory of its own so that the paths it prints are the page's.
func TestInitWritesThePagesTeamApp(t *testing.T) {
	for _, page := range []struct{ path, heading string }{
		{teamAppPage, "### The whole path, recorded"},
		// Install with Helm leads its custom resource install with the same path; its subsection, a broker with an app of its own, is not init's.
		{"../../docs/how-to/install-with-helm.md", "## Install on custom resources"},
	} {
		t.Run(filepath.Base(page.path), func(t *testing.T) {
			section := pageSection(t, page.path, page.heading)
			if strings.Contains(section, "platform.yaml") {
				t.Fatalf("the section %q runs into its subsections", page.heading)
			}
			checkInitWritesTheTeamApp(t, section)
		})
	}
}

// checkInitWritesTheTeamApp runs init with the recorded path's values, and holds its files and commands to the section's.
func checkInitWritesTheTeamApp(t *testing.T, section string) {
	t.Helper()
	ver := "v" + chartVersion(t)
	t.Chdir(t.TempDir())
	opts, err := parseInit(crArgs("my-values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := execInit(t.Context(), opts, ver, strings.NewReader(""), env{}.get, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr.String())
	}
	for _, f := range []struct{ path, opening string }{
		{"my-values.yaml", "```yaml title=\"my-values.yaml\""},
		{"team-a.yaml", "```yaml title=\"team-a.yaml\""},
	} {
		got := readFile(t, f.path)
		want := codeBlocks(t, section, f.opening)[0]
		if !reflect.DeepEqual(yamlDocs(t, got), yamlDocs(t, want)) {
			t.Errorf("init's %s is not the page's:\ninit:\n%s\npage:\n%s", f.path, got, want)
		}
		if !strings.Contains(got, "\n# ") && !strings.Contains(got, "\n  # ") {
			t.Errorf("%s has no comments:\n%s", f.path, got)
		}
	}
	// Each of the page's commands is printed as the page shows it, its lines together.
	for _, block := range codeBlocks(t, section, "```sh") {
		if !strings.Contains(stdout.String(), block) {
			t.Errorf("stdout lacks the page's commands\n%s\nstdout:\n%s", block, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), "Wrote my-values.yaml and team-a.yaml.") {
		t.Errorf("stdout does not name the two files:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), agentTokenSecretName) {
		t.Errorf("stdout names the agent token Secret without --agent-token:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "checked: Agent Kourier loads team-a.yaml") {
		t.Errorf("stderr %q does not say what was checked", stderr.String())
	}
	if bindings := loadTeamFile(t, "team-a.yaml"); len(bindings) != 1 {
		t.Errorf("resolved %d Bindings; want 1", len(bindings))
	}
}

// With --agent-token, the Binding reads its token from a Secret in the team's namespace, and the commands create that
// Secret there and add it to the team's grant to the broker.
func TestInitWithAnAgentTokenPutsTheSecretInTheTeamsNamespace(t *testing.T) {
	out := outFile(t)
	stdout, stderr, code := initRun(t, "", env{}.get, "", crArgs(out, "--agent-token")...)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	b := loadTeamFile(t, teamFile(out, pageTeam))[0]
	if ref := b.Spec.Identity.TokenSecretRef; b.Namespace != pageTeam || ref == nil || ref.Name != agentTokenSecretName || ref.Key != "token" {
		t.Errorf("Binding %s/%s tokenSecretRef = %+v; want %s, key token, in %s", b.Namespace, b.Name, ref, agentTokenSecretName, pageTeam)
	}
	for _, want := range []string{
		"kubectl -n team-a create secret generic " + agentTokenSecretName + " --from-literal=token=\"$AGENT_TOKEN\"\n",
		"--resource=secrets --resource-name=team-a-slack --resource-name=" + agentTokenSecretName + "\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if values := readFile(t, out); strings.Contains(values, "rbac") || strings.Contains(values, agentTokenSecretName) {
		t.Errorf("the values grant the token Secret, which is the team's to grant:\n%s", values)
	}
}

// Each dialect, with and without the token, makes a team file whose references resolve, in the team's namespace, whatever
// the agent's and the release's namespaces are.
func TestInitTeamAppResolves(t *testing.T) {
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
			bindings := loadTeamFile(t, teamFile(out, tc.team))
			if len(bindings) != 1 {
				t.Fatalf("resolved %d Bindings; want 1", len(bindings))
			}
			b := bindings[0]
			if b.Namespace != tc.team || b.Spec.Agent.Namespace != "agents" || b.Spec.Agent.Name != "payments-agent" || b.Spec.Chat.Channel != "G0ABCDEF123" {
				t.Errorf("Binding %s/%s = %+v", b.Namespace, b.Name, b.Spec)
			}
			if b.Spec.Chat.ConnectionRef.Name != tc.team+"-slack" || b.Spec.Chat.ConnectionRef.Namespace != "" || b.Spec.Agent.BackendRef.Namespace != "" {
				t.Errorf("the Binding names %+v and %+v; want both in its own namespace, the connection %s-slack", b.Spec.Agent.BackendRef, b.Spec.Chat.ConnectionRef, tc.team)
			}
			if (b.Spec.Identity.TokenSecretRef != nil) != tc.agentToken {
				t.Errorf("tokenSecretRef = %+v; want one: %v", b.Spec.Identity.TokenSecretRef, tc.agentToken)
			}
			var values struct {
				Config map[string]any `json:"config"`
			}
			if err := yaml.Unmarshal([]byte(readFile(t, out)), &values); err != nil {
				t.Fatal(err)
			}
			if want := map[string]any{"connectionNamespaces": []any{tc.team}}; !reflect.DeepEqual(values.Config, want) {
				t.Errorf("config = %v; want %v", values.Config, want)
			}
			for _, want := range []string{
				"kubectl create namespace kourier\n", "-n kourier -f " + out + "\n",
				"--serviceaccount=kourier:agent-kourier\n", "kubectl apply -f " + teamFile(out, tc.team) + "\n",
			} {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout)
				}
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
	if !strings.Contains(stderr, "Agent namespace: Team namespace, where its Slack app and Binding go [agents]: ") {
		t.Errorf("stderr does not ask the team's namespace after the agent's, with the agent's as the default:\n%s", stderr)
	}
	if b := loadTeamFile(t, teamFile(out, "agents"))[0]; b.Namespace != "agents" || b.Spec.Agent.Name != "sre-agent" {
		t.Errorf("Binding %s/%s = %+v; want it in agents", b.Namespace, b.Name, b.Spec)
	}

	out = outFile(t)
	_, stderr, code = initRun(t, "", env{}.get, "payments\n", crArgs(out, "--team-namespace", "")...)
	if code != exitOK || strings.Contains(stderr, "Agent URL") {
		t.Fatalf("exit code %d, stderr %q; want only the team's namespace asked", code, stderr)
	}
	if b := loadTeamFile(t, teamFile(out, "payments"))[0]; b.Namespace != "payments" {
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
		name string
		args func(dir string) []string
		want string
	}{
		{"a source that does not exist", func(dir string) []string { return crArgs(filepath.Join(dir, "v.yaml"), "--source", "configmap") }, `--source "configmap" is not crds or files`},
		{"a team namespace that is no name", func(dir string) []string { return crArgs(filepath.Join(dir, "v.yaml"), "--team-namespace", "My Team") }, "--team-namespace"},
		{"a values file named like the team's file", func(dir string) []string { return crArgs(filepath.Join(dir, "team-a.yaml")) }, "is where init writes the team's file"},
		{"a values file named like the team's file, with --force", func(dir string) []string { return crArgs(filepath.Join(dir, "team-a.yaml"), "--force") }, "is where init writes the team's file"},
		{"a values file named like the team's file in another case", func(dir string) []string { return crArgs(filepath.Join(dir, "Team-A.yaml"), "--force") }, "is where init writes the team's file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stdout, stderr, code := initRun(t, "", env{}.get, "", tc.args(dir)...)
			if code != exitUsage || !strings.Contains(stderr, tc.want) || stdout != "" {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d and %q", code, stdout, stderr, exitUsage, tc.want)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("files were written: %v", entries)
			}
		})
	}
}

// Either file that exists stops init, the values file before the questions and the team's once its namespace is known,
// and nothing is written, unless --force says so.
func TestInitDoesNotOverwriteEitherFile(t *testing.T) {
	for _, name := range []string{"values", "team"} {
		t.Run(name, func(t *testing.T) {
			out := outFile(t)
			paths := []string{out, teamFile(out, pageTeam)}
			existing, other := paths[0], paths[1]
			if name == "team" {
				existing, other = other, existing
			}
			if err := os.WriteFile(existing, []byte("mine\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := initRun(t, "", env{}.get, "", crArgs(out)...)
			if code != exitUsage || !strings.Contains(stderr, existing+" exists") || !strings.Contains(stderr, "--force") || stdout != "" {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d, %s and --force named", code, stdout, stderr, exitUsage, existing)
			}
			if _, err := os.Lstat(other); err == nil {
				t.Errorf("%s was written", other)
			}
			if got := readFile(t, existing); got != "mine\n" {
				t.Errorf("%s is now %q", existing, got)
			}

			if _, stderr, code := initRun(t, "", env{}.get, "", crArgs(out, "--force")...); code != exitOK {
				t.Fatalf("--force: exit code %d, stderr %q", code, stderr)
			}
			if got := readFile(t, existing); got == "mine\n" {
				t.Errorf("--force left %s as it was", existing)
			}
			loadTeamFile(t, paths[1])
		})
	}
	// The values file is known before the questions, so an existing one stops init before them; so does the team's file
	// when --team-namespace names it.
	out := outFile(t)
	if err := os.WriteFile(out, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := initRun(t, "", env{}.get, "", "--out", out); code != exitUsage || strings.Contains(stderr, "Agent URL") {
		t.Errorf("exit code %d, stderr %q; want %d and no question", code, stderr, exitUsage)
	}
	out = outFile(t)
	if err := os.WriteFile(teamFile(out, "payments"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := initRun(t, "", env{}.get, "", "--out", out, "--team-namespace", "payments"); code != exitUsage || strings.Contains(stderr, "Agent URL") || !strings.Contains(stderr, "another directory") {
		t.Errorf("exit code %d, stderr %q; want %d, no question and the team file's advice", code, stderr, exitUsage)
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

// What Slack refuses writes neither file, and a channel name is looked up into the team's Binding. The bot token is the
// team's app's.
func TestInitOnCustomResourcesChecksSlack(t *testing.T) {
	out := outFile(t)
	slackSrv := &initSlack{historyErr: "not_in_channel"}
	_, stderr, code := initRun(t, slackSrv.serve(t), env{"SLACK_BOT_TOKEN": canaryToken}.get, "", crArgs(out)...)
	if code != exitFailure || !strings.Contains(stderr, "invite the bot") {
		t.Errorf("exit code %d, stderr %q; want %d and invite the bot", code, stderr, exitFailure)
	}
	for _, p := range []string{out, teamFile(out, pageTeam)} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}

	slackSrv = &initSlack{channels: []map[string]string{{"id": "C0BBBBBBBBB", "name": "sre-alerts"}}}
	_, stderr, code = initRun(t, slackSrv.serve(t), env{"SLACK_BOT_TOKEN": canaryToken}.get, "", crArgs(out, "--channel", "#sre-alerts")...)
	if code != exitOK || !strings.Contains(stderr, "the bot can read C0BBBBBBBBB") {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if got := loadTeamFile(t, teamFile(out, pageTeam))[0].Spec.Chat.Channel; got != "C0BBBBBBBBB" {
		t.Errorf("the Binding has channel %q; want C0BBBBBBBBB", got)
	}
}

// The commands for the team's app quote the paths, put the token Secret in the team's namespace and name it in the grant,
// and apply the team's file last.
func TestNextStepsForTheTeamsApp(t *testing.T) {
	got := nextSteps(initOptions{source: sourceCRDs, out: "my dir/v.yaml", namespace: "kourier", teamNamespace: "payments", agentToken: true}, "v0.1.1")
	want := `Next, as the platform team, the namespace and the broker, which serves the ChatConnections of payments:

kubectl create namespace kourier
helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version 0.1.1 \
  -n kourier -f 'my dir/v.yaml'

Then, as the team, in its namespace payments, which must exist (see https://agentkourier.dev/how-to/team-slack-app/):

# AGENT_TOKEN is the bearer token the agent accepts for the Binding's identity; the Secret goes in the Binding's namespace
kubectl -n payments create secret generic agent-kourier-agent-token --from-literal=token="$AGENT_TOKEN"

# The team's Slack app: a file that only you can read, with the lines botToken=<bot token> and appToken=<app-level token>
kubectl -n payments create secret generic payments-slack \
  --from-env-file="$HOME/.config/payments/slack-secret.env"
kubectl -n payments create role agent-kourier-secrets-payments --verb=list,watch \
  --resource=secrets --resource-name=payments-slack --resource-name=agent-kourier-agent-token
kubectl -n payments create rolebinding agent-kourier-secrets-payments --role=agent-kourier-secrets-payments \
  --serviceaccount=kourier:agent-kourier

kubectl apply --dry-run=server -f 'my dir/payments.yaml'
kubectl apply -f 'my dir/payments.yaml'
kubectl -n payments get akbinding
`
	if got != want {
		t.Errorf("nextSteps =\n%s\nwant\n%s", got, want)
	}
}

// --out is trimmed before any path is made from it, so the file written is the one the commands name.
func TestInitTrimsTheValuesPath(t *testing.T) {
	for route, args := range map[string]func(string, ...string) []string{"files": starterArgs, "crds": crArgs} {
		t.Run(route, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			stdout, stderr, code := initRun(t, "", env{}.get, "", args("  v.yaml ")...)
			if code != exitOK {
				t.Fatalf("exit code %d, stderr %q", code, stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "v.yaml")); err != nil {
				t.Errorf("v.yaml was not written: %v", err)
			}
			if !strings.Contains(stdout, " -f v.yaml\n") {
				t.Errorf("stdout does not name v.yaml:\n%s", stdout)
			}
		})
	}
	if _, stderr, code := initRun(t, "", env{}.get, "", "--out", "  "); code != exitUsage || !strings.Contains(stderr, "--out is empty") {
		t.Errorf("--out of spaces: exit code %d, stderr %q", code, stderr)
	}
}

// --force overwrites only regular files, and checks each before it writes any, so that a run cannot replace one file and
// then stop at a symbolic link or a directory where the next goes.
func TestInitForceRefusesWhatIsNoFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "my-values.yaml")
	if err := os.WriteFile(out, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("theirs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, teamFile(out, pageTeam)); err != nil {
		t.Skip(err)
	}
	// With --team-namespace the team's file is checked before the questions; without it, after the team's namespace is
	// answered (here the default, the agent's namespace).
	withoutTeam := slices.Clone(crArgs(out, "--force"))
	i := slices.Index(withoutTeam, "--team-namespace")
	withoutTeam = slices.Delete(withoutTeam, i, i+2)
	for name, tc := range map[string]struct {
		args  []string
		stdin string
		asked bool
	}{
		"given":    {crArgs(out, "--force"), "", false},
		"answered": {withoutTeam, "\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := initRun(t, "", env{}.get, tc.stdin, tc.args...)
			if code != exitUsage || !strings.Contains(stderr, "is not a regular file") || stdout != "" {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d and not a regular file", code, stdout, stderr, exitUsage)
			}
			if got := strings.Contains(stderr, "Team namespace"); got != tc.asked {
				t.Errorf("asked for the team's namespace: %v, want %v; stderr %q", got, tc.asked, stderr)
			}
			if got := readFile(t, out); got != "mine\n" {
				t.Errorf("the values file is now %q", got)
			}
			if got := readFile(t, target); got != "theirs\n" {
				t.Errorf("the link's target is now %q", got)
			}
		})
	}
}
