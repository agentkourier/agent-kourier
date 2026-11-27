package chart_test

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

var axRefusals = []refusal{
	{"no server", []string{"--set", "addons.ax.server.address="}, "addons.ax.enabled needs addons.ax.server.address", nil},
	{"no Task template", []string{"--set", "addons.ax.taskTemplate.configMap.name="}, "addons.ax.enabled needs addons.ax.taskTemplate.configMap.name", nil},
	{"no result Secret", []string{"--set", "addons.ax.resultCallback.tokenSecret.name="}, "addons.ax.enabled needs addons.ax.resultCallback.tokenSecret.name", nil},
	{"same Secret", []string{"--set", "addons.ax.resultCallback.tokenSecret.name=ax-token"}, "must name a separate Secret", nil},
	{"callback Secret granted to broker", []string{"--set", "rbac.secretNames.agent-kourier={ax-result-token}"}, "must be omitted from broker Secret references", nil},
	{"HTTP callback", []string{"--set", "addons.ax.resultCallback.url=http://results.test/ax/results/v1"}, "must be an HTTPS URL", nil},
	{"legacy model", []string{"--set", "addons.ax.model.provider=bedrock"}, "additional properties 'model' not allowed", nil},
}

func TestAXCallbackSecretAndRoutes(t *testing.T) {
	t.Parallel()
	helmPath(t)
	out := mustRender(t, "agent-kourier", "agent-kourier", "-f", values("ax-addon"), "--set-string", "addons.ax.resultCallback.ingress.annotations.example=composed")
	var ax, broker appsv1.Deployment
	find(t, out, "Deployment", "agent-kourier-ax", &ax)
	find(t, out, "Deployment", "agent-kourier", &broker)
	secret := slices.IndexFunc(ax.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Secret != nil && v.Secret.SecretName == "ax-result-token"
	})
	if secret < 0 || !slices.ContainsFunc(ax.Spec.Template.Spec.Containers[0].VolumeMounts, func(v corev1.VolumeMount) bool {
		return v.Name == ax.Spec.Template.Spec.Volumes[secret].Name && v.ReadOnly
	}) {
		t.Fatal("AX deployment must mount the callback token read-only")
	}
	if slices.ContainsFunc(broker.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Secret != nil && v.Secret.SecretName == "ax-result-token"
	}) {
		t.Fatal("broker mounts the callback token")
	}
	var role rbacv1.Role
	find(t, out, "Role", "agent-kourier-secrets", &role)
	for _, rule := range role.Rules {
		if slices.Contains(rule.Resources, "secrets") &&
			(len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, "ax-result-token")) {
			t.Fatal("broker can read the AX callback token")
		}
	}
	var ingress networkingv1.Ingress
	find(t, out, "Ingress", "agent-kourier-ax-results", &ingress)
	if ingress.Annotations["example"] != "composed" {
		t.Fatalf("callback annotations = %v", ingress.Annotations)
	}
	paths := ingress.Spec.Rules[0].HTTP.Paths
	if len(paths) != 2 || paths[0].Path != "/ax/results/v1" || paths[1].Path != "/ax/results/v1/start" {
		t.Fatalf("unexpected callback routes: %+v", paths)
	}
	for _, path := range paths {
		if path.PathType == nil || *path.PathType != networkingv1.PathTypeExact ||
			path.Backend.Service.Name != "agent-kourier-ax" || path.Backend.Service.Port.Number != 8090 {
			t.Fatalf("unexpected callback route: %+v", path)
		}
	}
}
