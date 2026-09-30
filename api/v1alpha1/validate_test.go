package v1alpha1

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// validatable is the behavior shared by the three kinds; PT is *T.
type validatable[T any] interface {
	*T
	Default()
	Validate() error
}

type mutation[T any] struct {
	name   string
	mutate func(*T)
	want   string // the one expected problem, "path: message"; empty means the object must validate
}

// problemsOf unwraps an errors.Join result into its individual messages.
func problemsOf(err error) []string {
	var j interface{ Unwrap() []error }
	if !errors.As(err, &j) {
		return nil
	}
	var out []string
	for _, e := range j.Unwrap() {
		out = append(out, e.Error())
	}
	return out
}

func badName(path, v string) string {
	return fmt.Sprintf("%s: invalid name %q: must be at most 253 lowercase letters, digits, '-' or '.', starting and ending with a letter or digit", path, v)
}

func badNamespace(path, v string) string {
	return fmt.Sprintf("%s: invalid namespace %q: must be at most 63 lowercase letters, digits or '-', starting and ending with a letter or digit", path, v)
}

func badKey(path, v string) string {
	return fmt.Sprintf("%s: invalid key %q: must consist of alphanumeric characters, '-', '_' or '.'", path, v)
}

// runMutations applies each mutation to a fresh copy of the fixture and requires Validate to
// report exactly the expected problem, or none.
func runMutations[T any, PT validatable[T]](t *testing.T, file string, cases []mutation[T]) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := load[T, PT](t, file)
			tc.mutate(obj)
			var want []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if got := problemsOf(PT(obj).Validate()); !slices.Equal(got, want) {
				t.Fatalf("Validate() problems = %q, want %q", got, want)
			}
		})
	}
}

func TestSpecExamplesValidate(t *testing.T) {
	if err := load[AgentBackend](t, "agentbackend.yaml").Validate(); err != nil {
		t.Errorf("AgentBackend: %v", err)
	}
	if err := load[ChatConnection](t, "chatconnection.yaml").Validate(); err != nil {
		t.Errorf("ChatConnection: %v", err)
	}
	for _, f := range []string{"binding.yaml", "binding-minimal.yaml"} {
		if err := load[Binding](t, f).Validate(); err != nil {
			t.Errorf("Binding %s: %v", f, err)
		}
	}
}

func TestValidateJoinsAllProblems(t *testing.T) {
	b := load[Binding](t, "binding.yaml")
	b.Spec.Chat.Channel = ""
	b.Spec.Alerts.RateLimit.Per = metav1.Duration{}
	want := []string{"spec.chat.channel: required", "spec.alerts.rateLimit.per: must be positive"}
	if got := problemsOf(b.Validate()); !slices.Equal(got, want) {
		t.Fatalf("Validate() problems = %q, want %q", got, want)
	}
}

