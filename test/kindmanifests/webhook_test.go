package kindmanifests_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/test/e2e"
)

// The fast tier's webhook connections (custom resources only; test/e2e's WH_ tests): the four recipes of examples/recipes/webhook-*, applied
// by lib.sh's deploy_webhook as they are but for the agent token each Binding is given (webhook_binding), with hack/kind/fast/webhook/webhook.yaml's
// AgentBackends in place of the recipes' (a placeholder URL, or the Service of the GitHub recipe's own agent, which the tier does not run). The objects resolve together on the stub agent's webhook agents; the fake vendors sign
// with the secrets deploy_webhook puts in the connections' Secrets, as the agent's own accounts, at the routes test/e2e names; and the tier serves
// them on custom resources only.

const (
	webhookManifest = "../../hack/kind/fast/webhook/webhook.yaml"
	webhookValues   = "../../hack/kind/webhook-values.yaml"
	recipesDir      = "../../examples/recipes"
	// stubAgentURL and fakeVendorsURL are the Services of the stub agent and the fake vendors in doubles.yaml.
	stubAgentURL   = "http://stubagent.e2e.svc.cluster.local:8080"
	fakeVendorsURL = "http://fakevendors.e2e.svc.cluster.local:8080"
)

// recipeDoc is the one object of a recipe's file, of vendor's recipe.
func recipeDoc(t *testing.T, vendor, file string) map[string]any {
	t.Helper()
	docs := objects(t, filepath.Join(recipesDir, "webhook-"+vendor, "kourier", file))
	if len(docs) != 1 {
		t.Fatalf("the %s recipe's %s holds %d objects; want 1", vendor, file, len(docs))
	}
	return docs[0]
}

// decodeAs decodes one API object as the custom resource source does (config.DecodeObject: defaulted, then validated), and fails the test on
// a field the type does not have, which the CRD's schema would prune.
func decodeAs[T any, PT interface {
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
		t.Fatalf("the %s %v: problems %v, fields the API does not have %v", doc["kind"], doc["metadata"], errs, unknown)
	}
	return obj
}

