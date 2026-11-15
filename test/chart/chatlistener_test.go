package chart_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/agentkourier/agent-kourier/internal/broker"
)

// chatOn is the files route on SQLite with the chat listener switched on, and more values after it.
func chatOn(more ...string) []string {
	return append([]string{"--set", "image.repository=agent-kourier,config.source=files,chatListener.enabled=true"}, more...)
}

// envVars is the environment of the Deployment's one container, by name, with the downward API's references.
func envVars(objs []object, t *testing.T) map[string]corev1.EnvVar {
	t.Helper()
	d := deployment(t, objs)
	env := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	return env
}

// fieldRef is the downward API path an environment variable is read from, or "" when it is not.
func fieldRef(e corev1.EnvVar) string {
	if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil {
		return ""
	}
	return e.ValueFrom.FieldRef.FieldPath
}

// The chart's default leaves the chat listener off: no variable of it, no port, no chat Service, no Ingress, no Role to label the pod, and
// the health Service stays the only Service, with every pod behind it.
func TestChatListenerIsOffByDefault(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, args := range [][]string{
		{"--set", "image.repository=agent-kourier,config.source=files"},
		{"--set", "image.repository=agent-kourier,config.source=files,store.type=postgres,store.postgres.dsnSecret.name=pg,store.postgres.dsnSecret.key=dsn"},
		crMode("--set", "image.repository=agent-kourier"),
	} {
		objs := mustRender(t, "agent-kourier", "agent-kourier", args...)
		env := envVars(objs, t)
		for _, name := range []string{"AGENTKOURIER_CHAT_LISTEN", "AGENTKOURIER_POD_NAMESPACE"} {
			if _, set := env[name]; set {
				t.Errorf("%v: %s is set by default; want the chat listener off", args, name)
			}
		}
		if ports := deployment(t, objs).Spec.Template.Spec.Containers[0].Ports; len(ports) != 1 || ports[0].Name != "http" {
			t.Errorf("%v: container ports %+v; want http alone", args, ports)
		}
		for _, o := range objs {
			if o.name() == "agent-kourier-chat" || o.name() == "agent-kourier-leader-label" || o.kind() == "Ingress" {
				t.Errorf("%v: rendered %s %s with the chat listener off", args, o.kind(), o.name())
			}
		}
	}
}

// Switched on, the listener is AGENTKOURIER_CHAT_LISTEN on its own named port, and the pod is told its own name and namespace from the
// downward API, which turns the leader label on: with SQLite too, whose one replica leads and so carries the label. With Postgres the
// pod's name is already set for the election and is not set twice.
func TestChatListenerSetsTheListenerAndThePodsIdentity(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for name, args := range map[string][]string{
		"sqlite":   chatOn(),
		"postgres": chatOn("--set", "store.type=postgres,store.postgres.dsnSecret.name=pg,store.postgres.dsnSecret.key=dsn"),
		"crds":     crMode("--set", "image.repository=agent-kourier,chatListener.enabled=true"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			objs := mustRender(t, "agent-kourier", "agent-kourier", args...)
			env := envVars(objs, t)
			if got := env["AGENTKOURIER_CHAT_LISTEN"].Value; got != ":8081" {
				t.Errorf("AGENTKOURIER_CHAT_LISTEN = %q; want :8081, the default port", got)
			}
			if got := fieldRef(env["AGENTKOURIER_POD_NAME"]); got != "metadata.name" {
				t.Errorf("AGENTKOURIER_POD_NAME from %q; want the downward API's metadata.name", got)
			}
			if got := fieldRef(env["AGENTKOURIER_POD_NAMESPACE"]); got != "metadata.namespace" {
				t.Errorf("AGENTKOURIER_POD_NAMESPACE from %q; want the downward API's metadata.namespace", got)
			}
			c := deployment(t, objs).Spec.Template.Spec.Containers[0]
			want := []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}, {Name: "chat", ContainerPort: 8081}}
			if !slices.Equal(c.Ports, want) {
				t.Errorf("container ports %+v; want %+v", c.Ports, want)
			}
		})
	}
	// The port is the values'.
	env := envVars(mustRender(t, "agent-kourier", "agent-kourier", chatOn("--set", "chatListener.port=9000")...), t)
	if got := env["AGENTKOURIER_CHAT_LISTEN"].Value; got != ":9000" {
		t.Errorf("AGENTKOURIER_CHAT_LISTEN = %q with chatListener.port 9000; want :9000", got)
	}
}

