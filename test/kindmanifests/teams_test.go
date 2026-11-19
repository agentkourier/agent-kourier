package kindmanifests_test

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/test/e2e"
)

// The fast tier's Microsoft Teams connections (E2E_TEAMS=1; hack/kind/fast/teams and test/e2e's TEAMS_ tests): the fake Teams service hosts the bots and
// channels the tests name, its activities go to each connection's own route, the manifests bind the channels the tests use to the connections they expect, and
// the broker's hooks name the fake and its certificate.

const (
	teamsFakeFile   = "../../hack/kind/fast/teams/faketeams.yaml"
	teamsFile       = "../../hack/kind/fast/teams/teams.yaml"
	teamsValuesFile = "../../hack/kind/teams-values.yaml"
)

// fakeTeamsFlags returns the fake Teams service's flags, each with its values in order.
func fakeTeamsFlags(t *testing.T) map[string][]string {
	t.Helper()
	var d appsv1.Deployment
	load(t, teamsFakeFile, "Deployment", &d)
	cmd := d.Spec.Template.Spec.Containers[0].Command
	if d.Name != "faketeams" || len(cmd) == 0 || cmd[0] != "/faketeams" {
		t.Fatalf("%s runs %q as %q; want /faketeams as faketeams", teamsFakeFile, cmd, d.Name)
	}
	flags := map[string][]string{}
	for i := 1; i < len(cmd); i++ {
		name, ok := strings.CutPrefix(cmd[i], "-")
		if !ok {
			t.Fatalf("the fake's command has %q where a flag goes", cmd[i])
		}
		if name == "deliver-http" {
			flags[name] = append(flags[name], "true")
			continue
		}
		if i+1 == len(cmd) {
			t.Fatalf("the fake's flag -%s has no value", name)
		}
		flags[name] = append(flags[name], cmd[i+1])
		i++
	}
	return flags
}

// teamsObjects decodes the objects of teams.yaml, Defaulted, by kind.
func teamsObjects(t *testing.T) (conns map[string]v1alpha1.ChatConnection, bindings map[string]v1alpha1.Binding, backends []v1alpha1.AgentBackend, role rbacv1.Role) {
	t.Helper()
	conns, bindings = map[string]v1alpha1.ChatConnection{}, map[string]v1alpha1.Binding{}
	for _, doc := range objects(t, teamsFile) {
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		switch doc["kind"] {
		case "ChatConnection":
			var c v1alpha1.ChatConnection
			mustDecode(t, b, &c)
			c.Default()
			conns[c.Name] = c
		case "Binding":
			var bd v1alpha1.Binding
			mustDecode(t, b, &bd)
			bd.Default()
			bindings[bd.Name] = bd
		case "AgentBackend":
			var a v1alpha1.AgentBackend
			mustDecode(t, b, &a)
			a.Default()
			backends = append(backends, a)
		case "Role":
			mustDecode(t, b, &role)
		}
	}
	return conns, bindings, backends, role
}

