package v1alpha1

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// channelID matches Slack channel IDs; its literal must equal ChatBinding.Channel's Pattern marker.
var channelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

// IsReservedHeader reports whether name carries or could forge the Binding identity (D14),
// so an AgentBackend must not set it. Dialects refuse it too. X-Agent-Name is reserved
// because kagent lets it select the acting user via X-User-Id or ?user_id= (D32).
func IsReservedHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Proxy-Authorization", "Cookie", "X-User-Id", "X-Agent-Name":
		return true
	}
	return false
}

// problems collects validation failures, each prefixed with the path of the field.
type problems []error

func (p *problems) add(path, format string, args ...any) {
	*p = append(*p, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

func (p *problems) required(path, v string) {
	if v == "" {
		p.add(path, "required")
	}
}

func (p *problems) oneOf(path, v string, allowed ...string) {
	if !slices.Contains(allowed, v) {
		p.add(path, "unsupported value %q, must be one of %s", v, strings.Join(allowed, ", "))
	}
}

func (p *problems) positive(path string, n int64) {
	if n <= 0 {
		p.add(path, "must be positive")
	}
}

func (p *problems) duration(path string, d metav1.Duration) { p.positive(path, int64(d.Duration)) }

// name checks a required DNS-1123 subdomain, the form the API server enforces on names.
func (p *problems) name(path, v string) {
	if v == "" {
		p.add(path, "required")
	} else if len(validation.IsDNS1123Subdomain(v)) > 0 {
		p.add(path, "invalid name %q: must be at most 253 lowercase letters, digits, '-' or '.', starting and ending with a letter or digit", v)
	}
}

// namespace checks a required DNS-1123 label.
func (p *problems) namespace(path, v string) {
	if v == "" {
		p.add(path, "required")
	} else if len(validation.IsDNS1123Label(v)) > 0 {
		p.add(path, "invalid namespace %q: must be at most 63 lowercase letters, digits or '-', starting and ending with a letter or digit", v)
	}
}

func (p *problems) namespaces(path string, ns []string) {
	for i, n := range ns {
		p.namespace(fmt.Sprintf("%s[%d]", path, i), n)
	}
}

func (p *problems) meta(m metav1.ObjectMeta) {
	p.name("metadata.name", m.Name)
	p.namespace("metadata.namespace", m.Namespace)
}

func (p *problems) secretRef(path string, r *SecretKeyRef) {
	if r == nil {
		return
	}
	p.name(path+".name", r.Name)
	if r.Key != "" && len(validation.IsConfigMapKey(r.Key)) > 0 {
		p.add(path+".key", "invalid key %q: must consist of alphanumeric characters, '-', '_' or '.'", r.Key)
	}
}

func (p *problems) objectRef(path string, r *ObjectRef) {
	p.name(path+".name", r.Name)
	if r.Namespace != "" {
		p.namespace(path+".namespace", r.Namespace)
	}
}

// Validate reports every per-object rule the object breaks, joined, each naming the field
// path. Call Default first; Validate treats unset defaulted fields as invalid. It does not
// resolve references or check other objects (see WP-21).
func (a *AgentBackend) Validate() error {
	var p problems
	p.meta(a.ObjectMeta)
	s := &a.Spec
	p.oneOf("spec.dialect", string(s.Dialect), string(DialectA2A), string(DialectKagentV1))
	if s.URL == "" {
		p.add("spec.url", "required")
	} else if u, err := url.Parse(s.URL); err != nil {
		p.add("spec.url", "must be an http or https URL with a host")
	} else if u.User != nil {
		// net/http would turn userinfo into Basic auth, bypassing the Binding identity.
		p.add("spec.url", "must not contain credentials; identity comes from the Binding (D14)")
	} else if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.add("spec.url", "invalid URL %q: must be an http or https URL with a host", u.Redacted())
	}
	for i, h := range s.Headers {
		path := fmt.Sprintf("spec.headers[%d]", i)
		p.required(path+".name", h.Name)
		if IsReservedHeader(h.Name) {
			p.add(path+".name", "header %q is reserved for the Binding identity (D14)", h.Name)
		}
		if (h.Value == "") == (h.ValueSecretRef == nil) {
			p.add(path, "set exactly one of value and valueSecretRef")
		}
		p.secretRef(path+".valueSecretRef", h.ValueSecretRef)
	}
	p.namespaces("spec.allowedNamespaces", s.AllowedNamespaces)
	return errors.Join(p...)
}

// Validate reports every per-object rule the object breaks, joined, each naming the field
// path. Call Default first; Validate treats unset defaulted fields as invalid. It does not
// resolve references or check other objects (see WP-21).
func (c *ChatConnection) Validate() error {
	var p problems
	p.meta(c.ObjectMeta)
	s := &c.Spec
	p.oneOf("spec.platform", s.Platform, PlatformSlack)
	p.oneOf("spec.mode", string(s.Mode), string(ModeSocket), string(ModeHTTP))
	p.name("spec.credentialsSecretRef.name", s.CredentialsSecretRef.Name)
	if dm := s.DirectMessages; dm != nil && dm.DefaultBindingRef != nil {
		p.objectRef("spec.directMessages.defaultBindingRef", dm.DefaultBindingRef)
	}
	p.namespaces("spec.allowedNamespaces", s.AllowedNamespaces)
	return errors.Join(p...)
}

// Validate reports every per-object rule the object breaks, joined, each naming the field
// path. Call Default first; Validate treats unset defaulted fields as invalid. It does not
// resolve references or enforce one Binding per channel (see WP-21).
func (b *Binding) Validate() error {
	var p problems
	p.meta(b.ObjectMeta)
	s := &b.Spec
	p.objectRef("spec.agent.backendRef", &s.Agent.BackendRef)
	p.required("spec.agent.name", s.Agent.Name)
	p.namespace("spec.agent.namespace", s.Agent.Namespace)
	if s.Identity.UserID == "" && s.Identity.TokenSecretRef == nil {
		p.add("spec.identity", "set userId, tokenSecretRef, or both")
	}
	p.secretRef("spec.identity.tokenSecretRef", s.Identity.TokenSecretRef)
	p.objectRef("spec.chat.connectionRef", &s.Chat.ConnectionRef)
	if c := s.Chat.Channel; c == "" {
		p.add("spec.chat.channel", "required")
	} else if !channelID.MatchString(c) {
		p.add("spec.chat.channel", "invalid channel %q: must be a channel ID such as C0123456789, not a name", c)
	}
	p.oneOf("spec.chat.output", string(s.Chat.Output), string(OutputLive), string(OutputFinal))
	if a := s.Alerts; a != nil {
		p.oneOf("spec.alerts.type", a.Type, AlertAlertmanager)
		p.secretRef("spec.alerts.tokenSecretRef", &a.TokenSecretRef)
		p.required("spec.alerts.promptTemplate", a.PromptTemplate)
		if r := a.RateLimit; r != nil {
			p.positive("spec.alerts.rateLimit.maxRuns", int64(r.MaxRuns))
			p.duration("spec.alerts.rateLimit.per", r.Per)
			p.positive("spec.alerts.rateLimit.maxConcurrent", int64(r.MaxConcurrent))
		}
		if a.MaxRunsPerDay < 0 {
			p.add("spec.alerts.maxRunsPerDay", "must not be negative")
		}
	}
	p.duration("spec.interactions.timeout", s.Interactions.Timeout)
	p.duration("spec.session.threadTTL", s.Session.ThreadTTL)
	return errors.Join(p...)
}