// The chat Service selects what the Deployment's pods are selected by plus the leader label, so only the leader's pod is behind it, and
// targets the chat port. The health Service keeps every pod and its one port: the chat port is never on it.
func TestChatServiceSelectsTheLeaderAlone(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", chatOn("--set", "service.enabled=true")...)
	d := deployment(t, objs)
	var chat corev1.Service
	find(t, objs, "Service", "agent-kourier-chat", &chat)
	want := maps.Clone(d.Spec.Selector.MatchLabels)
	want[broker.LeaderLabel] = broker.LeaderLabelValue
	if !maps.Equal(chat.Spec.Selector, want) {
		t.Errorf("chat Service selector %v; want %v: the Deployment's selector and the leader label", chat.Spec.Selector, want)
	}
	if chat.Spec.Type != corev1.ServiceTypeClusterIP || len(chat.Spec.Ports) != 1 || chat.Spec.Ports[0].Name != "chat" || chat.Spec.Ports[0].Port != 8081 || chat.Spec.Ports[0].TargetPort.String() != "chat" {
		t.Errorf("chat Service %+v; want ClusterIP, the one port chat 8081 to the pod's chat", chat.Spec)
	}
	if chat.Spec.PublishNotReadyAddresses {
		t.Error("the chat Service publishes pods that are not ready; want only ready ones")
	}
	var health corev1.Service
	find(t, objs, "Service", "agent-kourier", &health)
	if !maps.Equal(health.Spec.Selector, d.Spec.Selector.MatchLabels) || len(health.Spec.Ports) != 1 || health.Spec.Ports[0].Name != "http" {
		t.Errorf("health Service %+v; want every pod and the http port alone", health.Spec)
	}

	// Its type, port and annotations are the values'.
	objs = mustRender(t, "agent-kourier", "agent-kourier", chatOn("--set", "chatListener.service.type=LoadBalancer,chatListener.service.port=443,chatListener.service.annotations.example\\.test/lb=external")...)
	var lb corev1.Service
	find(t, objs, "Service", "agent-kourier-chat", &lb)
	if lb.Spec.Type != corev1.ServiceTypeLoadBalancer || len(lb.Spec.Ports) != 1 || lb.Spec.Ports[0].Port != 443 || lb.Spec.Ports[0].TargetPort.String() != "chat" || lb.Annotations["example.test/lb"] != "external" {
		t.Errorf("chat Service %+v, annotations %v; want a LoadBalancer on 443 to the pod's chat port, annotated", lb.Spec, lb.Annotations)
	}
}

// The leader labels its own pod, which needs patch on pods. A Deployment's pods are named by the ReplicaSet, so Helm cannot know the
// name for resourceNames: the Role is scoped to the release namespace, holds patch alone (a merge patch needs no get), and is rendered
// only with the chat listener and rbac.create.
func TestChatListenerRBACPatchesPodsInTheReleaseNamespaceOnly(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier-system", chatOn()...)
	var role rbacv1.Role
	find(t, objs, "Role", "agent-kourier-leader-label", &role)
	if role.Namespace != "agent-kourier-system" || len(role.Rules) != 1 {
		t.Fatalf("Role %s/%s with %d rules; want one, in the release namespace", role.Namespace, role.Name, len(role.Rules))
	}
	if r := role.Rules[0]; !slices.Equal(r.APIGroups, []string{""}) || !slices.Equal(r.Resources, []string{"pods"}) || !slices.Equal(r.Verbs, []string{"patch"}) || len(r.ResourceNames) != 0 {
		t.Errorf("the rule %+v; want patch on pods and nothing else", r)
	}
	var binding rbacv1.RoleBinding
	find(t, objs, "RoleBinding", "agent-kourier-leader-label", &binding)
	if binding.Namespace != "agent-kourier-system" || binding.RoleRef.Name != "agent-kourier-leader-label" || len(binding.Subjects) != 1 || binding.Subjects[0].Name != "agent-kourier" || binding.Subjects[0].Namespace != "agent-kourier-system" {
		t.Errorf("RoleBinding %+v; want the Role bound to Agent Kourier's ServiceAccount", binding)
	}
	for _, o := range objs {
		if o.kind() == "ClusterRole" && strings.Contains(o.name(), "label") {
			t.Errorf("rendered ClusterRole %s; want the pod label granted in the release namespace only", o.name())
		}
	}
	if objs := mustRender(t, "agent-kourier", "agent-kourier-system", chatOn("--set", "rbac.create=false")...); slices.ContainsFunc(objs, func(o object) bool { return o.name() == "agent-kourier-leader-label" }) {
		t.Errorf("rbac.create=false rendered the leader label's RBAC: %v", kinds(objs))
	}
}

// With the NetworkPolicy, the chat port is open to chatFrom and to nobody else, beside the metrics port's rule, which is as it was.
func TestNetworkPolicyAdmitsChatFromToTheChatPort(t *testing.T) {
	t.Parallel()
	helmPath(t)
	policy := []string{"--set", "networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}",
		"--set-json", `networkPolicy.chatFrom=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"ingress-nginx"}}}]`}
	var np networkingv1.NetworkPolicy
	find(t, mustRender(t, "agent-kourier", "agent-kourier", chatOn(policy...)...), "NetworkPolicy", "agent-kourier", &np)
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("ingress %+v; want the chat port's rule alone, with no metricsFrom", np.Spec.Ingress)
	}
	rule := np.Spec.Ingress[0]
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 8081 || len(rule.From) != 1 || rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "ingress-nginx" {
		t.Errorf("chat rule %+v; want chatFrom to port 8081", rule)
	}

	withMetrics := append(slices.Clone(policy), "--set-json", `networkPolicy.metricsFrom=[{"podSelector":{"matchLabels":{"app":"prometheus"}}}]`)
	var both networkingv1.NetworkPolicy
	find(t, mustRender(t, "agent-kourier", "agent-kourier", chatOn(withMetrics...)...), "NetworkPolicy", "agent-kourier", &both)
	var ports []int
	for _, r := range both.Spec.Ingress {
		for _, p := range r.Ports {
			ports = append(ports, p.Port.IntValue())
		}
	}
	if !slices.Equal(ports, []int{8080, 8081}) {
		t.Errorf("ingress ports %v; want the metrics port from metricsFrom, then the chat port from chatFrom", ports)
	}
}

