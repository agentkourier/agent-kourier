// Package fakeweb is the far side of Agent Kourier's web ChatConnections, for the fast E2E tier (spec 12.1). It plays two roles on
// one port:
//
//   - the sender (sender.go): on a test's word it signs a request as a kourier-v1 sender or as GitHub signs a webhook delivery, POSTs
//     it once to the broker's chat listener, and tells the test how it was answered;
//   - the receiver (receiver.go): it is the URL a kourier-v1 connection posts its replies to (spec.web.url). It checks each reply's
//     signature and Idempotency-Key, answers it, and keeps what it was sent for the tests to read.
//
// It is written from the wire contracts (the kourier-v1 profile's package comment, and GitHub's webhook documentation), not from
// Agent Kourier's code, so that a test through it checks the contract and not the code against itself: it signs and checks with
// HMAC-SHA256 of its own. Its unit tests hand what it sends to the profiles, and what the kourier-v1 profile encodes to it, so that the
// two sides are known to agree before a cluster runs them.
//
// The routes:
//
//	POST /replies              the receiver: a kourier-v1 reply, recorded; 200, 401 for one it does not verify, 400 for one that is
//	                           not the contract, or the status of a Failure
//	GET  /_control/replies     what the receiver has recorded, oldest first, as JSON
//	POST /_control/send        the sender: one request to the chat listener, as a Send says; answers a Sent
//	POST /_control/fail-next   a Failure: the receiver fails the next replies that keep the contract with its status; 204
//
// The control surface has no authentication, and the sender POSTs to the chat listener alone (Config.ChatURL, with a path under
// /chat/web/): run it only behind a ClusterIP Service. The secrets it signs with are fake constants that the tier gives it on its
// command line; it never prints them.
package fakeweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// The routes of the server.
const (
	// RepliesPath is the receiver: the path of the kourier-v1 connection's spec.web.url.
	RepliesPath = "/replies"
	// ControlRepliesPath serves the receiver's record.
	ControlRepliesPath = "/_control/replies"
	// ControlSendPath is the sender.
	ControlSendPath = "/_control/send"
	// ControlFailNextPath takes a Failure: the receiver fails the next replies on purpose.
	ControlFailNextPath = "/_control/fail-next"
)

// maxBody is the most bytes of a request's body the server reads: a reply past it is refused (413) and not recorded, and so is a Send.
// It is the chat listener's own cap on a body, which a reply, at most 1 MiB by the profile's bound, fits in.
const maxBody = 1 << 20

// Config is what a Server is built from.
type Config struct {
	// ChatURL is the broker's chat listener, scheme and host (and port), with no path: the sender POSTs to it, at a path under
	// /chat/web/, and nowhere else.
	ChatURL string
	// KourierV1Secret is the signingSecret of the kourier-v1 connection: the sender signs its messages with it, and the receiver
	// checks the replies with it.
	KourierV1Secret []byte
	// GitHubSecret is the webhookSecret of the github connection, which the sender signs its deliveries with.
	GitHubSecret []byte
	// Client sends the sender's requests; one with a 10 s timeout, GitHub's own wait for an answer (unverified), if nil.
	Client *http.Client
	// Now is the clock of the signatures; time.Now if nil.
	Now func() time.Time
}

// Server is the fake. It is an http.Handler and is safe for concurrent use.
type Server struct {
	chat    *url.URL
	kourier []byte
	github  []byte
	client  *http.Client
	now     func() time.Time

	mu         sync.Mutex
	replies    []Reply         // guarded by mu
	keys       map[string]bool // the Idempotency-Keys of the posts taken, guarded by mu
	failures   int             // how many more replies to fail on purpose (Failure), guarded by mu
	failStatus int             // the status they are failed with, guarded by mu
}

// New returns the fake of cfg, or an error for a config it cannot run with. The error names no secret.
func New(cfg Config) (*Server, error) {
	u, err := url.Parse(cfg.ChatURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, errors.New("fakeweb: the chat URL is not an absolute http or https URL")
	case u.Path != "" && u.Path != "/", u.RawQuery != "", u.Fragment != "":
		return nil, errors.New("fakeweb: the chat URL has a path, a query or a fragment; the sender adds the path")
	case len(cfg.KourierV1Secret) == 0 || len(cfg.GitHubSecret) == 0:
		return nil, errors.New("fakeweb: a secret is empty")
	}
	u.Path = ""
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Server{chat: u, kourier: cfg.KourierV1Secret, github: cfg.GitHubSecret, client: client, now: now, keys: map[string]bool{}}, nil
}

// ServeHTTP routes a request (see the package comment).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := func(method string, h http.HandlerFunc) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, "use "+method, http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
	switch r.URL.Path {
	case RepliesPath:
		route(http.MethodPost, s.receive)
	case ControlRepliesPath:
		route(http.MethodGet, s.serveReplies)
	case ControlSendPath:
		route(http.MethodPost, s.serveSend)
	case ControlFailNextPath:
		route(http.MethodPost, s.serveFailNext)
	default:
		http.NotFound(w, r)
	}
}

// writeJSON answers v as JSON with status 200.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, fmt.Sprintf("encode: %v", err), http.StatusInternalServerError)
	}
}