func TestAgentBackendValidate(t *testing.T) {
	hdr := func(o *AgentBackend, h Header) { o.Spec.Headers = []Header{h} }
	runMutations[AgentBackend](t, "agentbackend.yaml", []mutation[AgentBackend]{
		{"bad dialect", func(o *AgentBackend) { o.Spec.Dialect = "foo" }, `spec.dialect: unsupported value "foo", must be one of a2a, kagent-v1`},
		{"generic dialect ok", func(o *AgentBackend) { o.Spec.Dialect = DialectA2A }, ""},
		{"empty url", func(o *AgentBackend) { o.Spec.URL = "" }, "spec.url: required"},
		{"url without http scheme", func(o *AgentBackend) { o.Spec.URL = "ftp://kagent" },
			`spec.url: invalid URL "ftp://kagent": must be an http or https URL with a host`},
		{"url with credentials", func(o *AgentBackend) { o.Spec.URL = "http://user:pw@kagent" },
			"spec.url: must not contain credentials; identity comes from the Binding (D14)"},
		{"missing name", func(o *AgentBackend) { o.Name = "" }, "metadata.name: required"},
		{"missing namespace", func(o *AgentBackend) { o.Namespace = "" }, "metadata.namespace: required"},
		{"invalid name", func(o *AgentBackend) { o.Name = "Kagent_Pilot" }, badName("metadata.name", "Kagent_Pilot")},
		{"header without name", func(o *AgentBackend) { hdr(o, Header{Value: "v"}) }, "spec.headers[0].name: required"},
		{"header with no value", func(o *AgentBackend) { hdr(o, Header{Name: "X-A"}) },
			"spec.headers[0]: set exactly one of value and valueSecretRef"},
		{"header with both values", func(o *AgentBackend) {
			hdr(o, Header{Name: "X-A", Value: "v", ValueSecretRef: &SecretKeyRef{Name: "s"}})
		}, "spec.headers[0]: set exactly one of value and valueSecretRef"},
		{"reserved Authorization header", func(o *AgentBackend) { hdr(o, Header{Name: "authorization", Value: "v"}) },
			`spec.headers[0].name: header "authorization" is reserved for the Binding identity (D14)`},
		{"reserved Proxy-Authorization header", func(o *AgentBackend) { hdr(o, Header{Name: "proxy-authorization", Value: "v"}) },
			`spec.headers[0].name: header "proxy-authorization" is reserved for the Binding identity (D14)`},
		{"reserved Cookie header", func(o *AgentBackend) { hdr(o, Header{Name: "COOKIE", Value: "v"}) },
			`spec.headers[0].name: header "COOKIE" is reserved for the Binding identity (D14)`},
		{"reserved X-User-Id header", func(o *AgentBackend) { hdr(o, Header{Name: "X-USER-ID", Value: "v"}) },
			`spec.headers[0].name: header "X-USER-ID" is reserved for the Binding identity (D14)`},
		{"reserved X-Agent-Name header", func(o *AgentBackend) { hdr(o, Header{Name: "X-Agent-Name", Value: "v"}) },
			`spec.headers[0].name: header "X-Agent-Name" is reserved for the Binding identity (D14)`},
		{"reserved x-agent-name header, lower case", func(o *AgentBackend) { hdr(o, Header{Name: "x-agent-name", Value: "v"}) },
			`spec.headers[0].name: header "x-agent-name" is reserved for the Binding identity (D14)`},
		{"reserved X-Share-Token header", func(o *AgentBackend) { hdr(o, Header{Name: "X-Share-Token", Value: "v"}) },
			`spec.headers[0].name: header "X-Share-Token" is reserved for the Binding identity (D14)`},
		{"reserved x-share-token header", func(o *AgentBackend) { hdr(o, Header{Name: "x-share-token", Value: "v"}) },
			`spec.headers[0].name: header "x-share-token" is reserved for the Binding identity (D14)`},
		{"reserved X-Kagent-Insecure-Runtime-Identity header", func(o *AgentBackend) { hdr(o, Header{Name: "X-Kagent-Insecure-Runtime-Identity", Value: "v"}) },
			`spec.headers[0].name: header "X-Kagent-Insecure-Runtime-Identity" is reserved for the Binding identity (D14)`},
		{"reserved x-kagent-insecure-runtime-identity header", func(o *AgentBackend) { hdr(o, Header{Name: "x-kagent-insecure-runtime-identity", Value: "v"}) },
			`spec.headers[0].name: header "x-kagent-insecure-runtime-identity" is reserved for the Binding identity (D14)`},
		{"header from secret ok", func(o *AgentBackend) { hdr(o, Header{Name: "X-A", ValueSecretRef: &SecretKeyRef{Name: "s", Key: "k"}}) }, ""},
		{"header secret with bad name", func(o *AgentBackend) { hdr(o, Header{Name: "X-A", ValueSecretRef: &SecretKeyRef{Name: "Bad_Name"}}) },
			badName("spec.headers[0].valueSecretRef.name", "Bad_Name")},
		{"header secret with bad key", func(o *AgentBackend) {
			hdr(o, Header{Name: "X-A", ValueSecretRef: &SecretKeyRef{Name: "s", Key: "a/b"}})
		},
			badKey("spec.headers[0].valueSecretRef.key", "a/b")},
		{"bad allowed namespace", func(o *AgentBackend) { o.Spec.AllowedNamespaces = []string{"sre", "Not_A_Namespace"} },
			badNamespace("spec.allowedNamespaces[1]", "Not_A_Namespace")},
		{"empty allowed namespace", func(o *AgentBackend) { o.Spec.AllowedNamespaces = []string{""} }, "spec.allowedNamespaces[0]: required"},
	})
}

func TestChatConnectionValidate(t *testing.T) {
	runMutations[ChatConnection](t, "chatconnection.yaml", []mutation[ChatConnection]{
		{"bad platform", func(o *ChatConnection) { o.Spec.Platform = "teams" }, `spec.platform: unsupported value "teams", must be one of slack`},
		{"bad mode", func(o *ChatConnection) { o.Spec.Mode = "carrier-pigeon" },
			`spec.mode: unsupported value "carrier-pigeon", must be one of socket, http`},
		{"http mode ok", func(o *ChatConnection) { o.Spec.Mode = ModeHTTP }, ""},
		{"credentials without name", func(o *ChatConnection) { o.Spec.CredentialsSecretRef = SecretRef{} }, "spec.credentialsSecretRef.name: required"},
		{"credentials with bad name", func(o *ChatConnection) { o.Spec.CredentialsSecretRef.Name = "Slack_Main" },
			badName("spec.credentialsSecretRef.name", "Slack_Main")},
		{"no direct messages ok", func(o *ChatConnection) { o.Spec.DirectMessages = nil }, ""},
		{"default binding without name", func(o *ChatConnection) {
			o.Spec.DirectMessages.DefaultBindingRef = &ObjectRef{Namespace: "sre"}
		}, "spec.directMessages.defaultBindingRef.name: required"},
		{"default binding with bad namespace", func(o *ChatConnection) {
			o.Spec.DirectMessages.DefaultBindingRef = &ObjectRef{Namespace: "Bad_NS", Name: "b"}
		}, badNamespace("spec.directMessages.defaultBindingRef.namespace", "Bad_NS")},
		{"empty allowed namespace", func(o *ChatConnection) { o.Spec.AllowedNamespaces = []string{""} }, "spec.allowedNamespaces[0]: required"},
	})
}

