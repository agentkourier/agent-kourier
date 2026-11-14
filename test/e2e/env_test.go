package e2e

import (
	"strings"
	"testing"
)

func TestNewEnv(t *testing.T) {
	vars := map[string]string{"E2E_SLACK_URL": "http://s", "E2E_SINK_URL": "http://k", "E2E_STUB_URL": "http://u", "E2E_ALERTMANAGER_URL": "http://a", "E2E_SCENARIO_MCP_URL": "http://m", "E2E_AGENTKOURIER_URL": "http://c", "E2E_SIGNALER_URL": "http://g", "E2E_TIER": "full", "E2E_CONFIG_SOURCE": "crds", "E2E_PHASE": "open",
		"E2E_CHAT_CONNECTION": "agent-kourier/slack-fast", "E2E_CONNECTION_NAMESPACES": "team-a,team-b", "E2E_CONNECTION_NAMESPACE_SELECTOR": `{"matchLabels":{"agentkourier.dev/serve":"true"}}`, "E2E_NAMESPACES": `{"items":[]}`}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want Env
	}{
		{"defaults are the fast tier's Services", nil, Env{
			SlackURL:        "http://fakeslack.e2e.svc.cluster.local:8080",
			SinkURL:         "http://sink.e2e.svc.cluster.local:8080",
			StubURL:         "http://stubagent.e2e.svc.cluster.local:8080",
			AlertmanagerURL: "http://alertmanager.monitoring.svc.cluster.local:9093",
			ScenarioMCPURL:  "http://scenariomcp.kagent.svc.cluster.local:8082",
			AgentKourierURL: "http://agent-kourier.agent-kourier.svc.cluster.local:8080",
			SignalerURL:     "http://signaler.e2e.svc.cluster.local:8080",
			ConfigSource:    "files",
		}},
		{"every variable overrides", vars, Env{SlackURL: "http://s", SinkURL: "http://k", StubURL: "http://u", AlertmanagerURL: "http://a", ScenarioMCPURL: "http://m", AgentKourierURL: "http://c", SignalerURL: "http://g", Tier: "full", ConfigSource: "crds", Phase: "open",
			ChatConnection: "agent-kourier/slack-fast", ConnectionNamespaces: "team-a,team-b", ConnectionNamespaceSelector: `{"matchLabels":{"agentkourier.dev/serve":"true"}}`, Namespaces: `{"items":[]}`}},
	} {
		if got := NewEnv(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("%s: NewEnv() = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// testNamespaces is what kubectl get namespace -o json prints for team-a, labelled to be served, and team-b, with no label: the fields Admits does not
// read are there as kubectl prints them.
const testNamespaces = `{"apiVersion":"v1","kind":"List","items":[
 {"apiVersion":"v1","kind":"Namespace","metadata":{"name":"team-a","uid":"1","labels":{"agentkourier.dev/serve":"true","kubernetes.io/metadata.name":"team-a"}},"spec":{"finalizers":["kubernetes"]},"status":{"phase":"Active"}},
 {"apiVersion":"v1","kind":"Namespace","metadata":{"name":"team-b","uid":"2","labels":{"kubernetes.io/metadata.name":"team-b"}},"status":{"phase":"Active"}}],
 "metadata":{"resourceVersion":""}}`

func TestEnvAdmits(t *testing.T) {
	const (
		serve = `{"matchLabels":{"agentkourier.dev/serve":"true"}}`
		own   = "agent-kourier/slack-fast"
	)
	for _, tc := range []struct {
		name string
		env  Env
		want map[string]bool
	}{
		{"the list admits its namespaces and no other", Env{ConfigSource: "crds", ChatConnection: own, ConnectionNamespaces: "team-a,team-b"},
			map[string]bool{"team-a": true, "team-b": true, "team-c": false, "team": false, "": false, "team-a,team-b": false}},
		{"the list with no own connection admits its namespaces and no other", Env{ConfigSource: "crds", ConnectionNamespaces: "team-a"},
			map[string]bool{"team-a": true, "team-b": false}},
		{"an own connection alone admits no namespace", Env{ConfigSource: "crds", ChatConnection: own},
			map[string]bool{"team-a": false, "agent-kourier": false}},
		{"nothing set is every namespace", Env{ConfigSource: "crds"},
			map[string]bool{"team-a": true, "agent-kourier": true, "anywhere": true, "": false}},
		{"a selector admits the namespaces whose labels match it", Env{ConfigSource: "crds", ChatConnection: own, ConnectionNamespaceSelector: serve, Namespaces: testNamespaces},
			map[string]bool{"team-a": true, "team-b": false}},
		{"a selector with no own connection admits the same", Env{ConfigSource: "crds", ConnectionNamespaceSelector: serve, Namespaces: testNamespaces},
			map[string]bool{"team-a": true, "team-b": false}},
		{"the list and a selector admit the namespaces of either", Env{ConfigSource: "crds", ConnectionNamespaces: "team-b", ConnectionNamespaceSelector: serve, Namespaces: testNamespaces},
			map[string]bool{"team-a": true, "team-b": true}},
		{"matchExpressions are read too", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"matchExpressions":[{"key":"agentkourier.dev/serve","operator":"DoesNotExist"}]}`, Namespaces: testNamespaces},
			map[string]bool{"team-a": false, "team-b": true}},
		{"the empty selector is every namespace, with no labels read", Env{ConfigSource: "crds", ConnectionNamespaceSelector: "{}"},
			map[string]bool{"team-a": true, "agent-kourier": true, "anywhere": true}},
		{"the empty selector beside an own connection is every namespace too", Env{ConfigSource: "crds", ChatConnection: own, ConnectionNamespaceSelector: "{}"},
			map[string]bool{"team-a": true, "agent-kourier": true}},
		{"files admits nothing, whatever is set", Env{ConfigSource: "files", ConnectionNamespaces: "team-a", ConnectionNamespaceSelector: "{}"},
			map[string]bool{"team-a": false}},
		{"files with nothing set admits nothing: it serves the one connection of its directory", Env{ConfigSource: "files"},
			map[string]bool{"team-a": false}},
	} {
		for ns, want := range tc.want {
			got, err := tc.env.Admits(ns)
			if err != nil || got != want {
				t.Errorf("%s: Admits(%q) = %v, %v; want %v (%s)", tc.name, ns, got, err, want, tc.env.Admission())
			}
		}
	}
}

func TestEnvAdmitsFailsOnWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  Env
		ns   string
		want string
	}{
		{"a selector that is not JSON", Env{ConfigSource: "crds", ConnectionNamespaceSelector: "agentkourier.dev/serve=true", Namespaces: testNamespaces}, "team-a", "E2E_CONNECTION_NAMESPACE_SELECTOR"},
		{"a selector with a field a LabelSelector does not have", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"matchLabel":{"a":"b"}}`, Namespaces: testNamespaces}, "team-a", "E2E_CONNECTION_NAMESPACE_SELECTOR"},
		{"a selector whose field names are not the API's case", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"MatchLabels":{"a":"b"}}`, Namespaces: testNamespaces}, "team-a", "E2E_CONNECTION_NAMESPACE_SELECTOR"},
		{"a selector with a field twice", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"matchLabels":{"a":"b"},"matchLabels":{}}`, Namespaces: testNamespaces}, "team-a", "E2E_CONNECTION_NAMESPACE_SELECTOR"},
		{"a selector with an invalid label key", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"matchLabels":{"no spaces":"b"}}`, Namespaces: testNamespaces}, "team-a", "E2E_CONNECTION_NAMESPACE_SELECTOR"},
		{"a selector with an operator Kubernetes does not have", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"matchExpressions":[{"key":"a","operator":"Is"}]}`, Namespaces: testNamespaces}, "team-a", "E2E_CONNECTION_NAMESPACE_SELECTOR"},
		{"a selector that is null", Env{ConfigSource: "crds", ConnectionNamespaceSelector: "null", Namespaces: testNamespaces}, "team-a", "E2E_CONNECTION_NAMESPACE_SELECTOR"},
		{"a selector that is a list", Env{ConfigSource: "crds", ConnectionNamespaceSelector: "[]", Namespaces: testNamespaces}, "team-a", "E2E_CONNECTION_NAMESPACE_SELECTOR"},
		{"a selector and no namespaces", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"matchLabels":{"a":"b"}}`}, "team-a", "E2E_NAMESPACES"},
		{"namespaces that are not JSON", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"matchLabels":{"a":"b"}}`, Namespaces: "team-a"}, "team-a", "E2E_NAMESPACES"},
		{"a namespace the run did not read", Env{ConfigSource: "crds", ConnectionNamespaceSelector: `{"matchLabels":{"a":"b"}}`, Namespaces: testNamespaces}, "team-c", "team-c"},
	} {
		got, err := tc.env.Admits(tc.ns)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Admits(%q) = %v, %v; want an error that names %s", tc.name, tc.ns, got, err, tc.want)
		}
	}
	// The selector is read before the list is looked at: a namespace on the list does not let a selector that cannot be read pass.
	if got, err := (Env{ConfigSource: "crds", ConnectionNamespaces: "team-a", ConnectionNamespaceSelector: "{"}).Admits("team-a"); err == nil {
		t.Errorf("Admits(team-a) on the list with a selector that is not JSON = %v, nil; want an error", got)
	}
}

func TestEnvAdmissionSaysWhatIsSet(t *testing.T) {
	for _, tc := range []struct {
		env  Env
		want string
	}{
		{Env{ConfigSource: "files"}, "config source files: the one connection of its directory"},
		{Env{ConfigSource: "crds"}, "no own connection, no list, no selector: every namespace"},
		{Env{ConfigSource: "crds", ChatConnection: "agent-kourier/slack-fast", ConnectionNamespaces: "team-a", ConnectionNamespaceSelector: "{}"},
			`own connection agent-kourier/slack-fast, list "team-a", selector {}`},
	} {
		if got := tc.env.Admission(); got != tc.want {
			t.Errorf("Admission() of %+v = %q; want %q", tc.env, got, tc.want)
		}
	}
}
