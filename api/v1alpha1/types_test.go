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
	if cc.Spec.Platform != PlatformSlack || cc.Spec.CredentialsSecretRef.Name != "slack-main" {
		t.Errorf("ChatConnection did not decode as in the spec: %+v", cc.Spec)
	}

	b := load[Binding](t, "binding.yaml")
	if b.Spec.Chat.ThreadReplies != ThreadRepliesAll {
		t.Errorf("Binding threadReplies = %q, want %q", b.Spec.Chat.ThreadReplies, ThreadRepliesAll)
	}
	if b.Spec.Chat.Acknowledgment == nil || b.Spec.Chat.Acknowledgment.Enabled == nil || !*b.Spec.Chat.Acknowledgment.Enabled || b.Spec.Chat.Acknowledgment.Emoji != "" {
		t.Errorf("Binding acknowledgment defaults = %+v, want enabled with no emoji: the platform supplies its own", b.Spec.Chat.Acknowledgment)
	}
	if b.Spec.Identity.UserID != "agent-kourier-sre-alerts" || b.Spec.Chat.Channel != "C0123456789" ||
		!b.Spec.Interactions.AskUser || b.Spec.Interactions.ToolApprovals || len(b.Spec.Interactions.ApproverGroups) != 0 || !b.Spec.Feedback.Enabled {
		t.Errorf("Binding did not decode as in the spec: %+v", b)
	}
}

func TestToolApprovalFieldsDecode(t *testing.T) {
	var b Binding
	if err := yaml.UnmarshalStrict([]byte(`spec:
  interactions:
    toolApprovals: true
    approverGroups: [S0123456789, S9876543210]
`), &b); err != nil {
		t.Fatal(err)
	}
	b.Default()
	if !b.Spec.Interactions.ToolApprovals || !slices.Equal(b.Spec.Interactions.ApproverGroups, []string{"S0123456789", "S9876543210"}) {
		t.Errorf("tool approval interactions = %+v; want enabled approvals and the two groups", b.Spec.Interactions)
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

func TestDefaultAcknowledgment(t *testing.T) {
	enabled := false
	for _, tc := range []struct {
		name string
		in   *ChatAcknowledgment
		want bool
	}{
		{"omitted", nil, true},
		{"enabled", &ChatAcknowledgment{Enabled: boolPtr(true)}, true},
		{"disabled", &ChatAcknowledgment{Enabled: &enabled}, false},
		{"custom emoji", &ChatAcknowledgment{Enabled: boolPtr(true), Emoji: "eyes"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Binding{Spec: BindingSpec{Chat: ChatBinding{Acknowledgment: tc.in}}}
			b.Default()
			got := b.Spec.Chat.Acknowledgment
			if got == nil || got.Enabled == nil || *got.Enabled != tc.want {
				t.Fatalf("acknowledgment after Default = %+v; want enabled=%t", got, tc.want)
			}
			// The API names no reaction: an emoji stays as written, and an empty one stays empty for the platform to fill.
			wantEmoji := ""
			if tc.in != nil {
				wantEmoji = tc.in.Emoji
			}
			if got.Emoji != wantEmoji {
				t.Errorf("emoji after Default = %q, want %q", got.Emoji, wantEmoji)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func TestDefault(t *testing.T) {
	b := load[Binding](t, "binding-minimal.yaml")
	if b.Spec.Chat.Output != OutputLive || b.Spec.Interactions.Timeout.Duration != 30*time.Minute || b.Spec.Interactions.ToolApprovals || len(b.Spec.Interactions.ApproverGroups) != 0 ||
		b.Spec.Session.ThreadTTL.Duration != 2160*time.Hour || b.Spec.Feedback.Enabled {
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

// A Binding that carries the removed alerts block (a per-Binding Alertmanager webhook) is
// refused by the strict decoder: alerts reach a Binding through a botMessage trigger only (DA-1).
func TestBindingRefusesTheRemovedWebhookFields(t *testing.T) {
	for _, tc := range []struct{ name, in, field string }{
		{"alerts", "spec: { alerts: { type: alertmanager, promptTemplate: p } }", "alerts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := yaml.UnmarshalStrict([]byte(tc.in), &Binding{})
			if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "unknown field") {
				t.Errorf("strict decode error = %v, want an unknown-field error naming %s", err, tc.field)
			}
		})
	}
}

// A field stays in the API only if it works (DA-3). The fields of features that are not built are refused by the strict
// decoder, and return with the feature: the Binding's status and the ChatConnection's mode and directMessages.
func TestTheFieldsOfUnbuiltFeaturesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name, in, field string
		into            any
	}{
		{"status", "status: { conditions: [] }", "status", &Binding{}},
		{"mode", "spec: { mode: socket }", "mode", &ChatConnection{}},
		{"directMessages", "spec: { directMessages: { defaultBindingRef: { name: b } } }", "directMessages", &ChatConnection{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := yaml.UnmarshalStrict([]byte(tc.in), tc.into)
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
