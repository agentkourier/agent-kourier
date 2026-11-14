package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/broker"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// AGENTKOURIER_CONFIG_SOURCE is files until it says crds, unset included, and nothing else is a source: there is no
// fallback and no guess. The own connection is named with crds, where it is optional (spec D53: with no list and no selector either, the
// broker serves every namespace, spec D54), and it is refused with files. A list that is set and names no namespace is refused, so that a
// slip in a template does not become every namespace.
func TestEnvConfigChoosesTheConfigSource(t *testing.T) {
	const namesNone = "AGENTKOURIER_CONNECTION_NAMESPACES is set and names no namespace"
	for name, tc := range map[string]struct {
		env    env
		source broker.ConfigSource
		conn   v1alpha1.ObjectRef
		want   string // "" is accepted; otherwise a piece of the wiring error
	}{
		"unset is files":    {env{}, broker.SourceFiles, v1alpha1.ObjectRef{}, ""},
		"empty is files":    {env{"AGENTKOURIER_CONFIG_SOURCE": ""}, broker.SourceFiles, v1alpha1.ObjectRef{}, ""},
		"files, named":      {env{"AGENTKOURIER_CONFIG_SOURCE": "files"}, broker.SourceFiles, v1alpha1.ObjectRef{}, ""},
		"files in capitals": {env{"AGENTKOURIER_CONFIG_SOURCE": "FILES"}, broker.SourceFiles, v1alpha1.ObjectRef{}, ""},
		"crds with a connection": {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "agent-kourier-system/slack-main"},
			broker.SourceCRDs, v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "slack-main"}, ""},
		"crds with a list and no connection": {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CONNECTION_NAMESPACES": "team-a,team-b"},
			broker.SourceCRDs, v1alpha1.ObjectRef{}, ""},
		"crds with both": {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "agent-kourier-system/slack-main", "AGENTKOURIER_CONNECTION_NAMESPACES": "team-a"},
			broker.SourceCRDs, v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "slack-main"}, ""},
		"crds with neither":                                           {env{"AGENTKOURIER_CONFIG_SOURCE": "crds"}, broker.SourceCRDs, v1alpha1.ObjectRef{}, ""},
		"crds with an empty list":                                     {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CONNECTION_NAMESPACES": ""}, broker.SourceCRDs, v1alpha1.ObjectRef{}, ""},
		"crds with a list of separators only":                         {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CONNECTION_NAMESPACES": " , ,,"}, "", v1alpha1.ObjectRef{}, namesNone},
		"crds with an empty connection and a list of separators only": {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "", "AGENTKOURIER_CONNECTION_NAMESPACES": ","}, "", v1alpha1.ObjectRef{}, namesNone},
		"a bad connection is refused though a list is set":            {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "slack-main", "AGENTKOURIER_CONNECTION_NAMESPACES": "team-a"}, "", v1alpha1.ObjectRef{}, "namespace/name"},
		"a connection with no slash":                                  {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "slack-main"}, "", v1alpha1.ObjectRef{}, "namespace/name"},
		"a connection with a key too":                                 {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "a/b/c"}, "", v1alpha1.ObjectRef{}, "namespace/name"},
		"a connection with no name":                                   {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "a/"}, "", v1alpha1.ObjectRef{}, "namespace/name"},
		"files refuses a connection":                                  {env{"AGENTKOURIER_CHAT_CONNECTION": "a/b"}, "", v1alpha1.ObjectRef{}, "AGENTKOURIER_CHAT_CONNECTION is set with AGENTKOURIER_CONFIG_SOURCE=files"},
		"an unknown source":                                           {env{"AGENTKOURIER_CONFIG_SOURCE": "etcd"}, "", v1alpha1.ObjectRef{}, "AGENTKOURIER_CONFIG_SOURCE"},
		"a source with a space":                                       {env{"AGENTKOURIER_CONFIG_SOURCE": " crds"}, "", v1alpha1.ObjectRef{}, "AGENTKOURIER_CONFIG_SOURCE"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := loadEnv(tc.env.get)
			if tc.want != "" {
				if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("loadEnv = %v, want an ErrWiring saying %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.configSource != tc.source || c.chatConnection != tc.conn {
				t.Errorf("source %q, connection %+v; want %q, %+v", c.configSource, c.chatConnection, tc.source, tc.conn)
			}
		})
	}
}

// With crds and no own connection, no list and no selector the broker serves the ChatConnections of every namespace (spec D54): nothing is
// refused, and no selector is made for it (the source reads that combination as every namespace, and needs no right on namespaces for it).
func TestACrdsConfigWithNothingSetServesEveryNamespace(t *testing.T) {
	c, err := loadEnv(env{"AGENTKOURIER_CONFIG_SOURCE": "crds"}.get)
	if err != nil {
		t.Fatalf("loadEnv = %v; want every namespace", err)
	}
	if c.chatConnection != (v1alpha1.ObjectRef{}) || c.connectionNamespaces != nil || c.connectionNamespaceSelector != nil {
		t.Errorf("connection %+v, list %q, selector %v; want none of the three", c.chatConnection, c.connectionNamespaces, c.connectionNamespaceSelector)
	}
}

// A list that is set and names no namespace (separators and spaces only) is a slip, not a choice: it is refused, with exit 2 through run as
// through exitCode, and the message says what to do. Read as unset, it would serve every namespace.
func TestAListThatNamesNoNamespaceExitsWithUsage(t *testing.T) {
	e := env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CONNECTION_NAMESPACES": " , "}
	_, err := loadEnv(e.get)
	want := "AGENTKOURIER_CONNECTION_NAMESPACES is set and names no namespace (only commas and spaces): unset it, or name the namespaces"
	if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), want) {
		t.Fatalf("loadEnv = %v; want an ErrWiring saying %q", err, want)
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("exitCode = %d, want %d", got, exitUsage)
	}
	var stdout, stderr bytes.Buffer
	if code := run(nil, "dev", &stdout, &stderr, e.get); code != exitUsage || !strings.Contains(stderr.String(), want) {
		t.Errorf("run = %d with stderr %q; want %d and the message", code, stderr.String(), exitUsage)
	}
}

// A connection that is not namespace/name is refused without its value: it is a variable a person may paste a token into.
func TestEnvConfigDoesNotEchoABadChatConnection(t *testing.T) {
	const canary = "xoxb-FAKE-LEAK-CANARY"
	_, err := loadEnv(env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": canary}.get)
	if !errors.Is(err, broker.ErrWiring) || strings.Contains(err.Error(), canary) {
		t.Errorf("loadEnv = %v; want an ErrWiring that does not show the value", err)
	}
}

// A CRD that is not installed is the operator's to fix, so a usage error; a ServiceAccount that may not read them is RBAC,
// as it is for Secrets.
func TestExitCodesOfTheCustomResourceSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"the CRDs are not installed", fmt.Errorf("%w: %w", broker.ErrWiring, crsource.ErrCRDsAbsent), exitUsage},
		{"RBAC denied access to the custom resources", fmt.Errorf("start: %w", crsource.ErrAccessDenied), exitRBAC},
	} {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("%s: exitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}
