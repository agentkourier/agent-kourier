package kindmanifests_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/test/e2e"
	"github.com/agentkourier/agent-kourier/test/fakeweb"
)

// The fast tier's web ChatConnections (hack/kind/fast/web, on custom resources; test/e2e's WEB_ tests): the manifests are valid objects of the API, with the
// names, channels and agents that test/e2e's web.go gives them; the fake web sender and receiver sign and check with the secrets lib.sh's deploy_web puts in the
// connections' Secrets; and the tier serves them on custom resources only.

const (
	webFile   = "../../hack/kind/fast/web/web.yaml"
	webValues = "../../hack/kind/web-values.yaml"
)

// decodeWeb decodes one API object of web.yaml as the custom resource source does (config.DecodeObject: defaulted, then validated), and fails the test on a
// field the type does not have, which the CRD's schema would prune.
func decodeWeb[T any, PT interface {
	*T
	Default()
	Validate() error
}](t *testing.T, doc map[string]any) *T {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	obj, unknown, errs := config.DecodeObject[T, PT](raw)
	if len(errs) > 0 || len(unknown) > 0 {
		meta := doc["metadata"].(map[string]any)
		t.Fatalf("%s: the %s %s/%s: problems %v, fields the API does not have %v", webFile, doc["kind"], meta["namespace"], meta["name"], errs, unknown)
	}
	return obj
}

// webObjects decodes the API objects of web.yaml as the broker does, and resolves them together (config.ResolvePartial), as the broker resolves what it
// serves: every Binding must be served, with its connection's platform rules applied (output none on the github profile, the channel's shape). It returns
// the resolved objects by namespace/name.
func webObjects(t *testing.T) (conns map[string]v1alpha1.ChatConnection, bindings map[string]v1alpha1.Binding, backends map[string]v1alpha1.AgentBackend) {
	t.Helper()
	var set config.Set
	for _, doc := range objects(t, webFile) {
		switch doc["kind"] {
		case config.KindChatConnection:
			set.ChatConnections = append(set.ChatConnections, *decodeWeb[v1alpha1.ChatConnection](t, doc))
		case config.KindBinding:
			set.Bindings = append(set.Bindings, *decodeWeb[v1alpha1.Binding](t, doc))
		case config.KindAgentBackend:
			set.AgentBackends = append(set.AgentBackends, *decodeWeb[v1alpha1.AgentBackend](t, doc))
		}
	}
	n := len(set.Bindings)
	snap, rejected := config.ResolvePartial(&set)
	for _, r := range rejected {
		t.Errorf("the Binding %s/%s is not served: %s: %s", r.Namespace, r.Name, r.Reason, r.Message)
	}
	conns, bindings, backends = map[string]v1alpha1.ChatConnection{}, map[string]v1alpha1.Binding{}, map[string]v1alpha1.AgentBackend{}
	for _, rb := range snap.Bindings() {
		bindings[rb.Binding.Namespace+"/"+rb.Binding.Name] = *rb.Binding
		conns[rb.Connection.Namespace+"/"+rb.Connection.Name] = *rb.Connection
		backends[rb.Backend.Namespace+"/"+rb.Backend.Name] = *rb.Backend
	}
	if len(bindings) != n {
		t.Fatalf("%d of the %d Bindings of %s are served", len(bindings), n, webFile)
	}
	return conns, bindings, backends
}

