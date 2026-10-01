// Package kindmanifests_test checks the manifests and config that the kind tiers apply, which live under
// hack/kind, so a mistake in them fails `make test` and not the next `make e2e-fast-up`. It reads them
// by relative path, as test/e2e/kube's runner_role_test.go does for the runner's Role.
package kindmanifests_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// secrets is what up-fast.sh creates in namespace courier (never committed): the keys, not the values.
type secrets map[string][]string

func (s secrets) Keys(_ context.Context, namespace, name string) ([]string, error) {
	if keys, ok := s[namespace+"/"+name]; ok {
		return keys, nil
	}
	return nil, fmt.Errorf("%s/%s: %w", namespace, name, config.ErrSecretNotFound)
}

// courierDir is the fast tier's courier manifests and config.
const courierDir = "../../hack/kind/fast/courier/"

var upFastSecrets = secrets{"courier/slack-fast": {"botToken", "appToken"}, "courier/courier-agent-token": {"token"}}

func TestShippedConfigLoadsAndResolves(t *testing.T) {
	set, err := config.Load(t.Context(), courierDir+"config")
	if err != nil || len(set.Bindings) != 1 || len(set.ChatConnections) != 1 || len(set.AgentBackends) != 1 {
		t.Fatalf("Load = %+v, %v; want one of each", set, err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	rb, ok := snap.BindingForChannel("courier", "slack-fast", "C1")
	if !ok || rb.Binding.Spec.Agent.Name != "test-agent" || rb.Backend.Spec.URL != "http://stubagent.e2e.svc.cluster.local:8080" {
		t.Errorf("channel C1 resolves to %+v, %v; want the Binding for test-agent on the stub agent's Service", rb, ok)
	}
	// The Secrets are the ones up-fast.sh makes: without either, the config does not resolve.
	for name, missing := range map[string]string{"the Slack tokens": "courier/slack-fast", "the agent token": "courier/courier-agent-token"} {
		partial := secrets{}
		for k, v := range upFastSecrets {
			if k != missing {
				partial[k] = v
			}
		}
		if _, err := config.Resolve(t.Context(), set, partial); err == nil {
			t.Errorf("config resolved without %s", name)
		}
	}
}

// objects decodes every document of a manifest file.
func objects(t *testing.T, file string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, doc)
	}
}

// load reads the object of the given kind in file into into.
func load(t *testing.T, file, kind string, into any) {
	t.Helper()
	for _, doc := range objects(t, file) {
		if doc["kind"] == kind {
			b, _ := json.Marshal(doc)
			if err := json.Unmarshal(b, into); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("%s has no %s", file, kind)
}

func TestDeployment(t *testing.T) {
	var d appsv1.Deployment
	load(t, courierDir+"deployment.yaml", "Deployment", &d)
	spec, pod := d.Spec, d.Spec.Template.Spec
	if d.Namespace != "courier" || d.Name != "courier" || *spec.Replicas != 1 || spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("Deployment %s/%s, %d replicas, strategy %s; want courier/courier, one, Recreate: two pods must never share the SQLite volume", d.Namespace, d.Name, *spec.Replicas, spec.Strategy.Type)
	}
	if spec.Template.Labels["app.kubernetes.io/name"] != "courier" || spec.Selector.MatchLabels["app.kubernetes.io/name"] != "courier" {
		t.Errorf("labels %v; want app.kubernetes.io/name=courier on the pod and the selector", spec.Template.Labels)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("%d containers, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Name != "courier" || c.Image != "courier-e2e/courier-testhooks:dev" || c.ImagePullPolicy != corev1.PullNever {
		t.Errorf("container %s runs %s with pull policy %s; want courier, the local testhooks image, Never", c.Name, c.Image, c.ImagePullPolicy)
	}
	sc := c.SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
		pod.SecurityContext == nil || !*pod.SecurityContext.RunAsNonRoot || *pod.SecurityContext.RunAsUser != 65532 ||
		pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("security contexts %+v and %+v; want non-root 65532, the RuntimeDefault seccomp profile, a read-only root, no escalation, every capability dropped", pod.SecurityContext, sc)
	}
	if c.StartupProbe == nil || c.StartupProbe.HTTPGet.Path != "/healthz" || c.StartupProbe.PeriodSeconds*c.StartupProbe.FailureThreshold < 60 ||
		c.LivenessProbe == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" || c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("probes %+v, %+v, %+v; want a startup budget of at least 60 s, liveness on /healthz, readiness on /readyz", c.StartupProbe, c.LivenessProbe, c.ReadinessProbe)
	}
	if *pod.TerminationGracePeriodSeconds != 40 || pod.HostNetwork {
		t.Errorf("grace %d s, hostNetwork %v; want 40 s (past the 25 s shutdown budget) and no host network", *pod.TerminationGracePeriodSeconds, pod.HostNetwork)
	}
	for _, p := range c.Ports {
		if p.HostPort != 0 {
			t.Errorf("port %d is published on the host", p.HostPort)
		}
	}
	mounts := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name+":"+m.MountPath] = m.ReadOnly
	}
	if ro, ok := mounts["config:/etc/courier"]; !ok || !ro || mounts["data:/data"] || mounts["tmp:/tmp"] {
		t.Errorf("mounts %v; want config read-only at /etc/courier, and writable data at /data and tmp at /tmp", mounts)
	}
	vols := map[string]corev1.VolumeSource{}
	for _, v := range pod.Volumes {
		vols[v.Name] = v.VolumeSource
	}
	if vols["data"].PersistentVolumeClaim == nil || vols["tmp"].EmptyDir == nil || vols["config"].ConfigMap == nil || vols["config"].ConfigMap.Name != "courier-config" {
		t.Errorf("volumes %+v; want data on the PVC, tmp an emptyDir, and config the courier-config ConfigMap", vols)
	}
}

// TestRBAC: courier reads Secrets through shared informers, which only list and watch, so it never gets
// one by name (WP-21d); and only in its own namespace. It holds no Role to write anything.
func TestRBAC(t *testing.T) {
	var sa corev1.ServiceAccount
	var role rbacv1.Role
	var rb rbacv1.RoleBinding
	load(t, courierDir+"rbac.yaml", "ServiceAccount", &sa)
	load(t, courierDir+"rbac.yaml", "Role", &role)
	load(t, courierDir+"rbac.yaml", "RoleBinding", &rb)
	if sa.AutomountServiceAccountToken == nil || !*sa.AutomountServiceAccountToken {
		t.Error("the ServiceAccount must automount its token: courier reads Secrets through the API")
	}
	if len(role.Rules) != 1 || fmt.Sprint(role.Rules[0].Resources, role.Rules[0].Verbs, role.Rules[0].APIGroups) != "[secrets] [list watch] []" ||
		role.Namespace != "courier" || rb.Namespace != "courier" || rb.RoleRef.Name != role.Name || len(rb.Subjects) != 1 || rb.Subjects[0].Name != sa.Name {
		t.Errorf("Role %+v bound by %+v; want list and watch, never get, on Secrets in courier alone, bound to the ServiceAccount", role, rb)
	}
	var pvc corev1.PersistentVolumeClaim
	load(t, courierDir+"pvc.yaml", "PersistentVolumeClaim", &pvc)
	if pvc.Namespace != "courier" || pvc.Name != "courier-data" || *pvc.Spec.StorageClassName != "standard" {
		t.Errorf("PVC %s/%s on %v; want courier/courier-data on kind's local-path class, standard", pvc.Namespace, pvc.Name, pvc.Spec.StorageClassName)
	}
}
