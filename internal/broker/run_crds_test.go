package broker

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// crStartConfig is startConfig that reads the custom resources of a fake cluster, edited by edit, and serves the
// ChatConnection of spec 7's examples, with no config directory.
func crStartConfig(t *testing.T, edit func([]*unstructured.Unstructured)) RunConfig {
	t.Helper()
	c := startConfig(t)
	c.ConfigSource, c.ConfigDir, c.ChatConnection = SourceCRDs, "", testConnection
	c.Dynamic = crCluster(t, edit)
	return c
}

// With the custom resources as the source the broker reads them, serves, and is ready as it is on files; the config it
// reads is the cluster's, and its directory is not looked at.
func TestRunServesTheCustomResources(t *testing.T) {
	c := crStartConfig(t, nil)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	code, body := r.get("/metrics")
	if code != http.StatusOK || !strings.Contains(body, `agentkourier_config_invalid_objects{kind="Binding"} 0`) {
		t.Errorf("/metrics = %d; want the gauge of invalid Bindings at 0", code)
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after a signal; want nil", err)
	}
}

// One object that does not pass validation does not stop the broker or make it unready, and is counted: a Binding that
// is invalid when the broker starts is left out, and the others are served.
func TestRunDoesNotStopForOneInvalidObject(t *testing.T) {
	c := crStartConfig(t, func(objs []*unstructured.Unstructured) {
		_ = unstructured.SetNestedField(objs[2].Object, "#general", "spec", "chat", "channel") // sre-alerts
	})
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	_, body := r.get("/metrics")
	if !strings.Contains(body, `agentkourier_config_invalid_objects{kind="Binding"} 1`) {
		t.Errorf("/metrics does not count the invalid Binding:\n%s", grep(body, "agentkourier_config_"))
	}
	if !strings.Contains(body, `agentkourier_config_binding_rejections{reason="InvalidSpec"} 1`) {
		t.Errorf("/metrics does not count the Binding as rejected for its spec:\n%s", grep(body, "agentkourier_config_"))
	}
}

// With no CRDs installed the broker does not start, and says it is the wiring that is wrong, which exits 2.
func TestRunWithoutTheCRDsIsAWiringError(t *testing.T) {
	c := crStartConfig(t, nil)
	c.Dynamic.(interface {
		PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
	}).PrependReactor("list", "chatconnections", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(crConnections.GroupResource(), "")
	})
	err := start(t, c).wait()
	if !errors.Is(err, ErrWiring) || !errors.Is(err, crsource.ErrCRDsAbsent) {
		t.Errorf("Run = %v; want ErrWiring wrapping ErrCRDsAbsent", err)
	}
}

// The broker's own connection, if missing at start, stops it with a wiring error that names it.
func TestRunWithoutItsOwnConnectionIsAWiringError(t *testing.T) {
	c := crStartConfig(t, nil)
	c.ChatConnection.Name = "another"
	err := start(t, c).wait()
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "agent-kourier-system/another was not found") {
		t.Errorf("Run = %v; want ErrWiring saying the connection was not found", err)
	}
}

// grep returns the lines of text that hold sub.
func grep(text, sub string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