// TestTheWebManifestsAreTheConnectionsTheTestsUse: a kourier-v1 connection whose replies go to the fake receiver, a github one with no reply URL, each
// with a Binding of its name on the channel and stub agent test/e2e expects, in the output the tests count on.
func TestTheWebManifestsAreTheConnectionsTheTestsUse(t *testing.T) {
	conns, bindings, backends := webObjects(t)
	receiver := e2e.NewEnv(func(string) string { return "" }).WebURL + fakeweb.RepliesPath
	for key, want := range map[string]struct {
		profile  v1alpha1.WebProfile
		url      string
		identity string
		secret   string
	}{
		e2e.ConnWebKourier: {v1alpha1.WebProfileKourierV1, receiver, "", "web-kourier"},
		e2e.ConnWebGitHub:  {v1alpha1.WebProfileGitHub, "", e2e.GitHubLogin, "web-github"},
	} {
		c, ok := conns[key]
		switch {
		case !ok:
			t.Errorf("%s has no ChatConnection %s; it has %v", webFile, key, slices.Sorted(maps.Keys(conns)))
		case c.Spec.Platform != v1alpha1.PlatformWeb || c.Spec.Web == nil || c.Spec.Web.Profile != want.profile || c.Spec.Web.URL != want.url ||
			c.Spec.Web.Identity != want.identity:
			t.Errorf("the ChatConnection %s is %+v; want platform web, profile %s, reply URL %q and identity %q", key, c.Spec, want.profile, want.url, want.identity)
		case c.Spec.CredentialsSecretRef.Name != want.secret:
			t.Errorf("the ChatConnection %s reads the Secret %q; want %q, which deploy_web makes", key, c.Spec.CredentialsSecretRef.Name, want.secret)
		}
	}
	for key, want := range map[string]struct {
		conn, channel, agent string
		output               v1alpha1.OutputMode
	}{
		e2e.ConnWebKourier: {"kourier", e2e.ChannelWebKourier, e2e.AgentWebKourier, v1alpha1.OutputFinal},
		e2e.ConnWebGitHub:  {"github", e2e.GitHubRepo, e2e.AgentWebGitHub, v1alpha1.OutputNone},
	} {
		b, ok := bindings[key]
		if !ok {
			t.Errorf("%s has no Binding %s; it has %v", webFile, key, slices.Sorted(maps.Keys(bindings)))
			continue
		}
		s := b.Spec
		if s.Chat.ConnectionRef.Name != want.conn || s.Chat.Channel != want.channel || s.Agent.Name != want.agent || s.Chat.Output != want.output {
			t.Errorf("the Binding %s is on %s, channel %s, agent %s, output %s; want %s, %s, %s, %s", key, s.Chat.ConnectionRef.Name, s.Chat.Channel, s.Agent.Name,
				s.Chat.Output, want.conn, want.channel, want.agent, want.output)
		}
		if s.Agent.Namespace != "kagent" || s.Identity.TokenSecretRef == nil || s.Identity.TokenSecretRef.Name != "web-agent-token" {
			t.Errorf("the Binding %s asks the agent %s/%s with the token of %+v; want the stub's kagent agent, with the Secret web-agent-token, which test/e2e lists the tasks of",
				key, s.Agent.Namespace, s.Agent.Name, s.Identity.TokenSecretRef)
		}
		if a, ok := backends[e2e.WebNamespace+"/"+s.Agent.BackendRef.Name]; !ok || a.Spec.URL != "http://stubagent.e2e.svc.cluster.local:8080" {
			t.Errorf("the Binding %s's AgentBackend %s is %+v; want the stub agent's Service", key, s.Agent.BackendRef.Name, a.Spec)
		}
	}
}

// TestTheGitHubLoginIsWrittenOnce: the github connection's login, which test/e2e's GitHubLogin is the value of, appears once in the manifests, so that a change of
// the field that holds it is one line. The test looks for the value, not the field, for the same reason.
func TestTheGitHubLoginIsWrittenOnce(t *testing.T) {
	var found []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				walk(path+"."+k, x)
			}
		case []any:
			for i, x := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), x)
			}
		case string:
			if v == e2e.GitHubLogin {
				found = append(found, path)
			}
		}
	}
	for _, doc := range objects(t, webFile) {
		meta := doc["metadata"].(map[string]any)
		walk(fmt.Sprintf("%s %s/%s", doc["kind"], meta["namespace"], meta["name"]), doc)
	}
	if len(found) != 1 || !strings.HasPrefix(found[0], "ChatConnection "+e2e.ConnWebGitHub+".spec.web.") {
		t.Errorf("the login %q is at %v; want it once, in the github ChatConnection's spec.web", e2e.GitHubLogin, found)
	}
}