func mustDecode(t *testing.T, b []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

// TestTheTeamsManifestsBindTheChannelsOfTheTests: each object of teams.yaml is valid, in the team's namespace, and each Binding is on the connection and
// channel test/e2e's constants give it, holds to the Teams platform's shapes, and names the Workflows app the tests post alerts as. The connections are
// of the tenant and bots the tests name, in http mode, and only teams-push has a notificationURL, its own route.
func TestTheTeamsManifestsBindTheChannelsOfTheTests(t *testing.T) {
	conns, bindings, backends, role := teamsObjects(t)
	if len(conns) != 2 || len(backends) != 1 || len(bindings) != 5 {
		t.Fatalf("%s has %d connections, %d backends and %d Bindings; want 2, 1 and 5", teamsFile, len(conns), len(backends), len(bindings))
	}
	for _, want := range []struct{ name, app, notify string }{
		{"teams-push", e2e.TeamsAppPush, "https://agent-kourier-chat.agent-kourier.svc.cluster.local:8081/chat/teams/teams-e2e/teams-push"},
		{"teams-poll", e2e.TeamsAppPoll, ""},
	} {
		c, ok := conns[want.name]
		switch {
		case !ok:
			t.Errorf("no ChatConnection %s", want.name)
		case c.Namespace != e2e.TeamsNamespace || c.Spec.Platform != v1alpha1.PlatformTeams || c.Spec.Teams == nil:
			t.Errorf("ChatConnection %s/%s is %q; want a teams connection in %s", c.Namespace, c.Name, c.Spec.Platform, e2e.TeamsNamespace)
		case c.Spec.Teams.TenantID != e2e.TeamsTenant || c.Spec.Teams.AppID != want.app || c.Spec.TeamsMode() != v1alpha1.TeamsModeHTTP ||
			c.Spec.Teams.NotificationURL != want.notify:
			t.Errorf("ChatConnection %s has %+v; want tenant %s, app %s, http mode and notificationURL %q", c.Name, *c.Spec.Teams, e2e.TeamsTenant, want.app, want.notify)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("ChatConnection %s: %v", c.Name, err)
		}
	}
	for _, a := range backends {
		if err := a.Validate(); err != nil || a.Namespace != e2e.TeamsNamespace {
			t.Errorf("AgentBackend %s/%s: %v; want a valid one in %s", a.Namespace, a.Name, err, e2e.TeamsNamespace)
		}
	}
	for _, want := range []struct {
		binding, conn, channel, agent string
		alert, approvals              bool
	}{
		{"mention", "teams-push", e2e.ChannelTeamsMention, "scenario-gated", false, false},
		{"approval", "teams-push", e2e.ChannelTeamsApproval, "spike-approve", false, true},
		{"alert-push", "teams-push", e2e.ChannelTeamsAlertPush, "scenario-alert", true, false},
		{"alert-poll", "teams-poll", e2e.ChannelTeamsAlertPoll, "scenario-alert", true, false},
		{"failover", "teams-push", e2e.ChannelTeamsFailover, "scenario-alert", true, false},
	} {
		b, ok := bindings[want.binding]
		if !ok {
			t.Errorf("no Binding %s", want.binding)
			continue
		}
		s := b.Spec
		if b.Namespace != e2e.TeamsNamespace || s.Chat.ConnectionRef.Name != want.conn || s.Chat.Channel != want.channel || s.Agent.Name != want.agent ||
			s.Interactions.ToolApprovals != want.approvals || s.Chat.Output != v1alpha1.OutputLive {
			t.Errorf("Binding %s/%s is on %s, channel %s, agent %s, tool approvals %v, output %s; want %s, %s, %s, %v, live", b.Namespace, b.Name,
				s.Chat.ConnectionRef.Name, s.Chat.Channel, s.Agent.Name, s.Interactions.ToolApprovals, s.Chat.Output, want.conn, want.channel, want.agent, want.approvals)
		}
		if err := b.Validate(); err != nil {
			t.Errorf("Binding %s: %v", b.Name, err)
		}
		if p, ok := v1alpha1.ChatPlatformOf(conns[want.conn].Spec); !ok {
			t.Errorf("Binding %s: its connection has no platform", b.Name)
		} else if m := s.PlatformMismatches(p); len(m) != 0 {
			t.Errorf("Binding %s does not hold to Microsoft Teams: %+v", b.Name, m)
		}
		alert := len(s.Chat.Triggers) == 1 && s.Chat.Triggers[0].Type == v1alpha1.TriggerBotMessage && s.Chat.Triggers[0].From != nil &&
			s.Chat.Triggers[0].From.ID == e2e.TeamsWorkflows
		if alert != want.alert {
			t.Errorf("Binding %s has triggers %+v; want a botMessage trigger on %s: %v", b.Name, s.Chat.Triggers, e2e.TeamsWorkflows, want.alert)
		}
	}
	// The Role grants the broker the Secrets the team's objects name, which deploy_teams_connections makes, and nothing else.
	lib, err := os.ReadFile(twoAppsLib)
	if err != nil {
		t.Fatal(err)
	}
	refs := []string{}
	for _, c := range conns {
		refs = append(refs, c.Spec.CredentialsSecretRef.Name)
	}
	for _, b := range bindings {
		refs = append(refs, b.Spec.Identity.TokenSecretRef.Name)
	}
	slices.Sort(refs)
	refs = slices.Compact(refs)
	if len(role.Rules) != 1 || !slices.Equal(slices.Sorted(slices.Values(role.Rules[0].ResourceNames)), refs) || role.Namespace != e2e.TeamsNamespace {
		t.Errorf("the Role %s/%s grants %+v; want the Secrets %q alone", role.Namespace, role.Name, role.Rules, refs)
	}
	for _, name := range refs {
		if !strings.Contains(string(lib), "-n teams-e2e create secret generic "+name+" ") {
			t.Errorf("lib.sh does not make the Secret %s in teams-e2e", name)
		}
	}
}

// TestTheFakeTeamsHostsTheConnectionsOfTheTests: the fake Teams service is of the tenant the connections are, hosts each connection's bot with its own route on
// the chat listener as the messaging endpoint, has every channel a Binding names, takes the client secret that the tier's Secret holds, and is reached by the
// broker at the URL the hooks name, with the certificate deploy_fake_teams makes for that host.
func TestTheFakeTeamsHostsTheConnectionsOfTheTests(t *testing.T) {
	flags := fakeTeamsFlags(t)
	conns, bindings, _, _ := teamsObjects(t)
	if got := flags["tenant"]; !slices.Equal(got, []string{e2e.TeamsTenant}) {
		t.Errorf("the fake's -tenant = %q; want %s", got, e2e.TeamsTenant)
	}
	bots := map[string]string{}
	for _, b := range flags["bot"] {
		app, endpoint, _ := strings.Cut(b, "=")
		bots[app] = endpoint
	}
	for _, c := range conns {
		u, err := url.Parse(bots[c.Spec.Teams.AppID])
		if err != nil || u.Host != "agent-kourier-chat.agent-kourier.svc.cluster.local:8081" || u.Path != "/chat/teams/"+c.Namespace+"/"+c.Name {
			t.Errorf("the fake sends the bot of %s to %q; want its route on the chat Service", c.Name, bots[c.Spec.Teams.AppID])
		}
	}
	if len(bots) != len(conns) {
		t.Errorf("the fake hosts the bots %q; want those of the %d connections", bots, len(conns))
	}
	for _, b := range bindings {
		if !slices.Contains(flags["channel"], b.Spec.Chat.Channel) {
			t.Errorf("the fake has no channel %s, which Binding %s names", b.Spec.Chat.Channel, b.Name)
		}
	}
	if len(flags["deliver-http"]) != 1 {
		t.Error("the fake does not send the notifications of an https notificationURL over plain HTTP; the chat listener serves no TLS")
	}
	lib, err := os.ReadFile(twoAppsLib)
	if err != nil {
		t.Fatal(err)
	}
	secret := flags["client-secret"]
	if len(secret) != 1 || !strings.Contains(string(lib), "--from-literal=clientSecret="+secret[0]+" ") {
		t.Errorf("the fake takes the client secret %q, which deploy_teams_connections does not put in the Secret", secret)
	}

	// The hooks: the fake's public URL, and its certificate from the ConfigMap that deploy_fake_teams makes for the Service's name.
	raw, err := os.ReadFile(teamsValuesFile)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Config struct {
			ConnectionNamespaces []string `json:"connectionNamespaces"`
		} `json:"config"`
		ChatListener struct {
			Enabled bool `json:"enabled"`
		} `json:"chatListener"`
		ExtraEnv []corev1.EnvVar `json:"extraEnv"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(values.Config.ConnectionNamespaces, e2e.TeamsNamespace) || !values.ChatListener.Enabled {
		t.Errorf("%s admits %q with the chat listener %v; want %s admitted and the listener on", teamsValuesFile, values.Config.ConnectionNamespaces,
			values.ChatListener.Enabled, e2e.TeamsNamespace)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range values.ExtraEnv {
		env[e.Name] = e
	}
	public := flags["public-url"]
	if len(public) != 1 {
		t.Fatalf("the fake's -public-url = %q; want one", public)
	}
	if env["AGENTKOURIER_TEAMS_URL"].Value != public[0] {
		t.Errorf("AGENTKOURIER_TEAMS_URL = %q; want the fake's -public-url %q", env["AGENTKOURIER_TEAMS_URL"].Value, public)
	}
	if ref := env["AGENTKOURIER_TEAMS_CA"].ValueFrom; ref == nil || ref.ConfigMapKeyRef == nil || ref.ConfigMapKeyRef.Name != "faketeams-ca" || ref.ConfigMapKeyRef.Key != "ca.crt" {
		t.Errorf("AGENTKOURIER_TEAMS_CA is %+v; want the key ca.crt of the ConfigMap faketeams-ca", env["AGENTKOURIER_TEAMS_CA"])
	}
	if env["AGENTKOURIER_SLACK_API_URL"].Value == "" {
		t.Error("the overlay's extraEnv, which replaces the tier's, drops AGENTKOURIER_SLACK_API_URL")
	}
	host := ""
	if u, err := url.Parse(public[0]); err == nil {
		host = u.Hostname()
	}
	for _, want := range []string{"cert -host " + host + " ", "configmap faketeams-ca --from-file=ca.crt=", "secret tls faketeams-tls "} {
		if host == "" || !strings.Contains(string(lib), want) {
			t.Errorf("deploy_fake_teams has no %q", want)
		}
	}
}

// TestTheTeamsTargetRunsEveryTEAMSTest: the main Job's pattern leaves the TEAMS_ tests out (TestTheRunnerJobRunsEveryE2ETest), so make e2e-teams is what runs
// them, all of them.
func TestTheTeamsTargetRunsEveryTEAMSTest(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mk), "\tE2E_RUN='^TestTEAMS_' hack/kind/e2e-run.sh fast\n") {
		t.Error("make e2e-teams does not run every test named TestTEAMS_ in the fast tier")
	}
}

// TestUpFastRefusesTeamsItCannotRun: the Teams connections are a team's custom resources, so E2E_TEAMS=1 needs the custom resources route; it sets the admission
// list that E2E_TWO_APPS=1 sets too; and the value is 0 or 1. The script says so before it calls any tool.
func TestUpFastRefusesTeamsItCannotRun(t *testing.T) {
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
		"the ConfigMap route":      {[]string{"E2E_TEAMS=1", "E2E_CONFIG_SOURCE=files"}, "E2E_TEAMS=1 needs E2E_CONFIG_SOURCE=crds"},
		"the default route":        {[]string{"E2E_TEAMS=1"}, "E2E_TEAMS=1 needs E2E_CONFIG_SOURCE=crds"},
		"two Slack apps":           {[]string{"E2E_TEAMS=1", "E2E_TWO_APPS=1", "E2E_CONFIG_SOURCE=crds"}, "bring the tier up with one of them"},
		"a value it does not know": {[]string{"E2E_TEAMS=yes", "E2E_CONFIG_SOURCE=crds"}, "E2E_TEAMS must be 0 or 1"},
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

// TestTheTeamsAlertPromptsRenderWithATeamsChannel: the promptTemplate of each alert Binding of teams.yaml reads .Channel, which is a Teams channel ID
// (19:…@thread.tacv2), and renders through the broker's own render with that channel and what the trigger extracts from an alert's card. The TEAMS_
// alert tests need it to, and this fails with no tier when the render refuses the channel's '@'.
func TestTheTeamsAlertPromptsRenderWithATeamsChannel(t *testing.T) {
	_, bindings, _, _ := teamsObjects(t)
	for _, name := range []string{"alert-push", "alert-poll", "failover"} {
		b, ok := bindings[name]
		if !ok || len(b.Spec.Chat.Triggers) != 1 {
			t.Errorf("Binding %s has no one trigger", name)
			continue
		}
		tmpl := b.Spec.Chat.Triggers[0].PromptTemplate
		if !strings.Contains(tmpl, "{{ .Channel }}") {
			t.Errorf("Binding %s's promptTemplate does not read .Channel; this test guards that a Teams channel ID renders there", name)
		}
		p, err := render.NewPrompt(name, tmpl)
		if err != nil {
			t.Errorf("Binding %s: %v", name, err)
			continue
		}
		view := v1alpha1.PromptView{Channel: b.Spec.Chat.Channel, State: "FIRING", Key: "TeamsPush123", Source: "botMessage",
			Fields: map[string]string{"state": "FIRING", "key": "TeamsPush123", "alertname": "TeamsPush123"}}
		out, err := p.Render(view, render.TriggerMessage{Body: "[FIRING] TeamsPush123 shop\nnamespace: shop"})
		if err != nil {
			t.Errorf("Binding %s's prompt with the channel %s: %v", name, b.Spec.Chat.Channel, err)
			continue
		}
		if !strings.Contains(out, "An alert is FIRING in channel "+b.Spec.Chat.Channel+": TeamsPush123.") {
			t.Errorf("Binding %s's prompt is %q; want the channel and the alert's name in its first line", name, out)
		}
	}
}
