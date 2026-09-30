package kagentv1

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2aext"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

const (
	// HITLExtension is the URI of kagent's human-in-the-loop extension.
	HITLExtension = "https://kagent.dev/extensions/hitl/v1"
	// DefaultResponseTimeout is how long a backend may take to start answering.
	DefaultResponseTimeout = 30 * time.Second

	cardPath = "/.well-known/agent-card.json"
)

// New returns a Dialect whose calls must get their response headers within responseTimeout;
// zero means DefaultResponseTimeout. That bounds each call's start, not its length: a
// stream (SendStreamingMessage, SubscribeToTask) is bounded only by its caller's context.
// oauth2-proxy's --upstream-timeout (10m in the pilot) bounds the wait for response
// headers, not a stream's length (medium-high confidence, from review; not measured). What
// to do when a stream ends early is the caller's concern (WP-27).
func New(responseTimeout time.Duration) *Dialect {
	if responseTimeout <= 0 {
		responseTimeout = DefaultResponseTimeout
	}
	return &Dialect{base: &http.Transport{
		// No Proxy, on purpose: a nil Proxy dials the front door directly. Honoring
		// HTTP_PROXY would let an injected environment variable receive the Binding's bearer
		// token in cleartext.
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseTimeout,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}}
}

// Connect resolves the agent's card through the front door and returns an A2A client for it.
// It fetches the card from the full .well-known URL, then points every JSON-RPC interface
// at the front door and drops the others: the card advertises the controller port (:8083),
// which is fenced and does not verify identity. The client asks for the HITL extension
// whenever the card lists it, and sends the Binding's identity on every request, only to the
// front door. A rejected credential fails here, at the card fetch. The Dialect must come
// from New.
func (d *Dialect) Connect(ctx context.Context, backend *v1alpha1.AgentBackend, agent v1alpha1.AgentRef, creds dialect.Credentials) (*a2aclient.Client, error) {
	if d.base == nil {
		return nil, errors.New("kagentv1: Connect needs a Dialect built with New")
	}
	endpoint, err := d.Endpoint(backend, agent)
	if err != nil {
		return nil, fmt.Errorf("connect to agent %s/%s: %w", agent.Namespace, agent.Name, err)
	}
	frontDoor, err := url.Parse(endpoint) // valid: Endpoint checked it, and it holds no credentials
	if err != nil {
		return nil, fmt.Errorf("connect to agent %s/%s: %w", agent.Namespace, agent.Name, err)
	}
	hc := d.httpClient(creds, frontDoor)
	card, err := agentcard.NewResolver(hc).Resolve(ctx, endpoint+cardPath)
	if err != nil {
		return nil, fmt.Errorf("resolve card of agent %s/%s: %w", agent.Namespace, agent.Name, err)
	}
	card.SupportedInterfaces = slices.DeleteFunc(card.SupportedInterfaces, func(i *a2a.AgentInterface) bool {
		return i == nil || i.ProtocolBinding != a2a.TransportProtocolJSONRPC
	})
	if len(card.SupportedInterfaces) == 0 {
		return nil, fmt.Errorf("card of agent %s/%s lists no JSON-RPC interface", agent.Namespace, agent.Name)
	}
	for _, i := range card.SupportedInterfaces {
		i.URL = endpoint
	}
	// Defaults off, so JSON-RPC over hc is the only transport the client can pick.
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithCallInterceptors(a2aext.NewActivator(HITLExtension)),
		a2aclient.WithJSONRPCTransport(hc))
	if err != nil {
		return nil, fmt.Errorf("create client for agent %s/%s: %w", agent.Namespace, agent.Name, err)
	}
	return client, nil
}

// httpClient sends every request with the Binding's identity, and only to frontDoor's
// scheme and host. It sets no total timeout, which would cut streams, and it follows no
// redirect, which would carry the token to wherever the redirect points.
func (d *Dialect) httpClient(creds dialect.Credentials, frontDoor *url.URL) *http.Client {
	return &http.Client{
		Transport:     authTransport{next: d.base, dialect: d, creds: creds, frontDoor: frontDoor},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// authTransport authenticates each request, on a copy, before sending it to the front door.
type authTransport struct {
	next      http.RoundTripper
	dialect   *Dialect
	creds     dialect.Credentials
	frontDoor *url.URL
}

func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	refuse := func(err error) (*http.Response, error) {
		if req.Body != nil {
			_ = req.Body.Close() // RoundTrip owns the body even on error
		}
		return nil, err
	}
	if req.URL.Scheme != t.frontDoor.Scheme || !strings.EqualFold(req.URL.Host, t.frontDoor.Host) {
		return refuse(fmt.Errorf("refusing to send credentials to %s://%s, which is not the front door", req.URL.Scheme, req.URL.Host))
	}
	req = req.Clone(req.Context())
	if err := t.dialect.Authenticate(req, t.creds); err != nil {
		return refuse(err)
	}
	return t.next.RoundTrip(req)
}