// The Ingress is optional and sends /chat/ alone to the chat Service: the health and metrics paths are on another port and Service, which
// it never names. Its class, host, TLS and annotations are the values'.
func TestChatIngressRoutesTheChatPathsToTheChatService(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier", chatOn("--set", "chatListener.ingress.enabled=true,chatListener.ingress.className=nginx,chatListener.ingress.host=chat.example.test",
		"--set-json", `chatListener.ingress.tls=[{"hosts":["chat.example.test"],"secretName":"chat-tls"}]`,
		"--set", "chatListener.ingress.annotations.example\\.test/owner=platform")...)
	var ing networkingv1.Ingress
	find(t, objs, "Ingress", "agent-kourier-chat", &ing)
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" || ing.Annotations["example.test/owner"] != "platform" {
		t.Errorf("Ingress class %v, annotations %v; want nginx and the values' annotation", ing.Spec.IngressClassName, ing.Annotations)
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "chat-tls" || !slices.Equal(ing.Spec.TLS[0].Hosts, []string{"chat.example.test"}) {
		t.Errorf("Ingress TLS %+v; want the values'", ing.Spec.TLS)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "chat.example.test" || ing.Spec.Rules[0].HTTP == nil || len(ing.Spec.Rules[0].HTTP.Paths) != 1 {
		t.Fatalf("Ingress rules %+v; want one, for the host, with one path", ing.Spec.Rules)
	}
	p := ing.Spec.Rules[0].HTTP.Paths[0]
	if p.Path != "/chat/" || p.PathType == nil || *p.PathType != networkingv1.PathTypePrefix || p.Backend.Service == nil || p.Backend.Service.Name != "agent-kourier-chat" || p.Backend.Service.Port.Name != "chat" {
		t.Errorf("Ingress path %+v; want the prefix /chat/ to the chat Service's chat port", p)
	}

	// With no host, no class and no TLS, the rule takes every host and the cluster's default class picks the Ingress up.
	objs = mustRender(t, "agent-kourier", "agent-kourier", chatOn("--set", "chatListener.ingress.enabled=true")...)
	var bare networkingv1.Ingress
	find(t, objs, "Ingress", "agent-kourier-chat", &bare)
	if bare.Spec.IngressClassName != nil || len(bare.Spec.TLS) != 0 || len(bare.Spec.Rules) != 1 || bare.Spec.Rules[0].Host != "" {
		t.Errorf("Ingress %+v; want no class, no TLS and one rule for every host", bare.Spec)
	}
}

// What would render a chat listener nobody can reach, or a value the chart would ignore, is refused at render.
func TestChatListenerRefusals(t *testing.T) {
	t.Parallel()
	helmPath(t)
	image := "image.repository=agent-kourier,config.source=files"
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"the health port":                  {chatOn("--set", "chatListener.port=8080"), "chatListener.port must differ from listen.port"},
		"an Ingress with no listener":      {[]string{"--set", image + ",chatListener.ingress.enabled=true"}, "chatListener.ingress.enabled needs chatListener.enabled"},
		"chatFrom with no listener":        {[]string{"--set", image, "--set-json", `networkPolicy.chatFrom=[{"podSelector":{}}]`}, "networkPolicy.chatFrom is for chatListener.enabled"},
		"a NetworkPolicy with no chatFrom": {chatOn("--set", "networkPolicy.enabled=true,networkPolicy.kubeApiServer.cidrs={10.0.0.1/32}"), "networkPolicy.enabled with chatListener.enabled needs networkPolicy.chatFrom"},
		"the leader label in podLabels":    {chatOn("--set", "podLabels.agentkourier\\.dev/role=leader"), "podLabels must not set agentkourier.dev/role"},
		"the leader label, listener off":   {[]string{"--set", image, "--set", "podLabels.agentkourier\\.dev/role=leader"}, "podLabels must not set agentkourier.dev/role"},
		"an unknown Service type":          {chatOn("--set", "chatListener.service.type=ExternalName"), "chatListener.service.type"},
		"a port out of range":              {chatOn("--set", "chatListener.port=70000"), "chatListener.port"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := render(t.Context(), t, "agent-kourier", "agent-kourier", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("render error = %v; want one containing %q", err, tc.want)
			}
		})
	}
}
