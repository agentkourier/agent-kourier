package v1alpha1

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// load strictly decodes a testdata fixture (unknown fields fail, so the types must match
// the spec field for field) and applies defaults, as the loader will. PT is *T.
func load[T any, PT interface {
	*T
	Default()
}](t *testing.T, file string) *T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	obj := new(T)
	if err := yaml.UnmarshalStrict(data, obj); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	PT(obj).Default()
	return obj
}

func TestSpecExamplesDecode(t *testing.T) {
	be := load[AgentBackend](t, "agentbackend.yaml")
	if be.Spec.Dialect != DialectKagentV1 || len(be.Spec.AllowedNamespaces) != 2 {
		t.Errorf("AgentBackend did not decode as in the spec: %+v", be.Spec)
	}

	cc := load[ChatConnection](t, "chatconnection.yaml")
	if cc.Spec.Platform != PlatformSlack || cc.Spec.Mode != ModeSocket || cc.Spec.CredentialsSecretRef.Name != "slack-main" ||
		cc.Spec.DirectMessages.DefaultBindingRef.Name != "sre-alerts" {
		t.Errorf("ChatConnection did not decode as in the spec: %+v", cc.Spec)
	}

	b := load[Binding](t, "binding.yaml")
	a := b.Spec.Alerts
	if a == nil || a.RateLimit == nil || a.RateLimit.Per.Duration != 10*time.Minute ||
		a.RateLimit.MaxRuns != 5 || a.RateLimit.MaxConcurrent != 2 || a.MaxRunsPerDay != 40 {
		t.Errorf("alerts did not decode as in the spec: %+v", a)
	}
	if b.Spec.Identity.UserID != "courier-sre-alerts" || b.Spec.Chat.Channel != "C0123456789" ||
		!b.Spec.Interactions.AskUser || !b.Spec.Feedback.Enabled ||
		b.Status.WebhookPath != "/hooks/alertmanager/sre/sre-alerts" {
		t.Errorf("Binding did not decode as in the spec: %+v", b)
	}
}

func TestDefault(t *testing.T) {
	b := load[Binding](t, "binding-minimal.yaml")
	if b.Spec.Chat.Output != OutputLive || b.Spec.Interactions.Timeout.Duration != 30*time.Minute ||
		b.Spec.Session.ThreadTTL.Duration != 2160*time.Hour || b.Spec.Interactions.ToolApprovals {
		t.Errorf("defaults not applied: %+v", b.Spec)
	}

	set := &Binding{Spec: BindingSpec{
		Chat:         ChatBinding{Output: OutputFinal},
		Interactions: Interactions{Timeout: metav1.Duration{Duration: time.Minute}},
	}}
	set.Default()
	if set.Spec.Chat.Output != OutputFinal || set.Spec.Interactions.Timeout.Duration != time.Minute {
		t.Errorf("Default overwrote explicit values: %+v", set.Spec)
	}

	cc := &ChatConnection{}
	cc.Default()
	if cc.Spec.Mode != ModeSocket {
		t.Errorf("ChatConnection mode = %q, want %q", cc.Spec.Mode, ModeSocket)
	}
}

func TestDefaultSecretKey(t *testing.T) {
	ref := func(key string) *SecretKeyRef { return &SecretKeyRef{Name: "s", Key: key} }
	be := &AgentBackend{Spec: AgentBackendSpec{Headers: []Header{
		{Name: "A", ValueSecretRef: ref("")}, {Name: "B", ValueSecretRef: ref("custom")}, {Name: "C", Value: "v"},
	}}}
	unset := &Binding{Spec: BindingSpec{Identity: Identity{TokenSecretRef: ref("")}, Alerts: &Alerts{TokenSecretRef: *ref("")}}}
	set := &Binding{Spec: BindingSpec{Identity: Identity{TokenSecretRef: ref("custom")}, Alerts: &Alerts{TokenSecretRef: *ref("custom")}}}
	var none Binding
	for _, d := range []interface{ Default() }{be, unset, set, &none} {
		d.Default()
	}

	got := []string{
		be.Spec.Headers[0].ValueSecretRef.Key, be.Spec.Headers[1].ValueSecretRef.Key,
		unset.Spec.Identity.TokenSecretRef.Key, unset.Spec.Alerts.TokenSecretRef.Key,
		set.Spec.Identity.TokenSecretRef.Key, set.Spec.Alerts.TokenSecretRef.Key,
	}
	if want := []string{"token", "custom", "token", "token", "custom", "custom"}; !slices.Equal(got, want) {
		t.Errorf("keys after Default = %v, want %v", got, want)
	}
	if be.Spec.Headers[2].ValueSecretRef != nil || none.Spec.Identity.TokenSecretRef != nil || none.Spec.Alerts != nil {
		t.Errorf("Default allocated a nil ref: %+v %+v", be.Spec, none.Spec)
	}
}

func TestChatConnectionCredentialsHaveNoKey(t *testing.T) {
	in := []byte("spec: { credentialsSecretRef: { name: slack-main, key: botToken } }")
	if err := yaml.UnmarshalStrict(in, &ChatConnection{}); err == nil {
		t.Fatal("strict decode accepted credentialsSecretRef.key, want an unknown-field error")
	}
}

func TestNamespacedName(t *testing.T) {
	if got := NamespacedName("sre", "sre-alerts"); got != "sre/sre-alerts" {
		t.Errorf("NamespacedName = %q; want sre/sre-alerts", got)
	}
}
