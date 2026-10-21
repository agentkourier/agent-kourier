package kagentv1

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
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

// CloseIdleConnections closes the idle keep-alive connections of the transport that every
// client of this Dialect shares, and the goroutines that serve them. Call it after the clients
// are done, at shutdown: an idle connection otherwise lives until its idle timeout.
func (d *Dialect) CloseIdleConnections() {
	if c, ok := d.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// cardParser reads an agent card as A2A 1.0 and, when that lists no interface, as A2A 0.3: one
// `url` and a `preferredTransport`, with no protocolVersion, which is what kagent 0.9.x serves.
// The 0.3 reader maps it onto the 1.0 shape, so the checks in ConnectEndpoint see one kind of card.
func cardParser(body []byte) (*a2a.AgentCard, error) {
	card, err := agentcard.DefaultCardParser(body)
	if err == nil && len(card.SupportedInterfaces) > 0 {
		return card, nil
	}
	legacy, legacyErr := a2av0.NewAgentCardParser()(body)
	switch {
	case legacyErr == nil:
		return legacy, nil
	case err == nil:
		return card, nil // read as 1.0 but lists no interface: ConnectEndpoint's own check says so
	}
	return nil, err
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
		return nil, fmt.Errorf("connect to endpoint: %w", err)
	}
	return d.ConnectEndpoint(ctx, endpoint, creds, HITLExtension)
}

// ConnectEndpoint connects to an exact A2A endpoint with the same credential protections as kagent.
// extensions lists the extensions this caller understands; an empty list negotiates none.
func (d *Dialect) ConnectEndpoint(ctx context.Context, endpoint string, creds dialect.Credentials, extensions ...string) (*a2aclient.Client, error) {
	if d.base == nil {
		return nil, errors.New("a2a: Connect needs a dialect built with New")
	}
	frontDoor, err := url.Parse(endpoint) // valid: Endpoint checked it, and it holds no credentials
	if err != nil {
		return nil, fmt.Errorf("connect to endpoint: %w", err)
	}
	hc := d.httpClient(creds, frontDoor)
	resolver := agentcard.NewResolver(hc)
	resolver.CardParser = cardParser
	card, err := resolver.Resolve(ctx, strings.TrimRight(endpoint, "/")+cardPath)
	if err != nil {
		return nil, fmt.Errorf("resolve agent card: %w", err)
	}
	for _, ext := range card.Capabilities.Extensions {
		if ext.Required && !slices.Contains(extensions, ext.URI) {
			return nil, errors.New("agent requires an unsupported extension")
		}
	}
	card.SupportedInterfaces = slices.DeleteFunc(card.SupportedInterfaces, func(i *a2a.AgentInterface) bool {
		return i == nil || i.ProtocolBinding != a2a.TransportProtocolJSONRPC
	})
	if len(card.SupportedInterfaces) == 0 {
		return nil, errors.New("agent card lists no JSON-RPC interface")
	}
	for _, i := range card.SupportedInterfaces {
		i.URL = endpoint
	}
	// Defaults off, so JSON-RPC over hc is the only transport the client can pick: the 1.0 one, and
	// the 0.3 one for an agent whose interface is 0.3 (kagent 0.9.x), chosen by the interface's version.
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithCallInterceptors(a2aext.NewActivator(extensions...), messageExtensions{offered: extensions}),
		a2aclient.WithJSONRPCTransport(hc),
		a2av0.WithJSONRPCTransport(a2av0.JSONRPCTransportConfig{Client: hc}))
	if err != nil {
		return nil, fmt.Errorf("create agent client: %w", err)
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

// authTransport authenticates each request, on a copy, before sending it to the front door,
// and turns a response that is not 2xx into a dialect.HTTPStatusError.
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
	resp, err := telemetry.Transport(t.next, true).RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, &dialect.HTTPStatusError{Code: resp.StatusCode, RetryAfter: retryAfter(resp.Header)}
	}
	return resp, nil
}

// retryAfter reads a Retry-After header, which is a number of seconds or an HTTP date. It
// returns 0 for anything else, and for a delay that is not in the future, and caps it at a
// day so a hostile header cannot overflow or park a caller.
func retryAfter(h http.Header) time.Duration {
	const day = 24 * time.Hour
	v := h.Get("Retry-After")
	// RFC 9110 delay-seconds is digits only, so ParseUint, which rejects a sign. A value too
	// large for a uint64 is a very long delay: it takes the cap, not 0.
	if secs, err := strconv.ParseUint(v, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if secs >= uint64(day/time.Second) {
			return day
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(at), 0), day)
	}
	return 0
}

// messageExtensions asks for each offered extension that an outgoing message names in its extensions, whatever the card
// lists: a message that carries an extension's payload (an answer to a HITL pause) is sent as that extension's, so the
// agent reads the payload as such. It only adds to what the activator asks for; a URI both ask for is sent once, because
// a2a-go's ServiceParams.Append drops a value already present (a2aclient/middleware.go, v2.6.0).
type messageExtensions struct {
	a2aclient.PassthroughInterceptor
	offered []string
}

// Before adds the A2A-Extensions values of a send.
func (m messageExtensions) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if send, ok := req.Payload.(*a2a.SendMessageRequest); ok && send.Message != nil {
		for _, ext := range send.Message.Extensions {
			if slices.Contains(m.offered, ext) {
				req.ServiceParams.Append(a2a.SvcParamExtensions, ext)
			}
		}
	}
	return ctx, nil, nil
}