// TestTheFakeWebSignsWithTheConnectionsSecrets: the fake web's command gives it the secrets that deploy_web puts in the connections' Secrets, under the keys the
// profiles read, and the chat listener's Service and port that web-values.yaml turns on; the Role grants the broker the Secrets the objects name, and deploy_web
// makes each, the agent token with test/e2e's WebAgentEmail.
func TestTheFakeWebSignsWithTheConnectionsSecrets(t *testing.T) {
	var d appsv1.Deployment
	for _, doc := range objects(t, doublesFile) {
		if doc["kind"] != "Deployment" || doc["metadata"].(map[string]any)["name"] != "fakeweb" {
			continue
		}
		raw, _ := json.Marshal(doc)
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
	}
	if d.Name != "fakeweb" {
		t.Fatalf("%s has no Deployment fakeweb", doublesFile)
	}
	args := map[string]string{}
	cmd := d.Spec.Template.Spec.Containers[0].Command
	for i := 1; i+1 < len(cmd); i += 2 {
		args[cmd[i]] = cmd[i+1]
	}
	lib := readHackKind(t, "lib.sh")
	constant := func(name string) string {
		m := regexp.MustCompile(`(?m)^` + name + `=(\S+)$`).FindStringSubmatch(lib)
		if m == nil {
			t.Fatalf("lib.sh sets no %s", name)
		}
		return m[1]
	}
	if got, want := args["-kourier-v1-secret"], constant("FAKE_WEB_KOURIER_V1_SECRET"); got != want {
		t.Errorf("the fake web signs kourier-v1 with %q; deploy_web puts %q in the connection's Secret", got, want)
	}
	if got, want := args["-github-secret"], constant("FAKE_WEB_GITHUB_SECRET"); got != want {
		t.Errorf("the fake web signs GitHub's deliveries with %q; deploy_web puts %q in the connection's Secret", got, want)
	}
	if got := constant("WEB_AGENT_EMAIL"); got != e2e.WebAgentEmail {
		t.Errorf("deploy_web's agent token is of %q; test/e2e lists the tasks of %q", got, e2e.WebAgentEmail)
	}
	values := readHackKind(t, "web-values.yaml")
	if got := args["-chat-url"]; got != "http://agent-kourier-chat.agent-kourier.svc.cluster.local:8081" || !strings.Contains(values, "service: {port: 8081}") {
		t.Errorf("the fake web sends to %q; want the chat listener's Service on the port web-values.yaml gives it", got)
	}
	for _, want := range []string{
		"create secret generic web-kourier --from-literal=" + v1alpha1.KourierV1SigningSecretKey + `="${FAKE_WEB_KOURIER_V1_SECRET}"`,
		"create secret generic web-github --from-literal=" + v1alpha1.GitHubWebhookSecretKey + `="${FAKE_WEB_GITHUB_SECRET}"`,
		`create secret generic web-agent-token --from-literal=token="$(unsigned_jwt "${WEB_AGENT_EMAIL}")"`,
	} {
		if !strings.Contains(lib, want) {
			t.Errorf("lib.sh's deploy_web does not make %q", want)
		}
	}

	conns, bindings, _ := webObjects(t)
	var named []string
	for _, c := range conns {
		named = append(named, c.Spec.CredentialsSecretRef.Name)
	}
	for _, b := range bindings {
		if ref := b.Spec.Identity.TokenSecretRef; ref != nil && !slices.Contains(named, ref.Name) {
			named = append(named, ref.Name)
		}
	}
	var role rbacv1.Role
	var rb rbacv1.RoleBinding
	load(t, webFile, "Role", &role)
	load(t, webFile, "RoleBinding", &rb)
	slices.Sort(named)
	granted := slices.Clone(role.Rules[0].ResourceNames)
	slices.Sort(granted)
	if role.Namespace != e2e.WebNamespace || len(role.Rules) != 1 || !slices.Equal(granted, named) || !slices.Equal(role.Rules[0].Verbs, []string{"list", "watch"}) {
		t.Errorf("the Role = %s %+v; want list and watch of the Secrets %v by name", role.Namespace, role.Rules, named)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != "agent-kourier" || rb.Subjects[0].Namespace != "agent-kourier" || rb.RoleRef.Name != role.Name {
		t.Errorf("the RoleBinding = %+v; want the Role bound to the ServiceAccount agent-kourier/agent-kourier", rb)
	}
}

// TestTheTierServesTheWebConnectionsOnCustomResourcesOnly: web-values.yaml admits e2e-web and turns on the chat listener, which a web connection needs; up-fast.sh
// gives it to the route upgrade and runs deploy_web on custom resources, and not with two Slack apps, whose admission list the MC_ tests hold; the doubles are
// built, rolled out and checked for publication with the fake web among them.
func TestTheTierServesTheWebConnectionsOnCustomResourcesOnly(t *testing.T) {
	values := readHackKind(t, "web-values.yaml")
	for _, want := range []string{"connectionNamespaces: [" + e2e.WebNamespace + "]", "chatListener:\n  enabled: true", "podSelector: {matchLabels: {app: fakeweb}}"} {
		if !strings.Contains(values, want) {
			t.Errorf("%s lacks %q", webValues, want)
		}
	}
	up := readHackKind(t, "up-fast.sh")
	branch := regexp.MustCompile(`(?s)if \[ "\$\{E2E_TWO_APPS\}" = 1 \]; then\n\s*AGENTKOURIER_CRDS_ARGS=\(-f "\$\{HERE\}/two-apps-values.yaml"\)\nelif \[ "\$\{E2E_CONFIG_SOURCE\}" = crds \]; then\n\s*WEB=1\n\s*AGENTKOURIER_CRDS_ARGS=\(-f "\$\{HERE\}/web-values.yaml"\)\nfi`)
	if !branch.MatchString(up) {
		t.Error("up-fast.sh does not give web-values.yaml to the route upgrade on custom resources alone, with two Slack apps taking two-apps-values.yaml instead")
	}
	if !strings.Contains(up, "if [ \"${WEB}\" = 1 ]; then\n  step \"e2e-web:") || !strings.Contains(up, "\n  deploy_web\n") {
		t.Error("up-fast.sh does not run deploy_web when it serves the web connections")
	}
	lib := readHackKind(t, "lib.sh")
	for _, want := range []string{
		"for double in fakeslack stubagent sink signaler hitlagent fakeweb; do",
		"rollout status deploy/fakeslack deploy/stubagent deploy/hitlagent deploy/sink deploy/fakeweb",
		"for app in fakeslack stubagent hitlagent fakeweb; do",
	} {
		if !strings.Contains(lib, want) {
			t.Errorf("lib.sh lacks %q: the fake web is a double like the others, built, rolled out and never published", want)
		}
	}
	if !strings.Contains(readHackKind(t, "fast/Dockerfile"), " fakeweb /") {
		t.Error("the doubles' image does not hold the fake web's binary")
	}
}
