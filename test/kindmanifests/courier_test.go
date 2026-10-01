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
	"maps"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/test/e2e"
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
	if err != nil || len(set.Bindings) != 7 || len(set.ChatConnections) != 1 || len(set.AgentBackends) != 1 {
		t.Fatalf("Load = %+v, %v; want a Binding for the smoke test and one for each of six scenarios, and one connection and backend", set, err)
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
	// The environment cmd/courier reads (its package comment), explicit even where it is the default, so that
	// the Deployment says what courier runs with. No value comes from a Secret: the tokens are read by the
	// resolver from the API, never from the environment.
	env := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Errorf("env %s comes from %+v; want literal values only", e.Name, e.ValueFrom)
		}
		env[e.Name] = e.Value
	}
	wantEnv := map[string]string{"COURIER_CONFIG_DIR": "/etc/courier", "COURIER_DB_PATH": "/data/courier.db", "COURIER_LISTEN": ":8080",
		"COURIER_LOG_LEVEL": "info", "COURIER_KLOG_V": "0", "COURIER_SLACK_API_URL": "http://fakeslack.e2e.svc.cluster.local:8080/"}
	if !maps.Equal(env, wantEnv) {
		t.Errorf("env %v; want %v", env, wantEnv)
	}
	if len(c.Args) != 0 || len(c.Command) != 0 {
		t.Errorf("command %v and args %v; want none: the image's entrypoint runs courier, and the environment configures it", c.Command, c.Args)
	}
	// The listen address, the named port and the probes are one port: a probe on another would never pass.
	if len(c.Ports) != 1 || c.Ports[0].Name != "http" || c.Ports[0].ContainerPort != 8080 || c.StartupProbe.HTTPGet.Port.StrVal != "http" ||
		c.LivenessProbe.HTTPGet.Port.StrVal != "http" || c.ReadinessProbe.HTTPGet.Port.StrVal != "http" {
		t.Errorf("ports %+v; want http on 8080, which COURIER_LISTEN names, probed by all three probes", c.Ports)
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

// TestScenarioBindings: each scenario has a channel of its own, bound to the stub agent it drives, with
// the interactions it needs: askUser for S3 (the agent asks), and neither for S11, whose tool approval
// courier must deny (Phase 1 has no write-capable agent). test/e2e names the channels.
func TestScenarioBindings(t *testing.T) {
	set, err := config.Load(t.Context(), courierDir+"config")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tc := range []struct {
		scenario, channel, agent string
		askUser                  bool
	}{
		{"smoke", e2e.ChannelSmoke, "test-agent", false},
		{"S1", e2e.ChannelS1, "scenario-gated", false},
		{"S2", e2e.ChannelS2, "scenario-memory", false},
		{"S3", e2e.ChannelS3, "scenario-ask", true},
		{"S4", e2e.ChannelS4, "scenario-gated", false},
		{"S5", e2e.ChannelS5, "test-agent", false},
		{"S11", e2e.ChannelS11, "spike-approve", false},
	} {
		rb, ok := snap.BindingForChannel("courier", "slack-fast", tc.channel)
		if !ok {
			t.Errorf("%s: channel %s resolves to no Binding", tc.scenario, tc.channel)
			continue
		}
		spec := rb.Binding.Spec
		if spec.Agent.Name != tc.agent || spec.Agent.Namespace != "kagent" || spec.Interactions.AskUser != tc.askUser || spec.Interactions.ToolApprovals {
			t.Errorf("%s: channel %s is bound to %s/%s with askUser %t and toolApprovals %t; want %s, askUser %t and toolApprovals off", tc.scenario, tc.channel,
				spec.Agent.Namespace, spec.Agent.Name, spec.Interactions.AskUser, spec.Interactions.ToolApprovals, tc.agent, tc.askUser)
		}
		if seen[tc.channel] {
			t.Errorf("%s: channel %s is another scenario's too; scenarios must not share a thread", tc.scenario, tc.channel)
		}
		seen[tc.channel] = true
	}
}

// TestService: the E2E runner reaches courier's health and metrics port, and nothing else of it, through
// a ClusterIP Service in the fast tier. The Alertmanager receiver has no Service until WP-47, so the
// Service has the one port, the one the probes use, and nothing publishes it.
func TestService(t *testing.T) {
	var svc corev1.Service
	var d appsv1.Deployment
	load(t, courierDir+"service.yaml", "Service", &svc)
	load(t, courierDir+"deployment.yaml", "Deployment", &d)
	c := d.Spec.Template.Spec.Containers[0]
	if svc.Namespace != "courier" || svc.Name != "courier" || svc.Spec.Type != corev1.ServiceTypeClusterIP || len(svc.Spec.ExternalIPs) != 0 || svc.Spec.LoadBalancerIP != "" {
		t.Errorf("Service %s/%s of type %q with external IPs %v; want courier/courier, ClusterIP, nothing published", svc.Namespace, svc.Name, svc.Spec.Type, svc.Spec.ExternalIPs)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != "http" || svc.Spec.Ports[0].Port != 8080 || svc.Spec.Ports[0].NodePort != 0 ||
		svc.Spec.Ports[0].TargetPort.String() != "http" {
		t.Errorf("Service ports %+v; want the one port, http 8080 to the pod's http", svc.Spec.Ports)
	}
	if len(svc.Spec.Selector) != 1 || svc.Spec.Selector["app.kubernetes.io/name"] != "courier" || d.Spec.Template.Labels["app.kubernetes.io/name"] != "courier" {
		t.Errorf("Service selector %v; want it to select courier's pod alone", svc.Spec.Selector)
	}
	probes := map[string]bool{c.StartupProbe.HTTPGet.Port.String(): true, c.LivenessProbe.HTTPGet.Port.String(): true, c.ReadinessProbe.HTTPGet.Port.String(): true}
	if len(probes) != 1 || !probes[svc.Spec.Ports[0].TargetPort.String()] {
		t.Errorf("probes use ports %v but the Service targets %s; want the health port, and only it", probes, svc.Spec.Ports[0].TargetPort.String())
	}
	if len(c.Ports) != 1 || c.Ports[0].Name != "http" {
		t.Errorf("container ports %+v; want only http: a receiver port added here must not reach the Service until WP-47", c.Ports)
	}
}

// TestRunnerJobGetsItsTier: the Job runs the tests of one tier, and a test that needs courier (the smoke
// test's mention) must be able to tell the fast tier, which has it, from the full tier, which does not
// yet. The Job's template carries a placeholder and e2e-run.sh fills it with the tier it was given.
func TestRunnerJobGetsItsTier(t *testing.T) {
	var job batchv1.Job
	load(t, "../../hack/kind/e2e/runner.yaml", "Job", &job)
	var tier string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "E2E_TIER" {
			tier = e.Value
		}
	}
	script, err := os.ReadFile("../../hack/kind/e2e-run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if tier != "@TIER@" || !strings.Contains(string(script), `sed "s/@TIER@/${tier}/" "${HERE}/e2e/runner.yaml"`) {
		t.Errorf("E2E_TIER = %q in the Job; want the placeholder @TIER@, and e2e-run.sh to substitute the tier it runs", tier)
	}
}
