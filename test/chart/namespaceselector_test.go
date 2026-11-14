package chart_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// config.connectionNamespaceSelector (spec D54): with config.source crds, a label selector on namespaces whose ChatConnections the broker admits,
// beside config.connectionNamespaces. The chart passes it to the broker as JSON, grants list and watch on namespaces when it selects by label, and
// with no own connection, no list and no selector the broker serves every namespace. These tests hold the chart to the admission table of D54
// as far as a render can show it.

const (
	ownKey      = "AGENTKOURIER_CHAT_CONNECTION"
	listKey     = "AGENTKOURIER_CONNECTION_NAMESPACES"
	selectorKey = "AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR"
)

// selectorFixture is the selector JSON the chart renders for fixtureSelector, byte for byte. The broker's parser test reads the same file, so the
// two sides agree on the bytes and not only on a description of them. go test ./test/chart -run TestSelectorFixture -update rewrites it.
const selectorFixture = "../../cmd/agent-kourier/testdata/chart-connection-namespace-selector.json"

// fixtureSelector is the selector of selectorFixture: a label to match and an expression with values, in the form a values file gives.
const fixtureSelector = `config.connectionNamespaceSelector={"matchLabels":{"agentkourier.dev/serve":"true"},"matchExpressions":[{"key":"tier","operator":"In","values":["a","b"]}]}`

// selectorArgs is crMode with config.source crds and the image, plus a --set of more (when not empty) and a --set-json of each of setJSON.
func selectorArgs(more string, setJSON ...string) []string {
	a := crMode("--set", "image.repository=agent-kourier,config.source=crds"+more)
	for _, j := range setJSON {
		a = append(a, "--set-json", j)
	}
	return a
}

// clusterRoleRules is the rules of the release's one ClusterRole.
func clusterRoleRules(t *testing.T, objs []object) []rbacv1.PolicyRule {
	t.Helper()
	for _, o := range objs {
		if o.kind() == "ClusterRole" {
			var cr rbacv1.ClusterRole
			find(t, []object{o}, "ClusterRole", o.name(), &cr)
			return cr.Rules
		}
	}
	t.Fatalf("no ClusterRole in %v", kinds(objs))
	return nil
}

// namespacesRule is the one rule the selector adds to the ClusterRole: the informer on namespaces lists and watches them, and never gets one.
var namespacesRule = rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"list", "watch"}}

