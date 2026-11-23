package cluster

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// HopHeader marks a request that a standby forwarded. A replica that does not serve answers such a request 503
// instead of forwarding it again, so a request crosses at most one hop, even while two replicas disagree on who leads.
const HopHeader = "X-Agent-Kourier-Ax-Forwarded"

// forwardDialTimeout bounds the connection to the leader's address: a leader whose pod is gone is answered 503 after
// it, not after the operating system's much longer connect timeout.
const forwardDialTimeout = 5 * time.Second

// Leaders finds the replica to forward to. *Elector implements it.
type Leaders interface {
	// Leader returns the base URL of the replica that leads, when it is another one and its lease is live.
	Leader(ctx context.Context) (*url.URL, bool)
	// Forget drops a leader that could not be reached, so the next request looks again.
	Forget()
}

// Router sends each A2A request (and each request for the agent card) to Local when this replica serves, and forwards
// it to the leader when this replica is a standby. A leader that has not finished taking over, a standby that knows of
// no leader, and a standby that receives a request another standby forwarded answer 503 with Retry-After, and never
// forward to themselves.
//
// The caller check is inside Local, so it runs on the leader alone: a request is decided once, against the token file
// of the replica that runs it, and a token rotation that reaches the replicas at different moments can refuse a request
// only where it would run. A standby passes the caller's headers on as they came (Authorization or AUTH_HEADER, and
// traceparent).
type Router struct {
	// Local serves the A2A endpoint and the card on this replica.
	Local http.Handler
	// Status says whether this replica serves.
	Status *Status
	// Leaders finds the leader; nil when there is no election (SQLite), and the replica never forwards.
	Leaders Leaders
	// RetryAfter is what a 503 asks the caller to wait: the election's retry period, in which a standby takes over.
	RetryAfter time.Duration
	Telemetry  *telemetry.Telemetry
	Log        *slog.Logger

	once  sync.Once
	proxy *httputil.ReverseProxy
}

// targetKey carries a request's leader from ServeHTTP to the proxy's Rewrite.
type targetKey struct{}

// ServeHTTP implements http.Handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case rt.Status.Serving():
		rt.Local.ServeHTTP(w, r)
	case rt.Status.Leading() || rt.Leaders == nil:
		rt.unavailable(w, "the agent is taking over as the leader")
	case r.Header.Get(HopHeader) != "":
		rt.Telemetry.Forward(telemetry.ForwardLoop)
		rt.unavailable(w, "this replica is not the leader")
	default:
		target, ok := rt.Leaders.Leader(r.Context())
		if !ok {
			rt.Telemetry.Forward(telemetry.ForwardNoLeader)
			rt.unavailable(w, "no replica leads")
			return
		}
		rt.once.Do(rt.build)
		rt.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, target)))
	}
}

// build makes the proxy. It flushes each write of the leader's answer at once (FlushInterval -1), so a stream of
// server-sent events reaches the caller event by event.
func (rt *Router) build() {
	dialer := &net.Dialer{Timeout: forwardDialTimeout, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	rt.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(pr.In.Context().Value(targetKey{}).(*url.URL))
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"] // SetXForwarded appends the caller to it
			pr.SetXForwarded()
			pr.Out.Header.Set(HopHeader, "1")
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(*http.Response) error {
			rt.Telemetry.Forward(telemetry.Forwarded)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				rt.Telemetry.Forward(telemetry.ForwardCancelled)
				return
			}
			rt.Leaders.Forget()
			rt.Telemetry.Forward(telemetry.ForwardFailed)
			rt.Log.Warn("forward a request to the leader", "leader", r.URL.Host, "error", err)
			rt.unavailable(w, "the leader could not be reached")
		},
		ErrorLog: slog.NewLogLogger(rt.Log.Handler(), slog.LevelWarn),
	}
}

// unavailable answers 503, asking the caller to try again after RetryAfter.
func (rt *Router) unavailable(w http.ResponseWriter, why string) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(max(rt.RetryAfter, time.Second).Seconds()))))
	http.Error(w, why, http.StatusServiceUnavailable)
}
