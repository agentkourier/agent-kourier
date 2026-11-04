package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/broker"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// AGENTKOURIER_CONFIG_SOURCE is files until it says crds, unset included, and nothing else is a source: there is no
// fallback and no guess. The connection is named with it, and only with it.
func TestEnvConfigChoosesTheConfigSource(t *testing.T) {
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
		"crds needs the connection":   {env{"AGENTKOURIER_CONFIG_SOURCE": "crds"}, "", v1alpha1.ObjectRef{}, "AGENTKOURIER_CHAT_CONNECTION is required"},
		"a connection with no slash":  {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "slack-main"}, "", v1alpha1.ObjectRef{}, "namespace/name"},
		"a connection with a key too": {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "a/b/c"}, "", v1alpha1.ObjectRef{}, "namespace/name"},
		"a connection with no name":   {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "a/"}, "", v1alpha1.ObjectRef{}, "namespace/name"},
		"files refuses a connection":  {env{"AGENTKOURIER_CHAT_CONNECTION": "a/b"}, "", v1alpha1.ObjectRef{}, "AGENTKOURIER_CHAT_CONNECTION is set with AGENTKOURIER_CONFIG_SOURCE=files"},
		"an unknown source":           {env{"AGENTKOURIER_CONFIG_SOURCE": "etcd"}, "", v1alpha1.ObjectRef{}, "AGENTKOURIER_CONFIG_SOURCE"},
		"a source with a space":       {env{"AGENTKOURIER_CONFIG_SOURCE": " crds"}, "", v1alpha1.ObjectRef{}, "AGENTKOURIER_CONFIG_SOURCE"},
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
