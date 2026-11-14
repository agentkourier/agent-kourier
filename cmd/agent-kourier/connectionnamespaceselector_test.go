package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/agentkourier/agent-kourier/internal/broker"
)

const selectorKey = "AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR"

// chartSelector is the selector JSON the chart renders for config.connectionNamespaceSelector, byte for byte (test/chart writes it).
func chartSelector(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "chart-connection-namespace-selector.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR holds a metav1.LabelSelector as JSON (spec D54). Unset or empty is no selector, {} (and any
// selector that requires nothing) is every namespace, and anything else must decode strictly, as the API server decodes one (case-sensitive
// fields, no unknown or repeated field, one object and nothing after it), and pass Kubernetes' validation of a label selector. It is refused
// with the files source, as the list is.
func TestEnvConfigReadsTheConnectionNamespaceSelector(t *testing.T) {
	crds := func(value string) env {
		return env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "agent-kourier-system/slack-main", selectorKey: value}
	}
	fixture := chartSelector(t)
	type admits struct {
		labels labels.Set
		want   bool
	}
	for name, tc := range map[string]struct {
		env    env
		none   bool     // no selector at all (nil)
		all    bool     // a selector that admits every namespace
		admits []admits // what a selector that reads labels admits
		bad    string   // "" is accepted; otherwise a piece of the wiring error
	}{
		"unset":                 {env: env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "a/b"}, none: true},
		"empty":                 {env: crds(""), none: true},
		"{} is every namespace": {env: crds("{}"), all: true},
		"{} with spaces around": {env: crds(" \n{ }\t"), all: true},
		"an empty matchLabels":  {env: crds(`{"matchLabels":{}}`), all: true},
		"no expressions":        {env: crds(`{"matchExpressions":[]}`), all: true},
		"the chart's rendering": {env: crds(fixture), admits: []admits{
			{labels.Set{"agentkourier.dev/serve": "true", "tier": "a"}, true},
			{labels.Set{"agentkourier.dev/serve": "true", "tier": "b", "other": "x"}, true},
			{labels.Set{"agentkourier.dev/serve": "true", "tier": "c"}, false},
			{labels.Set{"agentkourier.dev/serve": "true"}, false},
			{labels.Set{"agentkourier.dev/serve": "false", "tier": "a"}, false},
			{labels.Set{"tier": "a"}, false},
		}},
		"matchLabels": {env: crds(`{"matchLabels":{"team":"yes"}}`), admits: []admits{{labels.Set{"team": "yes"}, true}, {labels.Set{"team": "no"}, false}}},
		"Exists":      {env: crds(`{"matchExpressions":[{"key":"team","operator":"Exists"}]}`), admits: []admits{{labels.Set{"team": ""}, true}, {labels.Set{}, false}}},
		"DoesNotExist": {env: crds(`{"matchExpressions":[{"key":"team","operator":"DoesNotExist"}]}`),
			admits: []admits{{labels.Set{}, true}, {labels.Set{"team": "x"}, false}}},

		"files refuses it":             {env: env{selectorKey: "{}"}, bad: selectorKey + " is set with AGENTKOURIER_CONFIG_SOURCE=files"},
		"files refuses one not JSON":   {env: env{selectorKey: "team=a"}, bad: selectorKey + " is set with AGENTKOURIER_CONFIG_SOURCE=files"},
		"spaces only":                  {env: crds("  "), bad: selectorKey + " is not JSON"},
		"the kubectl syntax":           {env: crds("team=a"), bad: selectorKey + " is not JSON"},
		"cut short":                    {env: crds(`{"matchLabels":{"team":"a"}`), bad: selectorKey + " is not JSON"},
		"trailing data":                {env: crds(`{}{}`), bad: selectorKey + " has more after the selector"},
		"trailing text":                {env: crds(`{} team=a`), bad: selectorKey + " has more after the selector"},
		"null":                         {env: crds("null"), bad: selectorKey + " is not a JSON object"},
		"a list":                       {env: crds(`[]`), bad: selectorKey + " is not a JSON object"},
		"a string":                     {env: crds(`"team=a"`), bad: selectorKey + " is not a JSON object"},
		"an unknown field":             {env: crds(`{"matchLabel":{"team":"a"}}`), bad: selectorKey + " has a field that a LabelSelector does not have"},
		"a field in another case":      {env: crds(`{"MatchLabels":{"team":"a"}}`), bad: selectorKey + " has a field that a LabelSelector does not have"},
		"an unknown expression field":  {env: crds(`{"matchExpressions":[{"key":"team","operator":"In","value":["a"]}]}`), bad: selectorKey + " has a field that a LabelSelector does not have"},
		"a field twice":                {env: crds(`{"matchLabels":{"team":"a"},"matchLabels":{}}`), bad: selectorKey + " has a field twice"},
		"a label twice":                {env: crds(`{"matchLabels":{"team":"a","team":"b"}}`), bad: selectorKey + " has a field twice"},
		"a field of the wrong type":    {env: crds(`{"matchLabels":["team"]}`), bad: selectorKey + " does not have the shape of a LabelSelector"},
		"a label value not a string":   {env: crds(`{"matchLabels":{"team":1}}`), bad: selectorKey + " does not have the shape of a LabelSelector"},
		"a bad operator":               {env: crds(`{"matchExpressions":[{"key":"team","operator":"Equals","values":["a"]}]}`), bad: "matchExpressions item 1 has an operator that is not In, NotIn, Exists or DoesNotExist"},
		"a lower-case operator":        {env: crds(`{"matchExpressions":[{"key":"team","operator":"Exists"},{"key":"tier","operator":"in","values":["a"]}]}`), bad: "matchExpressions item 2 has an operator"},
		"In with no values":            {env: crds(`{"matchExpressions":[{"key":"team","operator":"In"}]}`), bad: "matchExpressions item 1 is not a valid requirement"},
		"Exists with values":           {env: crds(`{"matchExpressions":[{"key":"team","operator":"Exists","values":["a"]}]}`), bad: "matchExpressions item 1 is not a valid requirement"},
		"a bad expression key":         {env: crds(`{"matchExpressions":[{"key":"bad key","operator":"Exists"}]}`), bad: "matchExpressions item 1 is not a valid requirement"},
		"a bad expression value":       {env: crds(`{"matchExpressions":[{"key":"team","operator":"In","values":["a b"]}]}`), bad: "matchExpressions item 1 is not a valid requirement"},
		"a bad label key":              {env: crds(`{"matchLabels":{"bad key":"a"}}`), bad: "matchLabels holds a key or a value that is not a valid label key or value"},
		"a label key with two slashes": {env: crds(`{"matchLabels":{"a/b/c":"a"}}`), bad: "matchLabels holds a key or a value"},
		"a bad label value":            {env: crds(`{"matchLabels":{"team":"-a"}}`), bad: "matchLabels holds a key or a value"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := loadEnv(tc.env.get)
			if tc.bad != "" {
				if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), tc.bad) {
					t.Errorf("loadEnv = %v, want an ErrWiring saying %q", err, tc.bad)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sel := c.connectionNamespaceSelector
			switch {
			case tc.none:
				if sel != nil {
					t.Errorf("selector = %v; want none (nil)", sel)
				}
				return
			case sel == nil:
				t.Fatal("selector = nil; want one")
			case tc.all:
				if !sel.Empty() {
					t.Errorf("selector %q is not empty; want every namespace", sel)
				}
				return
			}
			if sel.Empty() {
				t.Fatalf("selector is empty; want one that reads labels")
			}
			if _, selectable := sel.Requirements(); !selectable {
				t.Fatalf("selector %q selects nothing; want what was given", sel)
			}
			for _, a := range tc.admits {
				if got := sel.Matches(a.labels); got != a.want {
					t.Errorf("selector %q matches %v = %v, want %v", sel, a.labels, got, a.want)
				}
			}
		})
	}
}