// TestAdmissionTable: each row of D54's admission table that the chart can express, by what it renders: the variables the broker reads its
// admission from, right after AGENTKOURIER_CONFIG_SOURCE in the order own, list, selector, and the ClusterRole's rule on namespaces, which a
// selector that selects by label needs and nothing else does. The selector is compact JSON; {} is a value of its own (every namespace) and is
// rendered, not dropped as an empty map would be by a template's truth test.
func TestAdmissionTable(t *testing.T) {
	t.Parallel()
	helmPath(t)
	const own = ",config.chatConnection=platform/slack"
	list := `config.connectionNamespaces=["team-a","team-b"]`
	sel := `config.connectionNamespaceSelector={"matchLabels":{"agentkourier.dev/serve":"true"}}`
	all := `config.connectionNamespaceSelector={}`
	const selJSON = `{"matchLabels":{"agentkourier.dev/serve":"true"}}`
	tests := []struct {
		name string
		set  string
		json []string
		env  map[string]string // the admission variables the pod gets; any of the three not here must be absent
		rule bool
	}{
		{"no own, no list, no selector: every namespace", "", nil, map[string]string{}, false},
		{"no own, no list, {}: every namespace", "", []string{all}, map[string]string{selectorKey: "{}"}, false},
		{"no own, a list", "", []string{list}, map[string]string{listKey: "team-a,team-b"}, false},
		{"no own, a selector", "", []string{sel}, map[string]string{selectorKey: selJSON}, true},
		{"no own, a list and a selector", "", []string{list, sel}, map[string]string{listKey: "team-a,team-b", selectorKey: selJSON}, true},
		{"no own, a list and {}", "", []string{list, all}, map[string]string{listKey: "team-a,team-b", selectorKey: "{}"}, false},
		{"own alone", own, nil, map[string]string{ownKey: "platform/slack"}, false},
		{"own and a list", own, []string{list}, map[string]string{ownKey: "platform/slack", listKey: "team-a,team-b"}, false},
		{"own and a selector", own, []string{sel}, map[string]string{ownKey: "platform/slack", selectorKey: selJSON}, true},
		{"own, a list and a selector", own, []string{list, sel}, map[string]string{ownKey: "platform/slack", listKey: "team-a,team-b", selectorKey: selJSON}, true},
		{"own and {}", own, []string{all}, map[string]string{ownKey: "platform/slack", selectorKey: "{}"}, false},
		{"own, a list and {}", own, []string{list, all}, map[string]string{ownKey: "platform/slack", listKey: "team-a,team-b", selectorKey: "{}"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, "agent-kourier", "rel-ns", selectorArgs(tc.set, tc.json...)...)
			d := deployment(t, objs)
			env := podEnv(d)
			wantNames := []string{"AGENTKOURIER_CONFIG_SOURCE"}
			for _, key := range []string{ownKey, listKey, selectorKey} {
				want, wanted := tc.env[key]
				got, has := env[key]
				switch {
				case wanted && !has:
					t.Errorf("%s is absent; want %q", key, want)
				case !wanted && has:
					t.Errorf("%s = %q; want it absent", key, got)
				case wanted && got != want:
					t.Errorf("%s = %q; want %q", key, got, want)
				}
				if wanted {
					wantNames = append(wantNames, key)
				}
			}
			var names []string
			for _, e := range d.Spec.Template.Spec.Containers[0].Env {
				names = append(names, e.Name)
			}
			if len(names) < len(wantNames) || !slices.Equal(names[:len(wantNames)], wantNames) {
				t.Errorf("environment %v; want it to begin %v", names, wantNames)
			}
			rules := clusterRoleRules(t, objs)
			if got := slices.ContainsFunc(rules, func(r rbacv1.PolicyRule) bool { return slices.Contains(r.Resources, "namespaces") }); got != tc.rule {
				t.Errorf("the ClusterRole has a rule on namespaces: %v; want %v (rules %+v)", got, tc.rule, rules)
			}
		})
	}
}

// TestDefaultsWithNoConfigServeEveryNamespace: a first install that names no connection, no namespace and no selector is the broker that serves the
// ChatConnections of every namespace. It renders, with none of the three admission variables, no rule on namespaces (the broker runs no namespace
// informer then) and no Secret Role: each team grants its own.
func TestDefaultsWithNoConfigServeEveryNamespace(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", "--kube-version", "v1.33.2", "--set", "image.repository=agent-kourier")
	env := podEnv(deployment(t, objs))
	if env["AGENTKOURIER_CONFIG_SOURCE"] != "crds" {
		t.Errorf("AGENTKOURIER_CONFIG_SOURCE %q; want crds", env["AGENTKOURIER_CONFIG_SOURCE"])
	}
	for _, key := range []string{ownKey, listKey, selectorKey} {
		if v, has := env[key]; has {
			t.Errorf("%s = %q; want it absent: nothing set is every namespace", key, v)
		}
	}
	if rules := clusterRoleRules(t, objs); slices.ContainsFunc(rules, func(r rbacv1.PolicyRule) bool { return slices.Contains(r.Resources, "namespaces") }) {
		t.Errorf("ClusterRole rules %+v; want no rule on namespaces with no selector", rules)
	}
	if got := secretRoles(t, objs); len(got) != 0 {
		t.Errorf("Secret Roles %v; want none (each team grants its own)", got)
	}
}

