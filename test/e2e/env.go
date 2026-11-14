// Package e2e holds the helpers end-to-end scenarios use to drive and observe the test
// environment (spec 12.1). They are plain HTTP callers, unit-tested against in-process servers,
// so `make test` runs them. Scenarios that need a cluster are tagged `e2e` and run in-cluster,
// reaching the doubles by Service name: the fake Slack's control surface is unauthenticated, so
// nothing is published to the host.
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kjson "sigs.k8s.io/json"
)

// Env locates the test doubles; the defaults are the fast tier's Service names.
type Env struct {
	SlackURL        string // the fake Slack: Web API and /_control/
	SinkURL         string // the sink receiver
	StubURL         string // the stub agent: /agents/ and /_control/release/
	AlertmanagerURL string // Alertmanager's API
	// ScenarioMCPURL is the full tier's scripted MCP server control surface. It is
	// kept in Env so a scenario can read execution counters without coupling the
	// test to a cluster-local hostname.
	ScenarioMCPURL string
	// AgentKourierURL is Agent Kourier's health and metrics port: the fast tier's Service of that port alone
	// (the chart's Service, with charts/agent-kourier/ci/fast-values.yaml).
	AgentKourierURL string
	// SignalerURL is the container signaler (test/signaler), which the fast tier brings up with two Agent Kourier replicas
	// and the failover scenario uses to freeze or kill a replica's process.
	SignalerURL string
	// Tier is the tier the runner is in, "fast" or "full", from E2E_TIER (hack/kind/e2e-run.sh sets it). It
	// has no default: a test that needs it must fail when it is missing, not guess.
	Tier string
	// ConfigSource is where the tier's Agent Kourier reads its config from, "files" (the ConfigMap) or "crds" (custom resources), from
	// E2E_CONFIG_SOURCE (hack/kind/e2e-run.sh reads it from the Deployment). The scenarios of the custom resources route skip unless it is crds.
	ConfigSource string
	// Phase is the part of a scenario that a script runs between helm upgrades, from E2E_PHASE: empty in an ordinary run, in which such a
	// scenario skips (hack/kind/crs-migration.sh sets it).
	Phase string
	// The broker's admission (spec D54), as its Deployment has it when the run starts: hack/kind/e2e-run.sh reads the variables of the Deployment and
	// hands them to the Job through the ConfigMap e2e-admission (lib.sh's runner_admission), never through the Job's YAML, since a selector holds
	// quotes, braces and slashes. Admits reads them; the scenarios of several Slack apps skip unless it admits team-a.
	//
	// ChatConnection is the broker's own connection, from E2E_CHAT_CONNECTION (AGENTKOURIER_CHAT_CONNECTION): namespace/name, empty for none.
	ChatConnection string
	// ConnectionNamespaces is the platform's admission list, from E2E_CONNECTION_NAMESPACES (AGENTKOURIER_CONNECTION_NAMESPACES): namespace names,
	// comma separated, empty for none.
	ConnectionNamespaces string
	// ConnectionNamespaceSelector is the admission selector, from E2E_CONNECTION_NAMESPACE_SELECTOR (AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR): a
	// metav1.LabelSelector as JSON, empty for none; {} is every namespace.
	ConnectionNamespaceSelector string
	// Namespaces is the namespaces the scenarios ask Admits about, with their labels as they were when the run started, from E2E_NAMESPACES: what
	// kubectl get namespace -o json prints. Only a selector that reads labels reads it.
	Namespaces string
}

// Admits says whether the broker serves the ChatConnections of namespace besides its own connection (spec D54). On the custom resources route it
// does when the namespace is on the list, or its labels match the selector, or the selector is {}, or nothing is set (no own connection, no list
// and no selector), which is every namespace. On the files route it serves the one connection of its directory and admits none. The selector is
// decoded as the broker decodes it (one JSON object, field names case-sensitive, no unknown or repeated field) and validated by Kubernetes, even
// for a namespace on the list; an error is a selector or namespaces it cannot read, or a namespace the run did not read, on which a scenario must
// fail rather than skip.
func (e Env) Admits(namespace string) (bool, error) {
	if e.ConfigSource != "crds" || namespace == "" {
		return false, nil
	}
	var sel labels.Selector
	if e.ConnectionNamespaceSelector != "" {
		var err error
		if sel, err = parseNamespaceSelector(e.ConnectionNamespaceSelector); err != nil {
			return false, err
		}
	}
	switch {
	case slices.Contains(strings.Split(e.ConnectionNamespaces, ","), namespace):
		return true, nil
	case sel == nil:
		return e.ChatConnection == "" && e.ConnectionNamespaces == "", nil
	case sel.Empty():
		return true, nil
	}
	set, err := e.labelsOf(namespace)
	if err != nil {
		return false, err
	}
	return sel.Matches(set), nil
}

