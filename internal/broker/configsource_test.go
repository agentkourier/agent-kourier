package broker

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

func TestValidateTheConfigSource(t *testing.T) {
	conn := v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "slack-main"}
	for name, tc := range map[string]struct {
		mutate func(*RunConfig)
		want   string // "" is accepted; otherwise a piece of the wiring error
	}{
		"files is the default":               {func(*RunConfig) {}, ""},
		"files, named":                       {func(c *RunConfig) { c.ConfigSource = SourceFiles }, ""},
		"files needs its directory":          {func(c *RunConfig) { c.ConfigSource = SourceFiles; c.ConfigDir = "" }, "ConfigDir"},
		"files refuses a ChatConnection":     {func(c *RunConfig) { c.ChatConnection = conn }, "files"},
		"crds needs the connection":          {func(c *RunConfig) { c.ConfigSource = SourceCRDs; c.ConfigDir = "" }, "ChatConnection"},
		"crds needs the whole connection":    {func(c *RunConfig) { c.ConfigSource = SourceCRDs; c.ChatConnection = v1alpha1.ObjectRef{Name: "x"} }, "ChatConnection"},
		"crds with its connection":           {func(c *RunConfig) { c.ConfigSource = SourceCRDs; c.ChatConnection = conn; c.ConfigDir = "" }, ""},
		"crds ignores a config directory":    {func(c *RunConfig) { c.ConfigSource = SourceCRDs; c.ChatConnection = conn }, ""},
		"an unknown source is refused":       {func(c *RunConfig) { c.ConfigSource = "etcd" }, `unknown config source "etcd"`},
		"the name is case sensitive, as env": {func(c *RunConfig) { c.ConfigSource = "CRDs"; c.ChatConnection = conn }, `unknown config source "CRDs"`},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid(t)
			tc.mutate(&c)
			err := c.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("validate = %v, want nil", err)
			case tc.want != "" && (!errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("validate = %v, want an ErrWiring saying %q", err, tc.want)
			}
		})
	}
}

// The dynamic client that reads the custom resources reaches the cluster the way the typed one does, through the pod's own
// ServiceAccount and nothing else.
func TestInClusterDynamicClientRefusesOutsideACluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	client, err := InClusterDynamicClient()

	if client != nil || !errors.Is(err, ErrWiring) || !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("InClusterDynamicClient = %v, %v; want no client and ErrWiring wrapping ErrNotInCluster", client, err)
	}
}
