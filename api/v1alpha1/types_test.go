package v1alpha1

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	if b.Spec.Chat.ThreadReplies != ThreadRepliesAll {
		t.Errorf("Binding threadReplies = %q, want %q", b.Spec.Chat.ThreadReplies, ThreadRepliesAll)
	}
	if b.Spec.Identity.UserID != "agent-kourier-sre-alerts" || b.Spec.Chat.Channel != "C0123456789" ||
		!b.Spec.Interactions.AskUser || !b.Spec.Feedback.Enabled {
		t.Errorf("Binding did not decode as in the spec: %+v", b)
	}
}

func TestDefaultThreadReplies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in, want ThreadReplies
	}{
		{"omitted", "", ThreadRepliesAll},
		{"all kept", ThreadRepliesAll, ThreadRepliesAll},
		{"mention kept", ThreadRepliesMention, ThreadRepliesMention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Binding{Spec: BindingSpec{Chat: ChatBinding{ThreadReplies: tc.in}}}
			b.Default()
			if got := b.Spec.Chat.ThreadReplies; got != tc.want {
				t.Errorf("ThreadReplies after Default = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDefault(t *testing.T) {
	b := load[Binding](t, "binding-minimal.yaml")
	if b.Spec.Chat.Output != OutputLive || b.Spec.Interactions.Timeout.Duration != 30*time.Minute ||
		b.Spec.Session.ThreadTTL.Duration != 2160*time.Hour || b.Spec.Interactions.ToolApprovals || b.Spec.Feedback.Enabled {
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
	unset := &Binding{Spec: BindingSpec{Identity: Identity{TokenSecretRef: ref("")}}}
	set := &Binding{Spec: BindingSpec{Identity: Identity{TokenSecretRef: ref("custom")}}}
	var none Binding
	for _, d := range []interface{ Default() }{be, unset, set, &none} {
		d.Default()
	}

	got := []string{
		be.Spec.Headers[0].ValueSecretRef.Key, be.Spec.Headers[1].ValueSecretRef.Key,
		unset.Spec.Identity.TokenSecretRef.Key, set.Spec.Identity.TokenSecretRef.Key,
	}
	if want := []string{"token", "custom", "token", "custom"}; !slices.Equal(got, want) {
		t.Errorf("keys after Default = %v, want %v", got, want)
	}
	if be.Spec.Headers[2].ValueSecretRef != nil || none.Spec.Identity.TokenSecretRef != nil {
		t.Errorf("Default allocated a nil ref: %+v %+v", be.Spec, none.Spec)
	}
}

// A Binding that carries the removed alerts block (a per-Binding Alertmanager webhook) or status.webhookPath is
// refused by the strict decoder: alerts reach a Binding through a botMessage trigger only (DA-1).
func TestBindingRefusesTheRemovedWebhookFields(t *testing.T) {
	for _, tc := range []struct{ name, in, field string }{
		{"alerts", "spec: { alerts: { type: alertmanager, promptTemplate: p } }", "alerts"},
		{"status.webhookPath", "status: { webhookPath: /hooks/alertmanager/sre/sre-alerts }", "webhookPath"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := yaml.UnmarshalStrict([]byte(tc.in), &Binding{})
			if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "unknown field") {
				t.Errorf("strict decode error = %v, want an unknown-field error naming %s", err, tc.field)
			}
		})
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