func TestBindingValidate(t *testing.T) {
	runMutations[Binding](t, "binding.yaml", []mutation[Binding]{
		{"missing name", func(o *Binding) { o.Name = "" }, "metadata.name: required"},
		{"agent without name", func(o *Binding) { o.Spec.Agent.Name = "" }, "spec.agent.name: required"},
		{"agent without namespace", func(o *Binding) { o.Spec.Agent.Namespace = "" }, "spec.agent.namespace: required"},
		{"agent with bad namespace", func(o *Binding) { o.Spec.Agent.Namespace = "Bad_NS" }, badNamespace("spec.agent.namespace", "Bad_NS")},
		{"backendRef without name", func(o *Binding) { o.Spec.Agent.BackendRef.Name = "" }, "spec.agent.backendRef.name: required"},
		{"backendRef with bad name", func(o *Binding) { o.Spec.Agent.BackendRef.Name = "Bad_Name" },
			badName("spec.agent.backendRef.name", "Bad_Name")},
		{"backendRef with bad namespace", func(o *Binding) { o.Spec.Agent.BackendRef.Namespace = "Bad_NS" },
			badNamespace("spec.agent.backendRef.namespace", "Bad_NS")},
		{"backendRef without namespace ok", func(o *Binding) { o.Spec.Agent.BackendRef.Namespace = "" }, ""},
		{"no identity", func(o *Binding) { o.Spec.Identity = Identity{} }, "spec.identity: set userId, tokenSecretRef, or both"},
		{"token-only identity ok", func(o *Binding) { o.Spec.Identity.UserID = "" }, ""},
		{"identity token without name", func(o *Binding) { o.Spec.Identity.TokenSecretRef.Name = "" }, "spec.identity.tokenSecretRef.name: required"},
		{"identity token with bad key", func(o *Binding) { o.Spec.Identity.TokenSecretRef.Key = "a b" },
			badKey("spec.identity.tokenSecretRef.key", "a b")},
		{"connectionRef without name", func(o *Binding) { o.Spec.Chat.ConnectionRef.Name = "" }, "spec.chat.connectionRef.name: required"},
		{"empty channel", func(o *Binding) { o.Spec.Chat.Channel = "" }, "spec.chat.channel: required"},
		{"channel name instead of ID", func(o *Binding) { o.Spec.Chat.Channel = "#sre-alerts" },
			`spec.chat.channel: invalid channel "#sre-alerts": must be a channel ID such as C0123456789, not a name`},
		{"bad output", func(o *Binding) { o.Spec.Chat.Output = "carrier-pigeon" },
			`spec.chat.output: unsupported value "carrier-pigeon", must be one of live, final`},
		{"final output ok", func(o *Binding) { o.Spec.Chat.Output = OutputFinal }, ""},
		{"bad alert type", func(o *Binding) { o.Spec.Alerts.Type = "grafana" }, `spec.alerts.type: unsupported value "grafana", must be one of alertmanager`},
		{"alerts without tokenSecretRef", func(o *Binding) { o.Spec.Alerts.TokenSecretRef = SecretKeyRef{} }, "spec.alerts.tokenSecretRef.name: required"},
		{"alerts without prompt", func(o *Binding) { o.Spec.Alerts.PromptTemplate = "" }, "spec.alerts.promptTemplate: required"},
		{"maxRuns zero", func(o *Binding) { o.Spec.Alerts.RateLimit.MaxRuns = 0 }, "spec.alerts.rateLimit.maxRuns: must be positive"},
		{"per negative", func(o *Binding) { o.Spec.Alerts.RateLimit.Per.Duration = -time.Minute }, "spec.alerts.rateLimit.per: must be positive"},
		{"maxConcurrent negative", func(o *Binding) { o.Spec.Alerts.RateLimit.MaxConcurrent = -1 }, "spec.alerts.rateLimit.maxConcurrent: must be positive"},
		{"negative maxRunsPerDay", func(o *Binding) { o.Spec.Alerts.MaxRunsPerDay = -1 }, "spec.alerts.maxRunsPerDay: must not be negative"},
		{"zero maxRunsPerDay means uncapped", func(o *Binding) { o.Spec.Alerts.MaxRunsPerDay = 0 }, ""},
		{"no rate limit ok", func(o *Binding) { o.Spec.Alerts.RateLimit = nil }, ""},
		{"chat-only binding ok", func(o *Binding) { o.Spec.Alerts = nil }, ""},
		{"toolApprovals without askUser ok", func(o *Binding) {
			o.Spec.Interactions.AskUser, o.Spec.Interactions.ToolApprovals = false, true
		}, ""},
		{"negative interaction timeout", func(o *Binding) { o.Spec.Interactions.Timeout.Duration = -1 }, "spec.interactions.timeout: must be positive"},
		{"negative threadTTL", func(o *Binding) { o.Spec.Session.ThreadTTL.Duration = -1 }, "spec.session.threadTTL: must be positive"},
	})
}
