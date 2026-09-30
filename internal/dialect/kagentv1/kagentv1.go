// Package kagentv1 is the dialect for kagent 1.0: A2A over JSON-RPC at
// POST /agents/{ns}/{name}, with the card at .well-known/agent-card.json, and the
// human-in-the-loop extension (spec 6.3). Courier reaches kagent only through its verifying
// front door, never the controller port, which trusts identity without checking it (D32).
package kagentv1

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

const (
	// ActorMetadataKey is the message metadata key that holds the acting user (spec 8.5).
	ActorMetadataKey = "agentcourier.dev/actor"

	maxActorRunes = 64
)

// Dialect implements dialect.Dialect for kagent 1.0. Build it with New to use Connect; the
// zero value serves Endpoint, Authenticate, and Attribute. It is safe for concurrent use.
type Dialect struct {
	base http.RoundTripper // shared by every client, so connections are pooled per host
}

var _ dialect.Dialect = (*Dialect)(nil)

// Endpoint returns <backend url>/agents/{namespace}/{name}. The backend URL must be the
// verifying front door's.
func (*Dialect) Endpoint(backend *v1alpha1.AgentBackend, agent v1alpha1.AgentRef) (string, error) {
	u, err := url.Parse(backend.Spec.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return "", errors.New("backend url must be an http or https URL with a host") // not wrapped: the URL may hold credentials
	case u.User != nil:
		return "", errors.New("backend url must not contain credentials; identity comes from the Binding (D14)")
	case u.RawQuery != "" || u.Fragment != "":
		return "", errors.New("backend url must not have a query or fragment")
	case len(validation.IsDNS1123Label(agent.Namespace)) > 0 || len(validation.IsDNS1123Subdomain(agent.Name)) > 0:
		return "", fmt.Errorf("agent %q in namespace %q is not a valid resource name", agent.Name, agent.Namespace)
	}
	return u.JoinPath("agents", agent.Namespace, agent.Name).String(), nil
}

// Authenticate sets Authorization: Bearer when creds has a token, and otherwise X-User-Id,
// never both, and sets the static headers. Static headers that would set Authorization,
// Proxy-Authorization, Cookie, X-User-Id, or X-Agent-Name are refused: they carry or forge
// the Binding identity (D14, D32; Validate rejects them earlier). Errors never contain a credential.
func (*Dialect) Authenticate(req *http.Request, creds dialect.Credentials) error {
	for name := range creds.Headers {
		if v1alpha1.IsReservedHeader(name) {
			return fmt.Errorf("static header %q is reserved for the Binding identity (D14)", name)
		}
	}
	hasControl := func(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }
	switch {
	case creds.Token == "" && creds.UserID == "":
		return errors.New("credentials carry no identity: set a token or a user ID")
	case hasControl(creds.Token) || hasControl(creds.UserID):
		return errors.New("credentials contain a control character")
	}
	for name := range creds.Headers { // set, not add, so a second call changes nothing
		req.Header.Del(name)
	}
	for name, values := range creds.Headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Del("Authorization")
	req.Header.Del("X-User-Id")
	if creds.Token != "" {
		req.Header.Set("Authorization", "Bearer "+creds.Token)
	} else {
		req.Header.Set("X-User-Id", creds.UserID)
	}
	return nil
}

// Attribute records the acting user as message metadata (ActorMetadataKey) and as a prefix
// the agent can read, such as "[Slack: Jane Doe (U123) <jane@example.com>]", at the start of
// the first text part (its own part only when the message has no text). It changes msg in
// place, so call it once per message. Names come from users, so the prefix
// drops control and format characters (bidi overrides, zero-width) and brackets, and is
// cut short. An actor with no ID is ignored. A HITL answer (see BuildResponse) gets the
// metadata but no text part.
func (*Dialect) Attribute(msg *a2a.Message, actor dialect.User) {
	if actor.ID == "" {
		return
	}
	who, meta := clean(actor.ID), map[string]any{"id": actor.ID}
	if name := clean(actor.Name); name != "" {
		who, meta["name"] = name+" ("+who+")", name
	}
	if email := clean(actor.Email); email != "" {
		who, meta["email"] = who+" <"+email+">", email
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	msg.Metadata[ActorMetadataKey] = meta
	if _, reply := msg.Metadata[HITLExtension]; reply {
		return // a structured answer keeps its one measured shape; the actor is in metadata
	}
	msg.Parts = withPrefix(msg.Parts, "[Slack: "+who+"]")
}

// withPrefix returns parts with prefix at the start of the first text part, then a newline.
// Without a text part it adds one holding only the prefix. kagent's harness runtime refuses a
// user message unless it has exactly one text part (spec R18). It returns a new slice and
// replaces the edited part, because the caller may share both.
func withPrefix(parts a2a.ContentParts, prefix string) a2a.ContentParts {
	for i, p := range parts {
		if text, ok := p.Content.(a2a.Text); ok {
			edited := *p
			edited.Content = a2a.Text(prefix + "\n" + string(text))
			edited.Metadata = maps.Clone(p.Metadata)
			out := append(a2a.ContentParts(nil), parts...)
			out[i] = &edited
			return out
		}
	}
	return append(a2a.ContentParts{a2a.NewTextPart(prefix)}, parts...)
}

// clean makes s safe to show in the attribution prefix.
func clean(s string) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return ' '
		case unicode.Is(unicode.Cf, r), strings.ContainsRune("[]<>()［］＜＞（）〈〉《》", r):
			return -1
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > maxActorRunes {
		s = string(r[:maxActorRunes])
	}
	return s
}