// webhookBinding runs lib.sh's webhook_binding over the Binding doc, as deploy_webhook pipes each recipe's Binding through it, and returns what
// it wrote.
func webhookBinding(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := filepath.Abs("../../hack/kind/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -eu; . "$1"; webhook_binding`, "bash", lib)
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("webhook_binding = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("webhook_binding wrote %q, not one JSON object: %v", out, err)
	}
	return got
}

// webhookObjects decodes the tier's webhook objects as the broker does, the recipes' ChatConnections and Bindings (each through
// webhook_binding) and webhook.yaml's AgentBackends, and resolves them together (config.ResolvePartial), as the broker resolves what it
// serves: every Binding must be served. It returns the resolved Bindings by namespace/name.
func webhookObjects(t *testing.T) map[string]config.ResolvedBinding {
	t.Helper()
	var set config.Set
	for _, doc := range objects(t, webhookManifest) {
		if doc["kind"] == config.KindAgentBackend {
			set.AgentBackends = append(set.AgentBackends, *decodeAs[v1alpha1.AgentBackend](t, doc))
		}
	}
	for _, vendor := range e2e.WebhookVendors {
		set.ChatConnections = append(set.ChatConnections, *decodeAs[v1alpha1.ChatConnection](t, recipeDoc(t, vendor, "chatconnection.yaml")))
		set.Bindings = append(set.Bindings, *decodeAs[v1alpha1.Binding](t, webhookBinding(t, recipeDoc(t, vendor, "binding.yaml"))))
	}
	snap, rejected := config.ResolvePartial(&set)
	for _, r := range rejected {
		t.Errorf("the Binding %s/%s is not served: %s: %s", r.Namespace, r.Name, r.Reason, r.Message)
	}
	out := map[string]config.ResolvedBinding{}
	for _, rb := range snap.Bindings() {
		out[rb.Binding.Namespace+"/"+rb.Binding.Name] = rb
	}
	if len(out) != len(e2e.WebhookVendors) {
		t.Fatalf("%d of the %d webhook Bindings are served", len(out), len(e2e.WebhookVendors))
	}
	return out
}

// libList is a bash array of lib.sh, one element to a line.
func libList(t *testing.T, name string) []string {
	t.Helper()
	return strings.Fields(libVar(t, `printf '%s\n' "${`+name+`[@]}"`))
}

// fakeWebhookSecrets is lib.sh's FAKE_WEBHOOK_SECRETS, by vendor.
func fakeWebhookSecrets(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, pair := range libList(t, "FAKE_WEBHOOK_SECRETS") {
		vendor, secret, ok := strings.Cut(pair, "=")
		if !ok || secret == "" {
			t.Fatalf("FAKE_WEBHOOK_SECRETS holds %q; want vendor=secret", pair)
		}
		out[vendor] = secret
	}
	return out
}

// TestTheWebhookTierServesTheRecipesOnTheStubAgent: each recipe's Binding is served, on its recipe's connection, named as test/e2e names them,
// through the a2a dialect at the stub's webhook agent of its vendor (the stub's -webhook-vendor-api serves agents/<vendor>), with the agent token
// deploy_webhook makes; webhook_binding sets that token, in place of the one a recipe names for its own agent (the GitHub recipe's), and changes
// nothing else of the recipe; and webhook.yaml holds the four AgentBackends alone, beside its Role and RoleBinding.
func TestTheWebhookTierServesTheRecipesOnTheStubAgent(t *testing.T) {
	served := webhookObjects(t)
	token := libVar(t, `printf %s "${WEBHOOK_AGENT_TOKEN}"`)
	for _, vendor := range e2e.WebhookVendors {
		rb, ok := served[e2e.WebhookBinding(vendor)]
		if !ok {
			t.Errorf("no Binding %s is served; served are %v", e2e.WebhookBinding(vendor), slices.Sorted(maps.Keys(served)))
			continue
		}
		if c := rb.Connection; c.Namespace+"/"+c.Name != e2e.WebhookConnection(vendor) || c.Spec.Platform != v1alpha1.PlatformWebhook {
			t.Errorf("the Binding %s is on the %s connection %s/%s; want the webhook connection %s", e2e.WebhookBinding(vendor), c.Spec.Platform, c.Namespace, c.Name, e2e.WebhookConnection(vendor))
		}
		if b := rb.Backend.Spec; b.Dialect != v1alpha1.DialectA2A || b.URL != stubAgentURL+"/agents/agents/"+vendor {
			t.Errorf("the Binding %s reaches %s at %s; want the a2a dialect at the stub's agents/%s", e2e.WebhookBinding(vendor), b.Dialect, b.URL, vendor)
		}
		if ref := rb.Binding.Spec.Identity.TokenSecretRef; ref == nil || ref.Name != token {
			t.Errorf("the Binding %s sends the token of %+v; want the Secret %s, which deploy_webhook makes", e2e.WebhookBinding(vendor), ref, token)
		}

		original := recipeDoc(t, vendor, "binding.yaml")
		got := webhookBinding(t, original)
		for _, doc := range []map[string]any{got, original} {
			identity, _ := doc["spec"].(map[string]any)["identity"].(map[string]any)
			delete(identity, "tokenSecretRef")
		}
		if !reflect.DeepEqual(got, original) {
			t.Errorf("webhook_binding changed more of the %s recipe's Binding than its token:\n%v\nwant\n%v", vendor, got, original)
		}

		// test/e2e names the limits of each of the recipe's triggers when one stops a start.
		triggers := rb.Binding.Spec.Chat.Triggers
		for i, tr := range triggers {
			var got e2e.WebhookLimits
			if r := tr.Limits.RateLimit; r != nil {
				got.MaxRuns, got.Per, got.MaxConcurrent = int(r.MaxRuns), r.Per.Duration, int(r.MaxConcurrent)
			}
			if d := tr.Limits.MaxRunsPerDay; d != nil {
				got.MaxRunsPerDay = int(*d)
			}
			if want, ok := e2e.WebhookTriggerLimits(vendor, i); !ok || got != want {
				t.Errorf("trigger %d of the %s recipe has the limits %+v; test/e2e says %+v (known %v)", i, vendor, got, want, ok)
			}
		}
		if _, ok := e2e.WebhookTriggerLimits(vendor, len(triggers)); ok {
			t.Errorf("test/e2e names the limits of a trigger %d of the %s recipe, which has %d triggers", len(triggers), vendor, len(triggers))
		}

		// The tier's AgentBackend is the recipe's but for its url.
		recipe := recipeDoc(t, vendor, "agentbackend.yaml")
		name := recipe["metadata"].(map[string]any)["name"]
		var tier map[string]any
		for _, doc := range objects(t, webhookManifest) {
			if doc["kind"] == "AgentBackend" && doc["metadata"].(map[string]any)["name"] == name {
				tier = doc
			}
		}
		if tier == nil {
			t.Errorf("%s has no AgentBackend %v in place of the %s recipe's", webhookManifest, name, vendor)
			continue
		}
		for _, doc := range []map[string]any{recipe, tier} {
			delete(doc["spec"].(map[string]any), "url")
		}
		if !reflect.DeepEqual(tier, recipe) {
			t.Errorf("the tier's AgentBackend %v differs from the %s recipe's in more than its url:\n%v\nwant\n%v", name, vendor, tier, recipe)
		}
	}
	var kinds []string
	for _, doc := range objects(t, webhookManifest) {
		kinds = append(kinds, doc["kind"].(string))
	}
	slices.Sort(kinds)
	if want := []string{"AgentBackend", "AgentBackend", "AgentBackend", "AgentBackend", "Role", "RoleBinding"}; !slices.Equal(kinds, want) {
		t.Errorf("%s holds %v; want %v: the connections and Bindings are the recipes' own", webhookManifest, kinds, want)
	}
}

// doublesDeployment returns the Deployment name of doubles.yaml.
func doublesDeployment(t *testing.T, name string) appsv1.Deployment {
	t.Helper()
	for _, doc := range objects(t, doublesFile) {
		if meta, ok := doc["metadata"].(map[string]any); ok && doc["kind"] == "Deployment" && meta["name"] == name {
			b, _ := json.Marshal(doc)
			var d appsv1.Deployment
			if err := json.Unmarshal(b, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("doubles.yaml has no Deployment %s", name)
	return appsv1.Deployment{}
}

// flagValues are the values of each flag of a command written flag, value, flag, value after its binary.
func flagValues(t *testing.T, command []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for i := 1; i < len(command); i++ {
		if !strings.HasPrefix(command[i], "-") || i+1 == len(command) {
			t.Fatalf("the command %q is not flag, value pairs after the binary", command)
		}
		out[command[i]] = append(out[command[i]], command[i+1])
		i++
	}
	return out
}

// TestTheFakeVendorsSignWithTheConnectionsSecrets: the fake vendors' command gives each vendor's connection its route, the agent's own account,
// which the recipe's connection names, and the fake secret that deploy_webhook puts in the Secret the connection names (FAKE_WEBHOOK_SECRETS, a
// fake constant of lib.sh); it sends to the chat Service, the one Slack's HTTP mode sends to, which webhook-values.yaml turns on as
// http-values.yaml does. The stub agent's webhook agents post on the fake vendors' Service, which test/e2e reaches too. The Role grants the
// broker the Secrets the objects name, and deploy_webhook makes each.
func TestTheFakeVendorsSignWithTheConnectionsSecrets(t *testing.T) {
	secrets := fakeWebhookSecrets(t)
	if got := slices.Sorted(maps.Keys(secrets)); !slices.Equal(got, slices.Sorted(slices.Values(e2e.WebhookVendors))) {
		t.Fatalf("FAKE_WEBHOOK_SECRETS has the vendors %v; want %v", got, e2e.WebhookVendors)
	}
	fake := doublesDeployment(t, "fakevendors")
	if c := fake.Spec.Template.Spec.Containers; len(c) != 1 || len(c[0].Command) == 0 || c[0].Command[0] != "/fakevendors" {
		t.Fatalf("the Deployment fakevendors runs %v; want /fakevendors alone", c)
	}
	args := flagValues(t, fake.Spec.Template.Spec.Containers[0].Command)
	request, err := url.Parse(libVar(t, `printf %s "${FAKE_SLACK_REQUEST_URL}"`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := args["-chat-url"], request.Scheme+"://"+request.Host; len(got) != 1 || got[0] != want {
		t.Errorf("the fake vendors send to %v; want %s, the chat Service Slack's HTTP mode sends to", got, want)
	}
	listener := func(file string) any {
		docs := objects(t, file)
		if len(docs) != 1 {
			t.Fatalf("%s holds %d documents; want 1", file, len(docs))
		}
		return docs[0]["chatListener"]
	}
	if got, want := listener(webhookValues), listener("../../hack/kind/http-values.yaml"); !reflect.DeepEqual(got, want) {
		t.Errorf("webhook-values.yaml's chatListener is %v and http-values.yaml's %v; want the same listener and Service", got, want)
	}

	sources := map[string]map[string]string{}
	for _, raw := range args["-source"] {
		src := map[string]string{}
		for _, pair := range strings.Split(raw, ",") {
			k, v, _ := strings.Cut(pair, "=")
			src[k] = v
		}
		sources[src["vendor"]] = src
	}
	if len(sources) != len(e2e.WebhookVendors) || len(args["-source"]) != len(e2e.WebhookVendors) {
		t.Errorf("the fake vendors have the sources %v; want one for each of %v", slices.Sorted(maps.Keys(sources)), e2e.WebhookVendors)
	}
	lib := readHackKind(t, "lib.sh")
	token := libVar(t, `printf %s "${WEBHOOK_AGENT_TOKEN}"`)
	named := []string{token}
	for _, vendor := range e2e.WebhookVendors {
		conn := decodeAs[v1alpha1.ChatConnection](t, recipeDoc(t, vendor, "chatconnection.yaml"))
		src := sources[vendor]
		if src["path"] != e2e.WebhookRoute(vendor) || src["identity"] != conn.Spec.Webhook.Identity || src["secret"] != secrets[vendor] {
			t.Errorf("the fake's %s source is at %q as %q signing with %q; want %s, as %q (the recipe's identity), signing with FAKE_WEBHOOK_SECRETS's %q",
				vendor, src["path"], src["identity"], src["secret"], e2e.WebhookRoute(vendor), conn.Spec.Webhook.Identity, secrets[vendor])
		}
		if want := vendor + "-webhook"; conn.Spec.CredentialsSecretRef.Name != want {
			t.Errorf("the %s recipe's connection reads the Secret %q; deploy_webhook makes %q", vendor, conn.Spec.CredentialsSecretRef.Name, want)
		}
		named = append(named, conn.Spec.CredentialsSecretRef.Name)
	}
	for _, want := range []string{
		`create secret generic "${vendor}-webhook" --from-literal=` + v1alpha1.WebhookSecretKey + `="${pair#*=}"`,
		`create secret generic "${WEBHOOK_AGENT_TOKEN}" --from-literal=token="$(unsigned_jwt "${WEBHOOK_AGENT_EMAIL}")"`,
	} {
		if !strings.Contains(lib, want) {
			t.Errorf("lib.sh's deploy_webhook does not make %q", want)
		}
	}

	stub := doublesDeployment(t, "stubagent")
	if got := flagValues(t, stub.Spec.Template.Spec.Containers[0].Command)["-webhook-vendor-api"]; len(got) != 1 || got[0] != fakeVendorsURL {
		t.Errorf("the stub agent's webhook agents post on %v; want the fake vendors' Service %s", got, fakeVendorsURL)
	}
	if got := e2e.NewEnv(func(string) string { return "" }).VendorsURL; got != fakeVendorsURL {
		t.Errorf("test/e2e reaches the fake vendors at %s; want their Service %s", got, fakeVendorsURL)
	}

	var role rbacv1.Role
	var rb rbacv1.RoleBinding
	load(t, webhookManifest, "Role", &role)
	load(t, webhookManifest, "RoleBinding", &rb)
	slices.Sort(named)
	if len(role.Rules) != 1 || role.Namespace != e2e.WebhookNamespace || !slices.Equal(slices.Sorted(slices.Values(role.Rules[0].ResourceNames)), named) ||
		!slices.Equal(role.Rules[0].Verbs, []string{"list", "watch"}) || !slices.Equal(role.Rules[0].Resources, []string{"secrets"}) {
		t.Errorf("the Role = %s %+v; want list and watch of the Secrets %v by name, in %s", role.Namespace, role.Rules, named, e2e.WebhookNamespace)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != "agent-kourier" || rb.Subjects[0].Namespace != "agent-kourier" || rb.RoleRef.Name != role.Name ||
		rb.Namespace != e2e.WebhookNamespace {
		t.Errorf("the RoleBinding = %+v; want the Role bound to the ServiceAccount agent-kourier/agent-kourier", rb)
	}
}

// TestDeployWebhookAppliesTheRecipes: against stubs, deploy_webhook makes the namespace, each connection's Secret with its fake secret and the
// agent token's Secret, applies webhook.yaml after a dry run by the API server, then each recipe's ChatConnection and its Binding through
// webhook_binding, and asks the broker's metrics whether the four connections are up.
func TestDeployWebhookAppliesTheRecipes(t *testing.T) {
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
	root := filepath.Dir(filepath.Dir(hack))
	script := `set -eu; . "$1"; HERE="$2"; ROOT="$3"; K=(kubectl --context c); deploy_webhook`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", filepath.Join(hack, "lib.sh"), hack, root)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+logs, "STUB_OWNER=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy_webhook = %v\n%s", err, out)
	}
	calls := strings.Split(readLog(t, logs, "calls"), "\n")
	ns := "-n " + e2e.WebhookNamespace + " "
	order := []int{index(calls, "kubectl ", "create namespace "+e2e.WebhookNamespace+" ")}
	for vendor, secret := range fakeWebhookSecrets(t) {
		if index(calls, "kubectl ", ns+"create secret generic "+vendor+"-webhook --from-literal="+v1alpha1.WebhookSecretKey+"="+secret+" ") < 0 {
			t.Errorf("deploy_webhook made no Secret %s-webhook with the fake secret: %q", vendor, calls)
		}
	}
	if index(calls, "kubectl ", ns+"create secret generic "+libVar(t, `printf %s "${WEBHOOK_AGENT_TOKEN}"`)+" --from-literal=token=") < 0 {
		t.Errorf("deploy_webhook made no agent token's Secret: %q", calls)
	}
	order = append(order, index(calls, "kubectl ", "apply --dry-run=server -f "+filepath.Join(hack, "fast/webhook/webhook.yaml")),
		index(calls, "kubectl ", "apply -f "+filepath.Join(hack, "fast/webhook/webhook.yaml")))
	for _, vendor := range e2e.WebhookVendors {
		recipe := filepath.Join(root, "examples/recipes/webhook-"+vendor, "kourier")
		order = append(order, index(calls, "kubectl ", "apply -f "+filepath.Join(recipe, "chatconnection.yaml")),
			index(calls, "kubectl ", "create --dry-run=client -o json -f "+filepath.Join(recipe, "binding.yaml")))
	}
	order = append(order, index(calls, "kubectl --context c -n e2e run probe-webhook-"))
	if slices.Contains(order, -1) || !slices.IsSorted(order) {
		t.Errorf("deploy_webhook's calls are out of order or missing (%v):\n%s", order, strings.Join(calls, "\n"))
	}
	lib := readHackKind(t, "lib.sh")
	if !regexp.MustCompile(`create --dry-run=client -o json -f "\$\{recipe\}/binding.yaml" \| webhook_binding \| "\$\{K\[@\]\}" apply -f -`).MatchString(lib) {
		t.Error("deploy_webhook does not apply each recipe's Binding through webhook_binding")
	}
	if !strings.Contains(lib, `grep -q "^agentkourier_chat_connected{connection=\"$c\"} 1$"`) || !strings.Contains(lib, `sh "${metrics}" "${conns[@]}"`) {
		t.Error("deploy_webhook's probe does not wait for agentkourier_chat_connected of each connection it applied")
	}
}

// TestTheWebhookConnectionsAreServedOnCustomResourcesOnly: webhook-values.yaml admits the recipes' namespace and lets the fake vendors and the
// fake Slack (whose HTTP mode the overlay must not shut out, as it replaces http-values.yaml's list) reach the chat port, by the labels
// doubles.yaml gives their pods; up-fast.sh gives it to the route upgrade of crds alone, unless the tier serves two Slack apps or Microsoft
// Teams (whose admission lists the MC_ and TEAMS_ tests hold), and deploys the recipes then; the runner's Job runs the WH_ tests.
func TestTheWebhookConnectionsAreServedOnCustomResourcesOnly(t *testing.T) {
	var values struct {
		Config struct {
			ConnectionNamespaces []string `json:"connectionNamespaces"`
		} `json:"config"`
		NetworkPolicy struct {
			ChatFrom []struct {
				NamespaceSelector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"namespaceSelector"`
				PodSelector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"podSelector"`
			} `json:"chatFrom"`
		} `json:"networkPolicy"`
	}
	docs := objects(t, webhookValues)
	if len(docs) != 1 {
		t.Fatalf("%s holds %d documents; want 1", webhookValues, len(docs))
	}
	raw, _ := json.Marshal(docs[0])
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(values.Config.ConnectionNamespaces, []string{e2e.WebhookNamespace}) {
		t.Errorf("webhook-values.yaml admits %v; want [%s]", values.Config.ConnectionNamespaces, e2e.WebhookNamespace)
	}
	var from []map[string]string
	for _, f := range values.NetworkPolicy.ChatFrom {
		if !reflect.DeepEqual(f.NamespaceSelector.MatchLabels, map[string]string{"kubernetes.io/metadata.name": "e2e"}) {
			t.Errorf("a chatFrom entry selects the namespaces %v; want e2e, the doubles'", f.NamespaceSelector.MatchLabels)
		}
		from = append(from, f.PodSelector.MatchLabels)
	}
	if want := []map[string]string{doublesDeployment(t, "fakeslack").Spec.Template.Labels, doublesDeployment(t, "fakevendors").Spec.Template.Labels}; !reflect.DeepEqual(from, want) {
		t.Errorf("chatFrom selects the pods %v; want the fake Slack's and the fake vendors' %v", from, want)
	}

	up := readHackKind(t, "up-fast.sh")
	for _, want := range []string{
		"if [ \"${E2E_TWO_APPS}\" = 1 ]; then\n  AGENTKOURIER_CRDS_ARGS=(-f \"${HERE}/two-apps-values.yaml\")\nelif [ \"${E2E_CONFIG_SOURCE}\" = crds ] && [ \"${E2E_TEAMS}\" = 0 ]; then\n  WEBHOOKS=1\n  AGENTKOURIER_CRDS_ARGS=(-f \"${HERE}/webhook-values.yaml\")\nfi",
		"if [ \"${WEBHOOKS}\" = 1 ]; then\n  step ",
		"\n  deploy_webhook\nfi",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up-fast.sh lacks %q", want)
		}
	}
}

// TestTheDoublesImageHoldsEveryDouble: build_doubles_image builds the doubles the Dockerfile copies into the image, and every double's Deployment
// runs one of them; the fake vendors, whose control surface has no authentication, are among the doubles assert_never_published checks.
func TestTheDoublesImageHoldsEveryDouble(t *testing.T) {
	lib := readHackKind(t, "lib.sh")
	built := regexp.MustCompile(`(?m)^  for double in ([a-z ]+); do$`).FindStringSubmatch(lib)
	copied := regexp.MustCompile(`(?m)^COPY ([a-z ]+) /$`).FindStringSubmatch(readHackKind(t, "fast/Dockerfile"))
	if built == nil || copied == nil || built[1] != copied[1] {
		t.Fatalf("build_doubles_image builds %q and the Dockerfile copies %q; want the same doubles", built, copied)
	}
	doubles := strings.Fields(built[1])
	for _, doc := range objects(t, doublesFile) {
		if doc["kind"] != "Deployment" {
			continue
		}
		name := doc["metadata"].(map[string]any)["name"].(string)
		if cmd := doublesDeployment(t, name).Spec.Template.Spec.Containers[0].Command; len(cmd) == 0 || !slices.Contains(doubles, strings.TrimPrefix(cmd[0], "/")) {
			t.Errorf("the Deployment %s runs %v; want one of the doubles the image holds, %v", name, cmd, doubles)
		}
	}
	unpublished := regexp.MustCompile(`(?m)^  for app in ([a-z ]+); do\n    assert_app_unpublished`).FindStringSubmatch(lib)
	if unpublished == nil || !slices.Contains(strings.Fields(unpublished[1]), "fakevendors") {
		t.Errorf("assert_never_published checks %q; want the fake vendors among them", unpublished)
	}
}