// No error of the selector shows the value or a piece of it: the variable is one a person may paste a token into, and a field name, a
// label key or a value is as much the value as the whole.
func TestEnvConfigDoesNotEchoTheConnectionNamespaceSelector(t *testing.T) {
	const canary = "xoxb-FAKE-LEAK-CANARY"
	for name, value := range map[string]string{
		"not JSON":         canary,
		"a bad character":  "{" + canary,
		"trailing":         "{}" + canary,
		"a string":         `"` + canary + `"`,
		"an unknown field": `{"` + canary + `":{}}`,
		"a field twice":    `{"matchLabels":{"` + canary + `":"a","` + canary + `":"b"}}`,
		"a wrong type":     `{"matchLabels":{"` + canary + `":1}}`,
		"a bad operator":   `{"matchExpressions":[{"key":"team","operator":"` + canary + `"}]}`,
		"a bad key":        `{"matchExpressions":[{"key":"` + canary + `/x/y","operator":"Exists"}]}`,
		"a bad value":      `{"matchExpressions":[{"key":"team","operator":"In","values":["` + canary + `!"]}]}`,
		"a bad label":      `{"matchLabels":{"team":"` + canary + `!"}}`,
		"with files":       `{"matchLabels":{"team":"` + canary + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := env{"AGENTKOURIER_CONFIG_SOURCE": "crds", selectorKey: value}
			if name == "with files" {
				delete(e, "AGENTKOURIER_CONFIG_SOURCE")
			}
			_, err := loadEnv(e.get)
			if !errors.Is(err, broker.ErrWiring) {
				t.Fatalf("loadEnv = %v; want an ErrWiring", err)
			}
			if strings.Contains(err.Error(), "FAKE") {
				t.Errorf("the error shows the value: %v", err)
			}
		})
	}
}

// A selector that is refused stops the broker with exit 2, through run as through exitCode, and stderr does not show it.
func TestABadConnectionNamespaceSelectorExitsWithUsage(t *testing.T) {
	const canary = "xoxb-FAKE-LEAK-CANARY"
	e := env{"AGENTKOURIER_CONFIG_SOURCE": "crds", selectorKey: `{"matchLabels":{"team":"` + canary + `!"}}`}
	var stdout, stderr bytes.Buffer
	if code := run(nil, "dev", &stdout, &stderr, e.get); code != exitUsage || !strings.Contains(stderr.String(), selectorKey) || strings.Contains(stderr.String(), "FAKE") {
		t.Errorf("run = %d with stderr %q; want %d, naming the variable and not its value", code, stderr.String(), exitUsage)
	}
}

// The selector and the list are read together: a namespace on either is admitted (the source's rule), so both may be set, with or without
// an own connection.
func TestEnvConfigReadsTheSelectorBesideTheList(t *testing.T) {
	c, err := loadEnv(env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CONNECTION_NAMESPACES": "team-a", selectorKey: `{"matchLabels":{"team":"yes"}}`}.get)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.connectionNamespaces) != 1 || c.connectionNamespaces[0] != "team-a" || c.connectionNamespaceSelector == nil || c.connectionNamespaceSelector.String() != "team=yes" {
		t.Errorf("list %q, selector %v; want team-a and team=yes", c.connectionNamespaces, c.connectionNamespaceSelector)
	}
}
