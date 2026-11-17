package e2e

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeweb"
)

// The fast tier's web ChatConnections, on custom resources (hack/kind/fast/web/web.yaml, served when hack/kind/web-values.yaml admits their namespace):
// a kourier-v1 connection, whose replies go to the fake receiver, and a github one, which posts nothing. test/kindmanifests pins that the manifests
// and lib.sh's deploy_web agree with these.
const (
	// WebNamespace holds both connections, their Secrets, AgentBackend and Bindings.
	WebNamespace = "e2e-web"
	// ConnWebKourier and ConnWebGitHub are the connections, as the broker's series name them; each has a Binding of the same name.
	ConnWebKourier = WebNamespace + "/kourier"
	ConnWebGitHub  = WebNamespace + "/github"
	// RouteWebKourier and RouteWebGitHub are their routes on the chat listener, which the fake sender POSTs to.
	RouteWebKourier = "/chat/web/" + ConnWebKourier
	RouteWebGitHub  = "/chat/web/" + ConnWebGitHub
	// ChannelWebKourier is the kourier-v1 Binding's channel, answered by the stub's AgentWebKourier, with final output.
	ChannelWebKourier = "web-turns-1"
	AgentWebKourier   = "scenario-memory"
	// GitHubRepo is the github Binding's channel, the repository its pull requests are of, answered by the stub's AgentWebGitHub, with
	// output none.
	GitHubRepo     = "agentkourier/e2e-web"
	AgentWebGitHub = "test-agent"
	// GitHubLogin is the account the agent posts as on GitHub, which the github connection's spec names: its own events start nothing.
	GitHubLogin = "pr-reviewer"
	// WebAgentEmail is the email of the agent token both Bindings send (lib.sh's deploy_web), which the stub agent owns their tasks by.
	WebAgentEmail = "e2e-web@example.test"
)

// WebAgentToken is the agent token of the web Bindings: an unsigned JWT with WebAgentEmail, as lib.sh's unsigned_jwt makes it. A
// scenario sends it to list the Bindings' tasks at the stub agent (Stub.Tasks).
func WebAgentToken() string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(`{"email":"`+WebAgentEmail+`"}`)) + "."
}

// Web drives the fake web sender and reads the fake receiver (test/fakeweb). Its control surface has no authentication, so this client
// is only for tests inside the cluster.
type Web struct{ c *client }

// NewWeb returns a client for the fake at baseURL.
func NewWeb(baseURL string) *Web {
	c := newClient(baseURL)
	c.limit = maxLog // the receiver's record holds every reply since the fake started
	// The sender's own request to the broker may take its client's 10 s: the wait outlasts it.
	c.hc = &http.Client{Timeout: 2 * httpTimeout}
	return &Web{c}
}

// Send asks the fake to send one request to the broker's chat listener, and returns how it was answered. A request that got no answer
// is a Sent with Status 0 and the transport's error, not an error here: an error is the fake's.
func (w *Web) Send(ctx context.Context, in fakeweb.Send) (fakeweb.Sent, error) {
	var sent fakeweb.Sent
	err := w.c.do(ctx, http.MethodPost, fakeweb.ControlSendPath, in, &sent)
	return sent, err
}

// FailNext has the fake receiver fail the next n replies that keep the contract with status, a 503 with a Retry-After of 1 second
// (fakeweb.Failure), so that a scenario sees the broker try a reply again.
func (w *Web) FailNext(ctx context.Context, n, status int) error {
	return w.c.do(ctx, http.MethodPost, fakeweb.ControlFailNextPath, fakeweb.Failure{N: n, Status: status}, nil)
}

// Replies returns every reply the fake receiver has been sent, oldest first.
func (w *Web) Replies(ctx context.Context) ([]fakeweb.Reply, error) {
	var out []fakeweb.Reply
	err := w.c.do(ctx, http.MethodGet, fakeweb.ControlRepliesPath, nil, &out)
	return out, err
}

// ThreadReplies returns the replies of the thread thread of channel of the connection conn, oldest first.
func (w *Web) ThreadReplies(ctx context.Context, conn, channel, thread string) ([]fakeweb.Reply, error) {
	all, err := w.Replies(ctx)
	if err != nil {
		return nil, err
	}
	var out []fakeweb.Reply
	for _, r := range all {
		if r.Connection == conn && r.Channel == channel && r.Thread == thread {
			out = append(out, r)
		}
	}
	return out, nil
}

// WaitThread waits up to timeout for the replies of a thread (ThreadReplies) to satisfy pred, and returns them. desc says what it waits for.
func (w *Web) WaitThread(ctx context.Context, timeout time.Duration, desc, conn, channel, thread string, pred func([]fakeweb.Reply) bool) ([]fakeweb.Reply, error) {
	var got []fakeweb.Reply
	err := Wait(ctx, timeout, desc, func(ctx context.Context) (bool, error) {
		var err error
		got, err = w.ThreadReplies(ctx, conn, channel, thread)
		return err == nil && pred(got), err
	})
	return got, err
}

// Posts are the replies that are posts, keep the contract and were taken (200), the repeats of one post left out: one for each message
// the receiver shows. A try the receiver failed on purpose (FailNext) is not one.
func Posts(rs []fakeweb.Reply) []fakeweb.Reply {
	var out []fakeweb.Reply
	for _, r := range rs {
		if r.Kind == "post" && r.Problem == "" && r.Status == http.StatusOK && !r.Repeat {
			out = append(out, r)
		}
	}
	return out
}