// TestSelectorInValuesFiles: Helm merges maps across values files, so a selector in a later -f file does not replace an earlier one: {}
// leaves it as it was, and another selector is merged into it, which ANDs the two (a list, matchExpressions, is replaced). A file that sets
// it to null before the new one clears it, and --set-json replaces it. The values.yaml comment and the docs say so; this pins it. With nothing earlier, one file's {} is {}.
func TestSelectorInValuesFiles(t *testing.T) {
	t.Parallel()
	helmPath(t)
	dir := t.TempDir()
	file := func(name, body string) string {
		p := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	teamA := file("team-a", "config:\n  connectionNamespaceSelector:\n    matchLabels: {team: a}\n")
	everything := file("everything", "config:\n  connectionNamespaceSelector: {}\n")
	tierB := file("tier-b", "config:\n  connectionNamespaceSelector:\n    matchLabels: {tier: b}\n")
	cleared := file("cleared", "config:\n  connectionNamespaceSelector: null\n")
	tierExists := file("tier-exists", "config:\n  connectionNamespaceSelector:\n    matchExpressions: [{key: tier, operator: Exists}]\n")
	noTeam := file("no-team", "config:\n  connectionNamespaceSelector:\n    matchExpressions: [{key: team, operator: DoesNotExist}]\n")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"{} in one file over the default", []string{"-f", everything}, `{}`},
		{"{} in a later file keeps the earlier selector", []string{"-f", teamA, "-f", everything}, `{"matchLabels":{"team":"a"}}`},
		{"a selector in a later file is merged into the earlier one", []string{"-f", teamA, "-f", tierB}, `{"matchLabels":{"team":"a","tier":"b"}}`},
		{"matchExpressions in a later file replaces the earlier list", []string{"-f", tierExists, "-f", noTeam}, `{"matchExpressions":[{"key":"team","operator":"DoesNotExist"}]}`},
		{"null in a file between them clears the earlier selector", []string{"-f", teamA, "-f", cleared, "-f", tierB}, `{"matchLabels":{"tier":"b"}}`},
		{"null in a file between them, then {}", []string{"-f", teamA, "-f", cleared, "-f", everything}, `{}`},
		{"--set-json {} replaces a file's selector", []string{"-f", teamA, "--set-json", "config.connectionNamespaceSelector={}"}, `{}`},
		{"--set-json replaces a file's selector", []string{"-f", teamA, "--set-json", `config.connectionNamespaceSelector={"matchLabels":{"tier":"b"}}`}, `{"matchLabels":{"tier":"b"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := append(crMode("--set", "image.repository=agent-kourier,config.source=crds"), tc.args...)
			got, has := podEnv(deployment(t, mustRender(t, "agent-kourier", "rel-ns", args...)))[selectorKey]
			if !has || got != tc.want {
				t.Errorf("%s = %q (set: %v); want %s", selectorKey, got, has, tc.want)
			}
		})
	}
}

// TestNullChatConnectionsServeEveryNamespace: config.chatConnections set to null, as --set config.chatConnections=null does, is no entry: with
// nothing else set the broker serves every namespace, as with the default {}, and the render is the default's.
func TestNullChatConnectionsServeEveryNamespace(t *testing.T) {
	t.Parallel()
	helmPath(t)
	base := []string{"--set", "image.repository=agent-kourier"}
	want, err := render(t.Context(), t, "agent-kourier", "agent-kourier", base...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := render(t.Context(), t, "agent-kourier", "agent-kourier", append(slices.Clone(base), "--set", "config.chatConnections=null")...)
	if err != nil {
		t.Fatalf("config.chatConnections=null: %v; want it to render every namespace", err)
	}
	if got != want {
		t.Errorf("config.chatConnections=null changes the render:\n%s", firstDifference(want, got))
	}
}

// TestSelectorChangesOnlyItsVariableAndTheRule: beside an own connection, {} adds its variable and nothing else, and a selector by label adds its
// variable and the one rule on namespaces, at the end of the ClusterRole: list and watch, never get, and no resourceNames (an informer lists the
// collection). The rest of the render is the unset render's, so the Secret Roles do not move.
func TestSelectorChangesOnlyItsVariableAndTheRule(t *testing.T) {
	t.Parallel()
	helmPath(t)
	base := selectorArgs(",config.chatConnection=agent-kourier/slack-main")
	renderWith := func(more ...string) string {
		t.Helper()
		out, err := render(t.Context(), t, "agent-kourier", "agent-kourier", append(slices.Clone(base), more...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	unset := renderWith()
	if strings.Contains(unset, selectorKey) {
		t.Fatalf("%s is in the render with the selector unset; want it absent", selectorKey)
	}
	if nulled := renderWith("--set", "config.connectionNamespaceSelector=null"); nulled != unset {
		t.Errorf("a null selector changes the render:\n%s", firstDifference(unset, nulled))
	}
	without := func(out string, drop func(lines []string, i int) int) string {
		lines := strings.Split(out, "\n")
		var kept []string
		for i := 0; i < len(lines); i++ {
			if n := drop(lines, i); n > 0 {
				i += n - 1
				continue
			}
			kept = append(kept, lines[i])
		}
		return strings.Join(kept, "\n")
	}
	envLine := func(lines []string, i int) int {
		if strings.Contains(lines[i], selectorKey) {
			return 1
		}
		return 0
	}

	everything := renderWith("--set-json", "config.connectionNamespaceSelector={}")
	if got := without(everything, envLine); got != unset {
		t.Errorf("{} changes more than its variable:\n%s", firstDifference(unset, got))
	}

	labelled := renderWith("--set-json", `config.connectionNamespaceSelector={"matchLabels":{"team":"yes"}}`)
	objs := decode(t, labelled)
	rules, unsetRules := clusterRoleRules(t, objs), clusterRoleRules(t, decode(t, unset))
	if want := append(slices.Clone(unsetRules), namespacesRule); !equality.Semantic.DeepEqual(rules, want) {
		t.Errorf("ClusterRole rules %+v; want the unset render's and then %+v", rules, namespacesRule)
	}
	ruleLines := func(lines []string, i int) int {
		if n := envLine(lines, i); n > 0 {
			return n
		}
		if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "resources: [namespaces]" {
			return 3
		}
		return 0
	}
	if got := without(labelled, ruleLines); got != unset {
		t.Errorf("a selector changes more than its variable and the rule on namespaces:\n%s", firstDifference(unset, got))
	}

	for _, o := range mustRender(t, "agent-kourier", "agent-kourier", append(slices.Clone(base), "--set", "rbac.create=false", "--set-json", `config.connectionNamespaceSelector={"matchLabels":{"team":"yes"}}`)...) {
		if strings.HasPrefix(o.kind(), "ClusterRole") {
			t.Errorf("rbac.create=false with a selector rendered %s %s", o.kind(), o.name())
		}
	}
}

// TestTheSoleEntryIsTheOwnConnectionOnlyWithNothingElseSet: with config.chatConnection empty, the one entry of config.chatConnections is the own
// connection only when the list and the selector are both unset. With a selector, {} included, the entries are team connections: no own
// variable, and none of them is checked against the namespaces a selector admits, since the chart cannot see a namespace's labels. Two entries
// render beside a selector, too.
func TestTheSoleEntryIsTheOwnConnectionOnlyWithNothingElseSet(t *testing.T) {
	t.Parallel()
	helmPath(t)
	two := `config.chatConnections={"a":{"namespace":"team-a","spec":{"platform":"slack","credentialsSecretRef":{"name":"a"}}},"b":{"namespace":"team-b","spec":{"platform":"slack","credentialsSecretRef":{"name":"b"}}}}`
	sel := `config.connectionNamespaceSelector={"matchLabels":{"team":"yes"}}`
	all := `config.connectionNamespaceSelector={}`
	list := `config.connectionNamespaces=["team-a"]`
	tests := []struct {
		name string
		json []string
		own  string // empty: no own connection
	}{
		{"one entry and nothing else: the own connection", []string{entryJSON("platform")}, "platform/slack-main"},
		{"one entry and a selector: a team connection", []string{entryJSON("platform"), sel}, ""},
		{"one entry and {}: a team connection", []string{entryJSON("platform"), all}, ""},
		{"one entry outside the list, with a selector too: not refused", []string{entryJSON("platform"), list, sel}, ""},
		{"two entries and a selector", []string{two, sel}, ""},
		{"two entries and {}", []string{two, all}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, "agent-kourier", "rel-ns", selectorArgs("", tc.json...)...)
			got, has := podEnv(deployment(t, objs))[ownKey]
			switch {
			case tc.own == "" && has:
				t.Errorf("%s = %q; want it absent: with a selector the entries are team connections", ownKey, got)
			case tc.own != "" && got != tc.own:
				t.Errorf("%s = %q; want %q", ownKey, got, tc.own)
			}
		})
	}
}

// TestNamespaceSelectorRefusals: what the chart refuses about the selector and the admission around it, and the near cases it renders.
func TestNamespaceSelectorRefusals(t *testing.T) {
	t.Parallel()
	helmPath(t)
	sel := `config.connectionNamespaceSelector={"matchLabels":{"team":"yes"}}`
	all := `config.connectionNamespaceSelector={}`
	two := `config.chatConnections={"a":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"a"}}},"b":{"spec":{"platform":"slack","credentialsSecretRef":{"name":"b"}}}}`
	const noOwn = "names the Secret of the broker's own ChatConnection, and there is none"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		// The files route serves the one ChatConnection of its directory: a selector there would silently stop serving the ones it selects.
		{"a selector with files", []string{"-f", values("minimal"), "--set", "config.source=files", "--set-json", sel},
			[]string{"config.connectionNamespaceSelector is for config.source crds", "config.source: files", "null"}},
		{"{} with files", []string{"-f", values("minimal"), "--set", "config.source=files", "--set-json", all},
			[]string{"config.connectionNamespaceSelector is for config.source crds", "config.source: files"}},
		{"a selector with config.source nulled, which reads as files", []string{"-f", values("minimal"), "--set", "config.source=null", "--set-json", sel},
			[]string{"config.connectionNamespaceSelector is for config.source crds"}},
		// Two entries with nothing else set: no guess at which is the own connection, and the message names every way out.
		{"two entries and nothing else", selectorArgs("", two),
			[]string{"needs config.chatConnection", "config.connectionNamespaces", "config.connectionNamespaceSelector", "every namespace", "config.source: files"}},
		{"two entries and an empty list", selectorArgs("", two, "config.connectionNamespaces=[]"),
			[]string{"needs config.chatConnection", "config.connectionNamespaceSelector"}},
		// config.chatConnectionSecret names an own connection's Secret, and with no own connection there is none to name, however the broker admits.
		{"chatConnectionSecret with a list", selectorArgs(",config.chatConnectionSecret=slack-bot", `config.connectionNamespaces=["team-a"]`), []string{noOwn}},
		{"chatConnectionSecret with a selector", selectorArgs(",config.chatConnectionSecret=slack-bot", sel), []string{noOwn}},
		{"chatConnectionSecret with {}", selectorArgs(",config.chatConnectionSecret=slack-bot", all), []string{noOwn}},
		{"chatConnectionSecret with nothing set, which is every namespace", selectorArgs(",config.chatConnectionSecret=slack-bot"), []string{noOwn, "every namespace"}},
		// With only the list, a rendered entry outside it would be rendered and never served.
		{"an entry outside the list, with no selector", selectorArgs("", entryJSON("platform"), `config.connectionNamespaces=["team-a"]`),
			[]string{`"slack-main"`, `namespace "platform"`, "never serve"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "rel-ns", tc.args...)
			if err == nil {
				t.Fatal("rendered; want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("render error = %v; want one containing %q", err, want)
				}
			}
		})
	}

	renders := []struct {
		name string
		args []string
	}{
		{"files with the selector null", []string{"-f", values("minimal"), "--set", "config.source=files,config.connectionNamespaceSelector=null"}},
		{"chatConnectionSecret with an own connection and {}", selectorArgs(",config.chatConnection=platform/slack,config.chatConnectionSecret=slack-bot", all)},
		{"chatConnectionSecret with an own connection and a selector", selectorArgs(",config.chatConnection=platform/slack,config.chatConnectionSecret=slack-bot", sel)},
	}
	for _, tc := range renders {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := render(t.Context(), t, "agent-kourier", "rel-ns", tc.args...); err != nil {
				t.Errorf("%v; want it to render", err)
			}
		})
	}
}

// TestNamespaceSelectorSchema: the value is a Kubernetes LabelSelector and nothing else: matchLabels of label keys to label values, and
// matchExpressions of key, operator (In, NotIn, Exists, DoesNotExist: the operators of a label selector, not a node selector's Gt and Lt) and
// values, which In and NotIn need and Exists and DoesNotExist refuse. An empty matchLabels or matchExpressions is refused: {} is the one way to
// select every namespace, so the chart and the broker cannot disagree on what selects everything. null is unset.
func TestNamespaceSelectorSchema(t *testing.T) {
	t.Parallel()
	helmPath(t)
	name63 := strings.Repeat("a", 63)
	for _, tc := range []struct {
		name string
		json string
		ok   bool
	}{
		{"null", `null`, true},
		{"{}", `{}`, true},
		{"a label", `{"matchLabels":{"team":"yes"}}`, true},
		{"a prefixed key", `{"matchLabels":{"agentkourier.dev/serve":"true"}}`, true},
		{"an empty label value", `{"matchLabels":{"team":""}}`, true},
		{"a 63-character value", `{"matchLabels":{"team":"` + name63 + `"}}`, true},
		{"In", `{"matchExpressions":[{"key":"tier","operator":"In","values":["a","b"]}]}`, true},
		{"NotIn", `{"matchExpressions":[{"key":"tier","operator":"NotIn","values":["c"]}]}`, true},
		{"Exists", `{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"Exists"}]}`, true},
		{"DoesNotExist with no values", `{"matchExpressions":[{"key":"tier","operator":"DoesNotExist","values":[]}]}`, true},
		{"both", `{"matchLabels":{"team":"yes"},"matchExpressions":[{"key":"tier","operator":"Exists"}]}`, true},
		{"a string", `"team=yes"`, false},
		{"a list", `[]`, false},
		{"a misspelt field", `{"matchLabel":{"team":"yes"}}`, false},
		{"an empty matchLabels", `{"matchLabels":{}}`, false},
		{"an empty matchExpressions", `{"matchExpressions":[]}`, false},
		{"a number as a label value", `{"matchLabels":{"team":1}}`, false},
		{"a key with a space", `{"matchLabels":{"my team":"yes"}}`, false},
		{"a 64-character value", `{"matchLabels":{"team":"` + name63 + `a"}}`, false},
		{"a value that ends in a dot", `{"matchLabels":{"team":"yes."}}`, false},
		{"a lower-case operator", `{"matchExpressions":[{"key":"tier","operator":"in","values":["a"]}]}`, false},
		{"Gt, a node selector's", `{"matchExpressions":[{"key":"tier","operator":"Gt","values":["1"]}]}`, false},
		{"In with no values", `{"matchExpressions":[{"key":"tier","operator":"In"}]}`, false},
		{"In with an empty list", `{"matchExpressions":[{"key":"tier","operator":"In","values":[]}]}`, false},
		{"Exists with values", `{"matchExpressions":[{"key":"tier","operator":"Exists","values":["a"]}]}`, false},
		{"no key", `{"matchExpressions":[{"operator":"Exists"}]}`, false},
		{"no operator", `{"matchExpressions":[{"key":"tier"}]}`, false},
		{"a misspelt expression field", `{"matchExpressions":[{"key":"tier","operator":"In","value":["a"]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", selectorArgs(",config.chatConnection=agent-kourier/slack-main", "config.connectionNamespaceSelector="+tc.json)...)
			switch {
			case tc.ok && err != nil:
				t.Errorf("%s: %v; want it to render", tc.json, err)
			// The error repeats helm's arguments, which name the value, so the check is on the path the schema reports.
			case !tc.ok && (err == nil || !strings.Contains(err.Error(), "at '/config/connectionNamespaceSelector")):
				t.Errorf("%s: render error = %v; want the schema to refuse config.connectionNamespaceSelector", tc.json, err)
			}
		})
	}
}

// TestSelectorFixture: the variable the chart renders for fixtureSelector is the fixture's bytes: compact JSON, keys sorted, no trailing newline.
// The broker's test of the variable reads the fixture, so a change to how the chart encodes the selector fails here first.
func TestSelectorFixture(t *testing.T) {
	t.Parallel()
	helmPath(t)
	got, has := podEnv(deployment(t, mustRender(t, "agent-kourier", "rel-ns", selectorArgs("", fixtureSelector)...)))[selectorKey]
	if !has {
		t.Fatalf("%s is absent; want the selector", selectorKey)
	}
	if *update {
		if err := os.WriteFile(selectorFixture, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(selectorFixture)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s = %s; want the fixture's bytes %s (go test ./test/chart -run TestSelectorFixture -update rewrites it)", selectorKey, got, want)
	}
	var decoded, given map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("%s is not JSON: %v", selectorKey, err)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(fixtureSelector, "config.connectionNamespaceSelector=")), &given); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(decoded, given) {
		t.Errorf("%s decodes to %v; want the selector given, %v", selectorKey, decoded, given)
	}
}
