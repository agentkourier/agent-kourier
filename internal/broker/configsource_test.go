package broker

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
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

// The leader election has a client of its own, with a rate limiter of its own: the informers' lists must not queue a renewal
// of the Lease behind them.
func TestTheLeaseHasItsOwnClientAndLimiter(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	client, err := InClusterLeaseClient()
	if client != nil || !errors.Is(err, ErrWiring) || !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("InClusterLeaseClient = %v, %v; want no client and ErrWiring wrapping ErrNotInCluster", client, err)
	}

	base := &rest.Config{Host: "https://example.invalid", QPS: 1, Burst: 1}
	secrets, lease := withLimits(base, secretsQPS, secretsBurst), withLimits(base, leaseQPS, leaseBurst)
	if secrets.QPS == lease.QPS || secrets.Burst == lease.Burst || base.QPS != 1 || base.Burst != 1 {
		t.Errorf("limits: secrets %v/%d, lease %v/%d, base %v/%d; want two copies with limits of their own and the base untouched",
			secrets.QPS, secrets.Burst, lease.QPS, lease.Burst, base.QPS, base.Burst)
	}
}

// The status writer's client reaches the cluster the way the others do, with a limiter of its own that is neither the Lease's nor
// the informers', and with warnings as its WarningHandler: a write to a status is answered with a Warning for each rule that the
// stored spec breaks, and those are counted, not logged by client-go's default handler, which would write a line for each.
func TestTheStatusClientHasItsOwnLimiterAndCountsTheWarningsOfTheAPIServer(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if client, err := InClusterStatusClient(&warningCounter{}); client != nil || !errors.Is(err, ErrWiring) || !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("InClusterStatusClient = %v, %v; want no client and ErrWiring wrapping ErrNotInCluster", client, err)
	}

	base := &rest.Config{Host: "https://example.invalid", QPS: 1, Burst: 1}
	own := forStatus(base, &warningCounter{})
	if own.QPS != statusQPS || own.Burst != statusBurst || base.QPS != 1 || base.Burst != 1 || own.WarningHandler == nil {
		t.Errorf("status config: %v/%d, handler %v, base %v/%d; want the status limits, a handler, and the base untouched", own.QPS, own.Burst, own.WarningHandler, base.QPS, base.Burst)
	}
	if statusQPS == leaseQPS || statusQPS == secretsQPS || statusBurst == leaseBurst || statusBurst == secretsBurst {
		t.Errorf("the status limits %v/%d equal another client's: a burst of status writes would share its limiter", statusQPS, statusBurst)
	}

	// A real client, to a server that answers an apply with three warnings: the counter has them, and the write is not refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || !strings.HasSuffix(r.URL.Path, "/bindings/alerts/status") || r.URL.Query().Get("fieldManager") != statusFieldManager || r.URL.Query().Get("force") != "true" {
			t.Errorf("request = %s %s?%s; want a forced patch of the status under the field manager", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		for _, rule := range []string{"one", "two", "three"} {
			w.Header().Add("Warning", `299 - "rule `+rule+` is broken"`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"agentkourier.dev/v1alpha1","kind":"Binding","metadata":{"name":"alerts","namespace":"sre"}}`))
	}))
	defer srv.Close()
	warnings := &warningCounter{}
	client, err := dynamic.NewForConfig(forStatus(&rest.Config{Host: srv.URL}, warnings))
	if err != nil {
		t.Fatal(err)
	}
	obj := statusObject("sre", "alerts", []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "OK"}})
	if _, err := client.Resource(bindingsGVR).Namespace("sre").ApplyStatus(t.Context(), "alerts", obj, metav1.ApplyOptions{FieldManager: statusFieldManager, Force: true}); err != nil {
		t.Fatal(err)
	}
	if n, first := warnings.take(); n != 3 || first != "rule one is broken" {
		t.Errorf("warnings = %d, first %q; want 3 and the first text", n, first)
	}
}