// Admission says what the broker's admission is set to, for a message.
func (e Env) Admission() string {
	if e.ConfigSource != "crds" {
		return "config source " + e.ConfigSource + ": the one connection of its directory"
	}
	if e.ChatConnection == "" && e.ConnectionNamespaces == "" && e.ConnectionNamespaceSelector == "" {
		return "no own connection, no list, no selector: every namespace"
	}
	parts := []string{"no own connection"}
	if e.ChatConnection != "" {
		parts[0] = "own connection " + e.ChatConnection
	}
	parts = append(parts, fmt.Sprintf("list %q", e.ConnectionNamespaces))
	if e.ConnectionNamespaceSelector != "" {
		parts = append(parts, "selector "+e.ConnectionNamespaceSelector)
	}
	return strings.Join(parts, ", ")
}

// parseNamespaceSelector decodes raw strictly, as a metav1.LabelSelector, and makes it a labels.Selector.
func parseNamespaceSelector(raw string) (labels.Selector, error) {
	var ls metav1.LabelSelector
	strict, err := kjson.UnmarshalStrict([]byte(raw), &ls)
	if err == nil && !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		err = errors.New("not a JSON object")
	}
	if err = errors.Join(append([]error{err}, strict...)...); err != nil {
		return nil, fmt.Errorf("E2E_CONNECTION_NAMESPACE_SELECTOR is not a LabelSelector as the broker reads one: %w", err)
	}
	sel, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return nil, fmt.Errorf("E2E_CONNECTION_NAMESPACE_SELECTOR: %w", err)
	}
	return sel, nil
}

// labelsOf returns the labels of namespace as Namespaces has them.
func (e Env) labelsOf(namespace string) (labels.Set, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(e.Namespaces), &list); err != nil {
		return nil, fmt.Errorf("E2E_NAMESPACES is not the namespaces as kubectl get namespace -o json prints them: %w", err)
	}
	for _, ns := range list.Items {
		if ns.Metadata.Name == namespace {
			return labels.Set(ns.Metadata.Labels), nil
		}
	}
	return nil, fmt.Errorf("E2E_NAMESPACES does not hold the namespace %s: the run read the labels of the namespaces the scenarios ask about, and not of this one", namespace)
}

// NewEnv reads E2E_SLACK_URL, E2E_SINK_URL, E2E_STUB_URL, E2E_ALERTMANAGER_URL and
// E2E_AGENTKOURIER_URL, E2E_SIGNALER_URL, E2E_TIER, E2E_CONFIG_SOURCE, E2E_PHASE, E2E_CHAT_CONNECTION, E2E_CONNECTION_NAMESPACES,
// E2E_CONNECTION_NAMESPACE_SELECTOR and E2E_NAMESPACES through getenv (os.Getenv in a runner); an unset or empty variable takes its
// default.
func NewEnv(getenv func(string) string) Env {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	return Env{
		SlackURL:                    get("E2E_SLACK_URL", "http://fakeslack.e2e.svc.cluster.local:8080"),
		SinkURL:                     get("E2E_SINK_URL", "http://sink.e2e.svc.cluster.local:8080"),
		StubURL:                     get("E2E_STUB_URL", "http://stubagent.e2e.svc.cluster.local:8080"),
		AlertmanagerURL:             get("E2E_ALERTMANAGER_URL", "http://alertmanager.monitoring.svc.cluster.local:9093"),
		ScenarioMCPURL:              get("E2E_SCENARIO_MCP_URL", "http://scenariomcp.kagent.svc.cluster.local:8082"),
		AgentKourierURL:             get("E2E_AGENTKOURIER_URL", "http://agent-kourier.agent-kourier.svc.cluster.local:8080"),
		SignalerURL:                 get("E2E_SIGNALER_URL", "http://signaler.e2e.svc.cluster.local:8080"),
		Tier:                        getenv("E2E_TIER"),
		ConfigSource:                get("E2E_CONFIG_SOURCE", "files"),
		Phase:                       getenv("E2E_PHASE"),
		ChatConnection:              getenv("E2E_CHAT_CONNECTION"),
		ConnectionNamespaces:        getenv("E2E_CONNECTION_NAMESPACES"),
		ConnectionNamespaceSelector: getenv("E2E_CONNECTION_NAMESPACE_SELECTOR"),
		Namespaces:                  getenv("E2E_NAMESPACES"),
	}
}
