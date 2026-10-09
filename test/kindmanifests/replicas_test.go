package kindmanifests_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// TestInstallAgentKourierPassesFurtherArgumentsToHelm: the fast tier on two replicas layers an overlay of values over the Postgres ones
// (install_agentkourier VALUES -f OVERLAY), which helm applies after the first, so that the overlay wins.
func TestInstallAgentKourierPassesFurtherArgumentsToHelm(t *testing.T) {
	bin, logs := t.TempDir(), t.TempDir()
	for _, tool := range []string{"kubectl", "helm"} {
		stub := "#!/bin/sh\necho \"$*\" >> \"$STUB_LOG/" + tool + "\"\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := `set -eu; . "$1"; ROOT=/repo; K=(kubectl --context kind-agent-kourier-fast); H=(helm --kube-context kind-agent-kourier-fast); install_agentkourier /v/base.yaml -f /v/overlay.yaml`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", "../../hack/kind/lib.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+logs)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install_agentkourier: %v\n%s", err, out)
	}
	got := readLog(t, logs, "helm")
	const want = "upgrade --install agent-kourier /repo/charts/agent-kourier --namespace agent-kourier -f /v/base.yaml --force-conflicts -f /v/overlay.yaml"
	if !strings.Contains(got, want) {
		t.Errorf("helm was asked:\n%s\nwant it to take the overlay after the base values: %s", got, want)
	}
	if !strings.Contains(readLog(t, logs, "kubectl"), "-n agent-kourier rollout restart deploy/agent-kourier") {
		t.Error("install_agentkourier did not restart the Deployment, which picks up a rebuilt image")
	}
}

// TestUpFastRefusesReplicasItCannotRun: two replicas need Postgres, which a SQLite file cannot be shared through (spec R7), and the
// tier runs one or two; the script says so before it touches anything.
func TestUpFastRefusesReplicasItCannotRun(t *testing.T) {
	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"two replicas on SQLite": {[]string{"E2E_REPLICAS=2"}, "E2E_REPLICAS=2 needs E2E_STORE=postgres"},
		"three replicas":         {[]string{"E2E_STORE=postgres", "E2E_REPLICAS=3"}, "E2E_REPLICAS must be 1 or 2"},
		"none":                   {[]string{"E2E_STORE=postgres", "E2E_REPLICAS=0"}, "E2E_REPLICAS must be 1 or 2"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "../../hack/kind/up-fast.sh")
			cmd.Env = append(os.Environ(), tc.env...)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Errorf("up-fast.sh with %v = %v\n%s\nwant it to refuse: %s", tc.env, err, out, tc.want)
			}
		})
	}
}

// TestE2ERunRefusesAPatternItCannotPutInTheJob: E2E_RUN replaces the Job's -test.run pattern, which sits in a YAML string and goes through
// sed, so a quote, a backslash or a space is refused before the script does anything.
func TestE2ERunRefusesAPatternItCannotPutInTheJob(t *testing.T) {
	for _, pattern := range []string{`TestS16"`, `Test\S16`, "Test S16", "Test#S16", "Test&S16"} {
		cmd := exec.CommandContext(t.Context(), "bash", "../../hack/kind/e2e-run.sh", "fast")
		cmd.Env = append(os.Environ(), "E2E_RUN="+pattern)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "E2E_RUN must be a Go regexp") {
			t.Errorf("e2e-run.sh with E2E_RUN=%q = %v\n%s\nwant it to refuse the pattern", pattern, err, out)
		}
	}
}

// TestSignalerIsLeastPrivilegeAndPublishedNowhere: the signaler shares the node's PID namespace and can end any process of the node, so
// it is not privileged, drops every capability but the one signalling another user's process needs, has a read-only filesystem and no
// service account token, and its Service is ClusterIP: nothing publishes it.
func TestSignalerIsLeastPrivilegeAndPublishedNowhere(t *testing.T) {
	const file = "../../hack/kind/fast/signaler.yaml"
	var d appsv1.Deployment
	load(t, file, "Deployment", &d)
	pod := d.Spec.Template.Spec
	if !pod.HostPID || pod.HostNetwork || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || len(pod.Containers) != 1 {
		t.Fatalf("pod spec %+v; want hostPID, no host network, no service account token and one container", pod)
	}
	sc := pod.Containers[0].SecurityContext
	if sc == nil || sc.Privileged != nil && *sc.Privileged || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil ||
		!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || !slices.Equal(sc.Capabilities.Add, []corev1.Capability{"KILL"}) {
		t.Errorf("container security context %+v; want unprivileged, no escalation, a read-only filesystem, ALL dropped and KILL added", sc)
	}
	for _, p := range pod.Containers[0].Ports {
		if p.HostPort != 0 {
			t.Errorf("port %+v has a host port", p)
		}
	}
	var svc corev1.Service
	load(t, file, "Service", &svc)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || len(svc.Spec.ExternalIPs) != 0 {
		t.Errorf("Service %+v; want ClusterIP and no external IPs", svc.Spec)
	}
}

// TestSignalerAdmitsOnlyTheRunner: the signaler is unauthenticated and can freeze or kill any container of the node, so its NetworkPolicy
// admits ingress from the pods that carry the E2E runner's label, in its own namespace, on its port, and from nothing else.
func TestSignalerAdmitsOnlyTheRunner(t *testing.T) {
	var job batchv1.Job
	load(t, "../../hack/kind/e2e/runner.yaml", "Job", &job)
	var np networkingv1.NetworkPolicy
	load(t, "../../hack/kind/fast/signaler.yaml", "NetworkPolicy", &np)

	if np.Namespace != "e2e" || np.Spec.PodSelector.MatchLabels["app"] != "signaler" || !slices.Equal(np.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}) {
		t.Fatalf("policy %s/%s selects %v for %v; want ingress to the signaler's pods in e2e", np.Namespace, np.Name, np.Spec.PodSelector, np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 || len(np.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("ingress rules %+v; want one rule, from one peer, on one port", np.Spec.Ingress)
	}
	peer := np.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector != nil || peer.IPBlock != nil || peer.PodSelector == nil || len(peer.PodSelector.MatchLabels) != 1 {
		t.Fatalf("peer %+v; want a pod selector of one label and no namespace selector or address block", peer)
	}
	for k, v := range peer.PodSelector.MatchLabels {
		if job.Spec.Template.Labels[k] != v {
			t.Errorf("the policy admits %s=%s; the runner's pod carries %v", k, v, job.Spec.Template.Labels)
		}
	}
	if p := np.Spec.Ingress[0].Ports[0]; p.Port == nil || p.Port.IntValue() != 8080 {
		t.Errorf("port %+v; want 8080, the signaler's", p)
	}
}
