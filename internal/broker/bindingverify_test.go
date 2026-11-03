package broker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// want is the status and the reason a checker is expected to report.
type want struct {
	status bindingcheck.Status
	reason bindingcheck.Reason
}

// Each answer of the card fetch that the table of conditions names has its class, and the statuses that say nothing of the agent are
// Unknown. The errors are shaped as the pool gives them: the HTTP client wraps the dialect's status error in a *url.Error, and the
// pool wraps that with the Binding's name.
func TestClassifyAgentSaysWhatEachAnswerMeans(t *testing.T) {
	status := func(code int) error {
		return fmt.Errorf("binding sre/oncall: %w", &url.Error{Op: "Get", URL: "http://backend", Err: &dialect.HTTPStatusError{Code: code}})
	}
	wrap := func(err error) error { return fmt.Errorf("binding sre/oncall: %w", err) }
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	for _, tc := range []struct {
		name string
		err  error
		want want
	}{
		{"token unreadable", wrap(fmt.Errorf("%w: %w", agent.ErrTokenUnreadable, errors.New("no key"))), want{bindingcheck.False, bindingcheck.ReasonCredentialsUnreadable}},
		{"header secret unreadable", wrap(agent.ErrHeaderUnreadable), want{bindingcheck.False, bindingcheck.ReasonCredentialsUnreadable}},
		{"token required, none given", wrap(agent.ErrTokenRequired), want{bindingcheck.False, bindingcheck.ReasonCredentialsUnreadable}},
		{"401", status(401), want{bindingcheck.False, bindingcheck.ReasonCredentialsRejected}},
		{"403", status(403), want{bindingcheck.False, bindingcheck.ReasonAgentForbidden}},
		{"404", status(404), want{bindingcheck.False, bindingcheck.ReasonAgentNotFound}},
		{"409", status(409), want{bindingcheck.False, bindingcheck.ReasonAgentNotReady}},
		{"408", status(408), want{bindingcheck.Unknown, bindingcheck.ReasonUnexpectedStatus}},
		{"425", status(425), want{bindingcheck.Unknown, bindingcheck.ReasonUnexpectedStatus}},
		{"429", status(429), want{bindingcheck.Unknown, bindingcheck.ReasonUnexpectedStatus}},
		{"500", status(500), want{bindingcheck.False, bindingcheck.ReasonBackendError}},
		{"502", status(502), want{bindingcheck.False, bindingcheck.ReasonBackendError}},
		{"503", status(503), want{bindingcheck.False, bindingcheck.ReasonBackendError}},
		{"307, a redirect to a login page", status(307), want{bindingcheck.False, bindingcheck.ReasonUnexpectedStatus}},
		{"400", status(400), want{bindingcheck.False, bindingcheck.ReasonUnexpectedStatus}},
		{"410", status(410), want{bindingcheck.False, bindingcheck.ReasonUnexpectedStatus}},
		{"418", status(418), want{bindingcheck.False, bindingcheck.ReasonUnexpectedStatus}},
		{"a bare status error, not wrapped by the client", wrap(&dialect.HTTPStatusError{Code: 404}), want{bindingcheck.False, bindingcheck.ReasonAgentNotFound}},
		{"2xx that is not 200: 204", wrap(&agentcard.ErrStatusNotOK{StatusCode: 204, Status: "204 No Content"}), want{bindingcheck.False, bindingcheck.ReasonBackendError}},
		{"2xx that is not 200: 206", wrap(&agentcard.ErrStatusNotOK{StatusCode: 206, Status: "206 Partial Content"}), want{bindingcheck.False, bindingcheck.ReasonBackendError}},
		{"no streaming", wrap(fmt.Errorf("agent kagent/x: %w", agent.ErrStreamingUnsupported)), want{bindingcheck.False, bindingcheck.ReasonStreamingUnsupported}},
		{"unsupported extension", wrap(dialect.ErrUnsupportedExtension), want{bindingcheck.False, bindingcheck.ReasonUnsupportedExtension}},
		{"no JSON-RPC interface", wrap(dialect.ErrNoJSONRPCInterface), want{bindingcheck.False, bindingcheck.ReasonNoJSONRPCInterface}},
		{"invalid card", wrap(dialect.ErrInvalidAgentCard), want{bindingcheck.False, bindingcheck.ReasonInvalidAgentCard}},
		{"dial failure", wrap(&url.Error{Op: "Get", URL: "http://backend", Err: dial}), want{bindingcheck.False, bindingcheck.ReasonUnreachable}},
		{"dial failure, no client around it", wrap(dial), want{bindingcheck.False, bindingcheck.ReasonUnreachable}},
		{"DNS failure", wrap(&url.Error{Op: "Get", URL: "http://backend", Err: &net.DNSError{Err: "no such host", Name: "backend", IsNotFound: true}}), want{bindingcheck.False, bindingcheck.ReasonUnreachable}},
		{"TLS certificate not trusted", wrap(&url.Error{Op: "Get", URL: "https://backend", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}), want{bindingcheck.False, bindingcheck.ReasonUnreachable}},
		{"TLS spoken to a plain server", wrap(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), want{bindingcheck.False, bindingcheck.ReasonUnreachable}},
		{"connection cut mid-answer", wrap(io.ErrUnexpectedEOF), want{bindingcheck.False, bindingcheck.ReasonUnreachable}},
		{"connection closed before an answer", wrap(io.EOF), want{bindingcheck.False, bindingcheck.ReasonUnreachable}},
		{"deadline passed", wrap(context.DeadlineExceeded), want{bindingcheck.False, bindingcheck.ReasonUnreachable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := classifyAgent(tc.err)
			if !ok || out.Status != tc.want.status || out.Reason != tc.want.reason {
				t.Errorf("classifyAgent(%v) = %s/%s (placed %t); want %s/%s", tc.err, out.Status, out.Reason, ok, tc.want.status, tc.want.reason)
			}
			if out.Message == "" {
				t.Error("the outcome has no message")
			}
		})
	}
}

// An error the classifier cannot place is not guessed at, and neither is a pool that closed or a Binding the backend cannot be built for.
func TestClassifyAgentDoesNotPlaceWhatItDoesNotKnow(t *testing.T) {
	for _, err := range []error{
		errors.New("something else"), agent.ErrPoolClosed, context.Canceled, session.ErrUnknownBinding,
		errors.New("broker: Binding sre/oncall: backend platform/kagent uses the \"x\" dialect"),
	} {
		if out, ok := classifyAgent(fmt.Errorf("wrapped: %w", err)); ok {
			t.Errorf("classifyAgent(%v) = %s/%s; want it unplaced", err, out.Status, out.Reason)
		}
	}
}

// A message names the class and the status and nothing the error said: a response can quote a credential. The marker is in every error's
// text here, and in no message.
func TestAgentOutcomeMessagesCarryNothingOfTheErrorText(t *testing.T) {
	const marker = "Bearer tok-SECRET-123"
	for _, err := range []error{
		fmt.Errorf("%s: %w", marker, &dialect.HTTPStatusError{Code: 401}),
		fmt.Errorf("%s: %w", marker, &agentcard.ErrStatusNotOK{StatusCode: 204}),
		fmt.Errorf("%s: %w", marker, agent.ErrTokenUnreadable),
		fmt.Errorf("%s: %w", marker, dialect.ErrInvalidAgentCard),
		&url.Error{Op: "Get", URL: "http://backend/?token=" + marker, Err: io.ErrUnexpectedEOF},
	} {
		out, ok := classifyAgent(err)
		if !ok {
			t.Fatalf("classifyAgent(%v) is unplaced", err)
		}
		if strings.Contains(out.Message, marker) || strings.Contains(out.Message, "tok-SECRET") {
			t.Errorf("the message %q holds the error's text", out.Message)
		}
	}
}

// probeFunc is an agentProbe made of a function.
type probeFunc func(ctx context.Context, binding string) error

func (f probeFunc) CheckAgent(ctx context.Context, binding string) error { return f(ctx, binding) }

// A check that finds nothing wrong is True.
func TestAgentCheckerReportsTrue(t *testing.T) {
	c := agentChecker{bindings: probeFunc(func(context.Context, string) error { return nil }), timeout: time.Second}
	out, err := c.CheckAgent(t.Context(), "sre/oncall")
	if err != nil || out.Status != bindingcheck.True || out.Reason != bindingcheck.ReasonOK {
		t.Errorf("CheckAgent = %+v, %v; want True/OK", out, err)
	}
}

// A backend that never answers is cut by the checker's own bound, and that is Unreachable: a backend that does not answer is one a turn
// would hang on. The bound is the checker's, shorter than the Prober's, so the Prober's ProbeTimeout (which says nothing of the backend,
// and which a checker may not report) is not what the operator reads.
func TestAgentCheckerOwnDeadlineIsUnreachable(t *testing.T) {
	blackHole := probeFunc(func(ctx context.Context, _ string) error { <-ctx.Done(); return fmt.Errorf("card: %w", ctx.Err()) })
	c := agentChecker{bindings: blackHole, timeout: 20 * time.Millisecond}
	out, err := c.CheckAgent(t.Context(), "sre/oncall")
	if err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonUnreachable {
		t.Errorf("CheckAgent = %+v, %v; want False/Unreachable", out, err)
	}
}

// The bound is the checker's whatever shape the error took: one that does not wrap the deadline is still a backend that did not answer.
func TestAgentCheckerOwnDeadlineIsUnreachableEvenForAnErrorThatDoesNotWrapIt(t *testing.T) {
	c := agentChecker{timeout: 20 * time.Millisecond, bindings: probeFunc(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return errors.New("gave up waiting")
	})}
	out, err := c.CheckAgent(t.Context(), "sre/oncall")
	if err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonUnreachable {
		t.Errorf("CheckAgent = %+v, %v; want False/Unreachable", out, err)
	}
}

// When the context the Prober gave ends, whether by its timeout or a shutdown, the check says nothing of the backend: it is not
// Unreachable, and it is not any verdict.
func TestAgentCheckerEndedByTheProbersContextIsNoVerdict(t *testing.T) {
	blackHole := probeFunc(func(ctx context.Context, _ string) error { <-ctx.Done(); return fmt.Errorf("card: %w", ctx.Err()) })
	c := agentChecker{bindings: blackHole, timeout: time.Minute}
	for name, mk := range map[string]func() (context.Context, context.CancelFunc){
		"deadline": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(t.Context(), 20*time.Millisecond)
		},
		"cancel": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(20*time.Millisecond, cancel)
			return ctx, cancel
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := mk()
			defer cancel()
			out, err := c.CheckAgent(ctx, "sre/oncall")
			if err == nil || out != (bindingcheck.Outcome{}) {
				t.Errorf("CheckAgent = %+v, %v; want an error and no outcome", out, err)
			}
		})
	}
}

// A Binding that a reload removed during a pass comes back as the error that says so, wrapped but findable: the log hook drops it,
// and the Prober prunes the Binding at its next Tick.
func TestAgentCheckerKeepsAnUnknownBindingFindable(t *testing.T) {
	c := agentChecker{bindings: probeFunc(func(context.Context, string) error {
		return fmt.Errorf("%w: sre/oncall", session.ErrUnknownBinding)
	}), timeout: time.Second}
	out, err := c.CheckAgent(t.Context(), "sre/oncall")
	if !errors.Is(err, session.ErrUnknownBinding) || out != (bindingcheck.Outcome{}) {
		t.Errorf("CheckAgent = %+v, %v; want no outcome and an error that wraps session.ErrUnknownBinding", out, err)
	}
}

// The adapter against a real Bindings and a real backend, so that the errors are the pool's own and not shaped by the test: each status
// of the table, a card that is not a card, a backend that is not there, a token that cannot be read, and the Binding that is fine.
func TestAgentCheckerAgainstARealBackend(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want want
	}{
		{"401", 401, want{bindingcheck.False, bindingcheck.ReasonCredentialsRejected}},
		{"403", 403, want{bindingcheck.False, bindingcheck.ReasonAgentForbidden}},
		{"404", 404, want{bindingcheck.False, bindingcheck.ReasonAgentNotFound}},
		{"409", 409, want{bindingcheck.False, bindingcheck.ReasonAgentNotReady}},
		{"408", 408, want{bindingcheck.Unknown, bindingcheck.ReasonUnexpectedStatus}},
		{"429", 429, want{bindingcheck.Unknown, bindingcheck.ReasonUnexpectedStatus}},
		{"500", 500, want{bindingcheck.False, bindingcheck.ReasonBackendError}},
		{"503", 503, want{bindingcheck.False, bindingcheck.ReasonBackendError}},
		{"307", 307, want{bindingcheck.False, bindingcheck.ReasonUnexpectedStatus}},
		{"418", 418, want{bindingcheck.False, bindingcheck.ReasonUnexpectedStatus}},
		{"204", 204, want{bindingcheck.False, bindingcheck.ReasonBackendError}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			backend, _ := refusingBackend(t, tc.code)
			f.snaps.p.Store(snapshot(t, backend, v1alpha1.DialectKagentV1, time.Hour))
			out, err := agentChecker{bindings: f.b, timeout: 5 * time.Second}.CheckAgent(t.Context(), "sre/oncall")
			if err != nil || out.Status != tc.want.status || out.Reason != tc.want.reason {
				t.Errorf("CheckAgent = %+v, %v; want %s/%s", out, err, tc.want.status, tc.want.reason)
			}
		})
	}

	t.Run("a card that is no card", func(t *testing.T) {
		f := newFixture(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>log in</html>") }))
		t.Cleanup(srv.Close)
		f.snaps.p.Store(snapshot(t, srv.URL, v1alpha1.DialectKagentV1, time.Hour))
		out, err := agentChecker{bindings: f.b, timeout: 5 * time.Second}.CheckAgent(t.Context(), "sre/oncall")
		if err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonInvalidAgentCard {
			t.Errorf("CheckAgent = %+v, %v; want False/InvalidAgentCard", out, err)
		}
	})
	t.Run("nothing listens", func(t *testing.T) {
		f := newFixture(t)
		srv := httptest.NewServer(http.NotFoundHandler())
		closed := srv.URL
		srv.Close()
		f.snaps.p.Store(snapshot(t, closed, v1alpha1.DialectKagentV1, time.Hour))
		out, err := agentChecker{bindings: f.b, timeout: 5 * time.Second}.CheckAgent(t.Context(), "sre/oncall")
		if err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonUnreachable {
			t.Errorf("CheckAgent = %+v, %v; want False/Unreachable", out, err)
		}
	})
	t.Run("a backend that never answers", func(t *testing.T) {
		f := newFixture(t)
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		t.Cleanup(func() { close(release); srv.Close() })
		f.snaps.p.Store(snapshot(t, srv.URL, v1alpha1.DialectKagentV1, time.Hour))
		out, err := agentChecker{bindings: f.b, timeout: 50 * time.Millisecond}.CheckAgent(t.Context(), "sre/oncall")
		if err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonUnreachable {
			t.Errorf("CheckAgent = %+v, %v; want False/Unreachable", out, err)
		}
	})
	t.Run("a token that cannot be read", func(t *testing.T) {
		f := newFixture(t)
		f.reader.set("sre/creds/token", "") // an empty value is unreadable
		out, err := agentChecker{bindings: f.b, timeout: 5 * time.Second}.CheckAgent(t.Context(), "sre/oncall")
		if err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonCredentialsUnreadable {
			t.Errorf("CheckAgent = %+v, %v; want False/CredentialsUnreadable", out, err)
		}
	})
	t.Run("an agent the stub agent does not have", func(t *testing.T) {
		f := newFixture(t)
		stub := stubagent.NewHandler() // serves no agent: the card of the Binding's is a 404
		srv := httptest.NewServer(stub)
		t.Cleanup(func() { srv.Close(); stub.Close() })
		f.snaps.p.Store(snapshot(t, srv.URL, v1alpha1.DialectKagentV1, time.Hour))
		out, err := agentChecker{bindings: f.b, timeout: 5 * time.Second}.CheckAgent(t.Context(), "sre/oncall")
		if err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonAgentNotFound {
			t.Errorf("CheckAgent = %+v, %v; want False/AgentNotFound", out, err)
		}
	})
	t.Run("a Binding that serves", func(t *testing.T) {
		f := newFixture(t)
		out, err := agentChecker{bindings: f.b, timeout: 5 * time.Second}.CheckAgent(t.Context(), "sre/oncall")
		if err != nil || out.Status != bindingcheck.True {
			t.Errorf("CheckAgent = %+v, %v; want True", out, err)
		}
	})
	t.Run("a Binding the config does not have", func(t *testing.T) {
		f := newFixture(t)
		_, err := agentChecker{bindings: f.b, timeout: 5 * time.Second}.CheckAgent(t.Context(), "sre/gone")
		if !errors.Is(err, session.ErrUnknownBinding) {
			t.Errorf("CheckAgent = %v; want an error that wraps session.ErrUnknownBinding", err)
		}
	})
}

// accessFake is a channelReader with an answer, a classification and a count of its calls.
type accessFake struct {
	mu       sync.Mutex
	access   chat.Access
	err      error
	verdict  chat.Verdict
	wait     bool // block until the context ends
	calls    int
	channels []string
}

func (a *accessFake) ChannelAccess(ctx context.Context, channel string) (chat.Access, error) {
	a.mu.Lock()
	a.calls++
	a.channels = append(a.channels, channel)
	access, err, wait := a.access, a.err, a.wait
	a.mu.Unlock()
	if wait {
		<-ctx.Done()
		return chat.Access{}, fmt.Errorf("conversations.info: %w", ctx.Err())
	}
	return access, err
}

func (a *accessFake) Classify(chat.Op, error) chat.Verdict {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.verdict
}

func (a *accessFake) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

// What the bot's access to the channel means for ChannelJoined, every row of the contract: Archived is tested before Member, the two
// sentinels are False, a rejected credential is False/ChatCredentialsRejected, and every other failure is Unknown/ChatUnavailable.
func TestChannelCheckerSaysWhatTheAccessAndTheErrorMean(t *testing.T) {
	for _, tc := range []struct {
		name    string
		access  chat.Access
		err     error
		verdict chat.Verdict
		want    want
	}{
		{"member", chat.Access{Member: true}, nil, chat.Verdict{}, want{bindingcheck.True, bindingcheck.ReasonOK}},
		{"not a member", chat.Access{}, nil, chat.Verdict{}, want{bindingcheck.False, bindingcheck.ReasonNotInChannel}},
		{"archived, and a member", chat.Access{Member: true, Archived: true}, nil, chat.Verdict{}, want{bindingcheck.False, bindingcheck.ReasonChannelArchived}},
		{"archived, and not a member", chat.Access{Archived: true}, nil, chat.Verdict{}, want{bindingcheck.False, bindingcheck.ReasonChannelArchived}},
		{"channel not found", chat.Access{}, fmt.Errorf("slack: %w", chat.ErrChannelNotFound), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone}, want{bindingcheck.False, bindingcheck.ReasonChannelNotFound}},
		{"missing scope", chat.Access{}, fmt.Errorf("slack: %w", chat.ErrMissingScope), chat.Verdict{Class: chat.Permanent}, want{bindingcheck.False, bindingcheck.ReasonMissingScope}},
		{"token rejected", chat.Access{}, errors.New("slack: invalid_auth"), chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}, want{bindingcheck.False, bindingcheck.ReasonChatCredentialsRejected}},
		{"rate limited", chat.Access{}, errors.New("slack: ratelimited"), chat.Verdict{Class: chat.Transient, RetryAfter: 30 * time.Second}, want{bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable}},
		{"transient failure", chat.Access{}, errors.New("slack: 502"), chat.Verdict{Class: chat.Transient}, want{bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable}},
		{"permanent for a reason of its own", chat.Access{}, errors.New("slack: access_denied"), chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther}, want{bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &accessFake{access: tc.access, err: tc.err, verdict: tc.verdict}
			c := channelChecker{platform: f, timeout: time.Second}
			out, err := c.CheckChannel(t.Context(), "C1")
			if err != nil || out.Status != tc.want.status || out.Reason != tc.want.reason {
				t.Errorf("CheckChannel = %+v, %v; want %s/%s", out, err, tc.want.status, tc.want.reason)
			}
			if out.Message == "" && out.Status != bindingcheck.True {
				t.Error("the outcome has no message")
			}
			if strings.Contains(out.Message, "xoxb-") {
				t.Errorf("the message %q holds a token", out.Message)
			}
		})
	}
}

// The checker's own bound on a chat platform that does not answer is Unknown/ChatUnavailable; the context of the Prober ending is no
// verdict at all.
func TestChannelCheckerDeadlines(t *testing.T) {
	f := &accessFake{wait: true}
	out, err := channelChecker{platform: f, timeout: 20 * time.Millisecond}.CheckChannel(t.Context(), "C1")
	if err != nil || out.Status != bindingcheck.Unknown || out.Reason != bindingcheck.ReasonChatUnavailable {
		t.Errorf("CheckChannel past its own bound = %+v, %v; want Unknown/ChatUnavailable", out, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	out, err = channelChecker{platform: f, timeout: time.Minute}.CheckChannel(ctx, "C1")
	if err == nil || out != (bindingcheck.Outcome{}) {
		t.Errorf("CheckChannel past the Prober's = %+v, %v; want an error and no outcome", out, err)
	}
}

// the checker against the fake platform and its own Classify, so that the FatalAuth of a real classification, and not one the test made up,
// is what gives ChatCredentialsRejected.
func TestChannelCheckerAgainstTheFakePlatform(t *testing.T) {
	p := chattest.NewFake()
	p.SetAccess("CIN", chat.Access{Member: true})
	p.SetAccess("COUT", chat.Access{})
	c := channelChecker{platform: p, timeout: time.Second}
	for channel, w := range map[string]want{
		"CIN":   {bindingcheck.True, bindingcheck.ReasonOK},
		"COUT":  {bindingcheck.False, bindingcheck.ReasonNotInChannel},
		"CNONE": {bindingcheck.False, bindingcheck.ReasonChannelNotFound},
	} {
		if out, err := c.CheckChannel(t.Context(), channel); err != nil || out.Status != w.status || out.Reason != w.reason {
			t.Errorf("CheckChannel(%s) = %+v, %v; want %s/%s", channel, out, err, w.status, w.reason)
		}
	}
	p.FailNext("ChannelAccess", &chattest.FakeError{Verdict: chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}, Msg: "invalid_auth"})
	if out, err := c.CheckChannel(t.Context(), "CIN"); err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonChatCredentialsRejected {
		t.Errorf("CheckChannel with a rejected token = %+v, %v; want False/ChatCredentialsRejected", out, err)
	}
	p.FailNext("ChannelAccess", fmt.Errorf("fake: %w", chat.ErrMissingScope))
	if out, err := c.CheckChannel(t.Context(), "CIN"); err != nil || out.Status != bindingcheck.False || out.Reason != bindingcheck.ReasonMissingScope {
		t.Errorf("CheckChannel without the scope = %+v, %v; want False/MissingScope", out, err)
	}
}

// The Message of Unknown/ChatUnavailable, through the real Slack client and its Classify: a rate limit and a deadline are said so, and
// every other failure is "the chat platform read failed" with its cause, which is the platform's code when it answered (no_permission) and the
// transport's error when it did not (connection refused), since the client drops the difference. The cause is in the Message and in the
// log line and no token is in either. A Message that changes between passes is not a transition, so a cause that changes adds no line.
func TestChannelCheckerReportsTheCauseOfAFailedReadThroughTheRealSlackClient(t *testing.T) {
	const bot, app = "xoxb-TESTTOKEN-123456", "xapp-TESTTOKEN-654321"
	var answer atomic.Value
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch a, _ := answer.Load().(string); a {
		case "hang":
			<-release
		case "ratelimit":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		case "http500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":false,"error":"`+a+`"}`)
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	client := slack.New(slack.Config{BotToken: secret.New(bot), AppToken: secret.New(app), APIURL: srv.URL + "/", Logger: slog.New(slog.DiscardHandler)})
	c := channelChecker{platform: slack.NewPlatform(client, "ns/conn"), timeout: 300 * time.Millisecond}

	var outs []bindingcheck.Outcome
	for _, tc := range []struct {
		answer  string
		reason  bindingcheck.Reason
		status  bindingcheck.Status
		message string // a substring
	}{
		{"no_permission", bindingcheck.ReasonChatUnavailable, bindingcheck.Unknown, "read failed: slack conversations.info: no_permission"},
		{"access_denied", bindingcheck.ReasonChatUnavailable, bindingcheck.Unknown, "read failed: slack conversations.info: access_denied"},
		{"http500", bindingcheck.ReasonChatUnavailable, bindingcheck.Unknown, "read failed"},
		{"ratelimit", bindingcheck.ReasonChatUnavailable, bindingcheck.Unknown, "rate-limited"},
		{"hang", bindingcheck.ReasonChatUnavailable, bindingcheck.Unknown, "did not answer the read in time"},
		{"invalid_auth", bindingcheck.ReasonChatCredentialsRejected, bindingcheck.False, "rejected the bot's token"},
		{"channel_not_found", bindingcheck.ReasonChannelNotFound, bindingcheck.False, "does not exist"},
	} {
		answer.Store(tc.answer)
		out, err := c.CheckChannel(t.Context(), "C1")
		if err != nil || out.Status != tc.status || out.Reason != tc.reason || !strings.Contains(out.Message, tc.message) {
			t.Errorf("%s: CheckChannel = %+v, %v; want %s/%s with %q in the message", tc.answer, out, err, tc.status, tc.reason, tc.message)
		}
		for _, token := range []string{bot, app, "TESTTOKEN"} {
			if strings.Contains(out.Message, token) {
				t.Errorf("%s: the message %q holds a token", tc.answer, out.Message)
			}
		}
		outs = append(outs, out)
	}

	// A port nobody listens on: the cause is the refusal, in a Message that claims neither that Slack answered nor that it did not.
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedURL := refused.URL
	refused.Close()
	closed := channelChecker{platform: slack.NewPlatform(slack.New(slack.Config{BotToken: secret.New(bot), AppToken: secret.New(app), APIURL: refusedURL + "/", Logger: slog.New(slog.DiscardHandler)}), "ns/conn"), timeout: 2 * time.Second}
	if out, err := closed.CheckChannel(t.Context(), "C1"); err != nil || out.Status != bindingcheck.Unknown || out.Reason != bindingcheck.ReasonChatUnavailable ||
		!strings.Contains(out.Message, "read failed") || !strings.Contains(out.Message, "connection refused") || strings.Contains(out.Message, "answered") || strings.Contains(out.Message, bot) {
		t.Errorf("a closed port: CheckChannel = %+v, %v; want Unknown/ChatUnavailable, \"read failed\" and \"connection refused\", and no claim that Slack answered", out, err)
	}

	// The code is in the log line of the transition, and no token is.
	line := logLines(func(l bindingLog) {
		l.transition(bindingcheck.Transition{Binding: "sre/oncall", Old: bindingcheck.Condition{Type: bindingcheck.ChannelJoined, Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonPending},
			New: bindingcheck.Condition{Type: bindingcheck.ChannelJoined, Status: outs[0].Status, Reason: outs[0].Reason, Message: outs[0].Message}})
	})
	if !strings.Contains(line, "no_permission") || !strings.Contains(line, "level=INFO") || strings.Contains(line, "TESTTOKEN") {
		t.Errorf("log line %q; want an INFO with the code no_permission and no token", line)
	}

	// no_permission, then access_denied: the Message changes, the status and reason do not, so State reports no transition and nothing is logged.
	st := bindingcheck.NewState()
	now := time.Now()
	st.Set("sre/oncall", now, bindingcheck.Condition{Type: bindingcheck.ChannelJoined, Status: outs[0].Status, Reason: outs[0].Reason, Message: outs[0].Message})
	if trs := st.Set("sre/oncall", now.Add(time.Minute), bindingcheck.Condition{Type: bindingcheck.ChannelJoined, Status: outs[1].Status, Reason: outs[1].Reason, Message: outs[1].Message}); len(trs) != 0 {
		t.Errorf("a changed code was a transition: %+v", trs)
	}
}

// A request that got no answer says why in the Message, without the request's URL: a name that does not resolve, a refused connection,
// a certificate that is not trusted and a scheme that is not http read differently. The wrong scheme is tried against the real client.
func TestAgentCheckerSaysWhyTheBackendDidNotAnswerWithoutItsURL(t *testing.T) {
	const secretURL = "http://backend.internal/agents/ns/name?token=SECRET-IN-URL"
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"DNS", &net.DNSError{Err: "no such host", Name: "backend.internal", IsNotFound: true}, "no such host"},
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, "connection refused"},
		{"TLS", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "unknown authority"},
		{"scheme", errors.New(`unsupported protocol scheme "ftp"`), "unsupported protocol scheme"},
		{"https at a plain server", errors.New("http: server gave HTTP response to HTTPS client"), "server gave HTTP response to HTTPS client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := classifyAgent(fmt.Errorf("binding sre/oncall: %w", &url.Error{Op: "Get", URL: secretURL, Err: tc.cause}))
			if !ok || out.Reason != bindingcheck.ReasonUnreachable || out.Status != bindingcheck.False || !strings.Contains(out.Message, tc.want) {
				t.Fatalf("classifyAgent = %+v, %t; want False/Unreachable with %q", out, ok, tc.want)
			}
			if strings.Contains(out.Message, "SECRET-IN-URL") || strings.Contains(out.Message, "/agents/") {
				t.Errorf("the message %q holds the request's URL", out.Message)
			}
			if !strings.Contains(out.Message, "the request for the agent card failed") || strings.Contains(out.Message, "did not answer") {
				t.Errorf("the message %q; want it to say the request failed and claim nothing of an answer", out.Message)
			}
		})
	}
	// A deadline is said so.
	t.Run("a deadline", func(t *testing.T) {
		out, ok := classifyAgent(fmt.Errorf("binding sre/oncall: %w", &url.Error{Op: "Get", URL: secretURL, Err: context.DeadlineExceeded}))
		if !ok || out.Reason != bindingcheck.ReasonUnreachable || !strings.Contains(out.Message, "did not answer in time") || strings.Contains(out.Message, "SECRET-IN-URL") {
			t.Errorf("classifyAgent = %+v, %t; want Unreachable, \"did not answer in time\" and no URL", out, ok)
		}
	})
	// A scheme that is not http never reaches the HTTP client: the API's validation refuses the URL at load, and the dialect refuses it
	// again before it dials. The probe then has an error it cannot place, which is a probe that could not tell, not Unreachable.
	t.Run("a scheme the dialect refuses before it dials", func(t *testing.T) {
		f := newFixture(t)
		f.snaps.p.Store(snapshot(t, "ftp://127.0.0.1:1/", v1alpha1.DialectKagentV1, time.Hour))
		out, err := agentChecker{bindings: f.b, timeout: 5 * time.Second}.CheckAgent(t.Context(), "sre/oncall")
		if err == nil || out != (bindingcheck.Outcome{}) {
			t.Errorf("CheckAgent = %+v, %v; want an error and no outcome", out, err)
		}
	})
}

// tokens is the baseline of the credentials tests: what the connection is using.
func tokens(bot, app string) map[string]secret.Value {
	return map[string]secret.Value{"botToken": secret.New(bot), "appToken": secret.New(app)}
}

// secretStore is an identity.SecretReader of one Secret whose values the test changes, and which can fail.
type secretStore struct {
	mu      sync.Mutex
	vals    map[string]string // by key; a key that is not here is missing from the Secret
	missing bool              // the Secret itself is gone
	err     error             // every read fails with it
	reads   []string          // namespace/name of each read
}

func (s *secretStore) Value(_ context.Context, ns, name, key string) (secret.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, ns+"/"+name)
	switch v, ok := s.vals[key]; {
	case s.err != nil:
		return secret.Value{}, s.err
	case s.missing:
		return secret.Value{}, fmt.Errorf("%s/%s: %w", ns, name, config.ErrSecretNotFound)
	case !ok:
		return secret.Value{}, fmt.Errorf("key %s: %w", key, secret.ErrKeyNotFound)
	default:
		return secret.New(v), nil
	}
}

// The chat credentials are compared by value, at the probe: a token other than the one in use is Unknown/ChatCredentialsChanged, a
// Secret or key that is gone, empty or of a type that is never read is Unknown/ChatCredentialsUnusable, and in either the chat platform is not asked: what
// a bot the Secret no longer names sees is not about the channel. A write that leaves the tokens alone is no change, and a read that
// fails for another reason cannot tell, so the probe goes on. No value reaches a message.
func TestChannelCheckerComparesTheChatCredentialsByValueWithoutCallingThePlatform(t *testing.T) {
	const bot, app, canary = "xoxb-in-use", "xapp-in-use", "xoxb-NEW-CANARY"
	same := func() map[string]string { return map[string]string{"botToken": bot, "appToken": app} }
	for _, tc := range []struct {
		name    string
		store   *secretStore
		change  string // a substring of the message, or "" for no change
		reason  bindingcheck.Reason
		changed bool
	}{
		{"the bot token rotated", &secretStore{vals: map[string]string{"botToken": canary, "appToken": app}}, "changed after the connection started", bindingcheck.ReasonChatCredentialsChanged, true},
		{"the app token rotated", &secretStore{vals: map[string]string{"botToken": bot, "appToken": canary}}, "changed after the connection started", bindingcheck.ReasonChatCredentialsChanged, true},
		{"the same tokens", &secretStore{vals: same()}, "", "", false},
		{"the app token missing and the bot token rotated: unusable wins over changed", &secretStore{vals: map[string]string{"botToken": canary}}, "gone or empty", bindingcheck.ReasonChatCredentialsUnusable, true},
		{"the app token rotated and the bot token missing: unusable wins over changed", &secretStore{vals: map[string]string{"appToken": canary}}, "gone or empty", bindingcheck.ReasonChatCredentialsUnusable, true},
		{"the Secret is of a type that is never read", &secretStore{err: fmt.Errorf("secret ns/n key %q of type %q: %w", "botToken", "kubernetes.io/tls", secret.ErrUnsupportedType)}, "never read", bindingcheck.ReasonChatCredentialsUnusable, true},
		{"the Secret is gone", &secretStore{missing: true}, "gone or empty", bindingcheck.ReasonChatCredentialsUnusable, true},
		{"a key is missing", &secretStore{vals: map[string]string{"botToken": bot}}, "gone or empty", bindingcheck.ReasonChatCredentialsUnusable, true},
		{"a key is empty", &secretStore{vals: map[string]string{"botToken": bot, "appToken": ""}}, "gone or empty", bindingcheck.ReasonChatCredentialsUnusable, true},
		{"a read that fails for another reason cannot tell", &secretStore{vals: same(), err: errors.New("cache not synced " + canary)}, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &accessFake{access: chat.Access{Member: true}}
			c := channelChecker{platform: f, timeout: time.Second, creds: credentialsWatch{
				baseline: tokens(bot, app), secrets: tc.store,
				where: func() (string, string, bool) { return "agent-kourier-system", "slack-main", true },
			}}
			out, err := c.CheckChannel(t.Context(), "C1")
			if err != nil {
				t.Fatal(err)
			}
			for _, secretText := range []string{bot, app, canary} {
				if strings.Contains(out.Message, secretText) {
					t.Errorf("the message %q holds a token", out.Message)
				}
			}
			if !tc.changed {
				if out.Status != bindingcheck.True || f.count() != 1 {
					t.Errorf("CheckChannel = %+v after %d platform calls; want True and one", out, f.count())
				}
				return
			}
			if out.Status != bindingcheck.Unknown || out.Reason != tc.reason || f.count() != 0 || !strings.Contains(out.Message, tc.change) {
				t.Errorf("CheckChannel = %+v after %d platform calls; want Unknown/%s, a message with %q, and none", out, f.count(), tc.reason, tc.change)
			}
		})
	}
}

// Nothing to compare, or nothing to compare with, is no change: no reader, no baseline, a connection the config no longer has.
func TestCredentialsWatchTellsNothingWithoutAReaderABaselineOrAConnection(t *testing.T) {
	store := &secretStore{vals: map[string]string{"botToken": "other", "appToken": "other"}}
	where := func() (string, string, bool) { return "ns", "name", true }
	gone := func() (string, string, bool) { return "", "", false }
	for name, w := range map[string]credentialsWatch{
		"no reader":     {baseline: tokens("a", "b"), where: where},
		"no baseline":   {secrets: store, where: where},
		"no where":      {baseline: tokens("a", "b"), secrets: store},
		"no connection": {baseline: tokens("a", "b"), secrets: store, where: gone},
	} {
		if got := w.check(t.Context()); got != credentialsSame {
			t.Errorf("%s: check = %d; want no change", name, got)
		}
	}
	if len(store.reads) != 0 {
		t.Errorf("the Secret was read %d times; want none", len(store.reads))
	}
}

// The Secret read is the one the current config names for the connection, in the connection's namespace: a reload that points the
// ChatConnection at another Secret is a change.
func TestVerificationReadsTheCredentialsSecretTheCurrentConfigNames(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnections(t))
	v := verification{Snaps: current, Conn: Connection{Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"}}}
	if ns, name, ok := v.credentialsSecret(); !ok || ns != "platform" || name != "slack-a" {
		t.Errorf("credentialsSecret = %q, %q, %t; want platform, slack-a", ns, name, ok)
	}
	// A ChatConnection of the same name in another namespace is another connection, with its own Secret.
	same := func(ns string) v1alpha1.ChatConnection {
		return v1alpha1.ChatConnection{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "slack-a"},
			Spec: v1alpha1.ChatConnectionSpec{Platform: "slack", CredentialsSecretRef: v1alpha1.SecretRef{Name: "creds-in-" + ns}}}
	}
	snap, err := config.Resolve(t.Context(), &config.Set{ChatConnections: []v1alpha1.ChatConnection{same("other"), same("platform")}}, keys{})
	if err != nil {
		t.Fatal(err)
	}
	current.p.Store(snap)
	for _, ns := range []string{"platform", "other"} {
		v.Conn.Ref.Namespace = ns
		if got, name, ok := v.credentialsSecret(); !ok || got != ns || name != "creds-in-"+ns {
			t.Errorf("credentialsSecret of slack-a in %s = %q, %q, %t; want %s, creds-in-%s", ns, got, name, ok, ns, ns)
		}
	}
	v.Conn.Ref.Namespace = "platform"
	current.p.Store(twoConnections(t))
	v.Conn.Ref.Name = "slack-nope"
	if _, _, ok := v.credentialsSecret(); ok {
		t.Error("credentialsSecret found a connection the config does not have")
	}
	current.p.Store(nil)
	if _, _, ok := v.credentialsSecret(); ok {
		t.Error("credentialsSecret found a Secret before the first load")
	}
}

// twoConnections is a config of two ChatConnections, slack-a and slack-b, with two Bindings on each; sre/oncall (on slack-a) and
// sre/other-bot (on slack-b) name the same channel, C1, which Resolve allows on two connections.
func twoConnections(t *testing.T, without ...string) *config.Snapshot {
	t.Helper()
	return twoConnectionsMoved(t, nil, without...)
}

// twoConnectionsMoved is twoConnections with the Bindings of moved ("namespace/name" to a connection) on that connection instead.
func twoConnectionsMoved(t *testing.T, moved map[string]string, without ...string) *config.Snapshot {
	t.Helper()
	be := v1alpha1.AgentBackend{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "kagent"}, Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: "http://x", AllowedNamespaces: []string{"sre", "ops"}}}
	conn := func(name string) v1alpha1.ChatConnection {
		return v1alpha1.ChatConnection{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: name},
			Spec: v1alpha1.ChatConnectionSpec{Platform: "slack", CredentialsSecretRef: v1alpha1.SecretRef{Name: name}, AllowedNamespaces: []string{"sre", "ops"}}}
	}
	binding := func(ns, name, connection, channel string) v1alpha1.Binding {
		return v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1alpha1.BindingSpec{
			Agent: v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "platform", Name: "kagent"}, Name: "a", Namespace: "kagent"},
			Chat:  v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "platform", Name: connection}, Channel: channel},
		}}
	}
	set := &config.Set{AgentBackends: []v1alpha1.AgentBackend{be}, ChatConnections: []v1alpha1.ChatConnection{conn("slack-a"), conn("slack-b")}}
	for _, b := range []v1alpha1.Binding{
		binding("sre", "oncall", "slack-a", "C1"), binding("ops", "oncall", "slack-a", "C2"),
		binding("sre", "other-bot", "slack-b", "C1"), // the same channel, another ChatConnection
		binding("ops", "also-other", "slack-b", "C3"),
	} {
		if to, ok := moved[b.Namespace+"/"+b.Name]; ok {
			b.Spec.Chat.ConnectionRef.Name = to
		}
		if !slices.Contains(without, b.Namespace+"/"+b.Name) {
			set.Bindings = append(set.Bindings, b)
		}
	}
	snap, err := config.Resolve(t.Context(), set, keys{})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// The Bindings to probe are those on the connection this process serves, by namespace/name, with their channels. A Binding on another
// ChatConnection, which Resolve allows to name the same channel, is left out: the bot would answer for a channel it was never asked about.
func TestBindingTargetsAreTheServedConnectionsBindingsOnly(t *testing.T) {
	snap := twoConnections(t)
	got := bindingTargets(snap, v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"})
	want := []bindingcheck.Target{{Binding: "ops/oncall", Channel: "C2"}, {Binding: "sre/oncall", Channel: "C1"}}
	if !slices.Equal(got, want) {
		t.Errorf("targets of slack-a = %v; want %v", got, want)
	}
	got = bindingTargets(snap, v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-b"})
	want = []bindingcheck.Target{{Binding: "ops/also-other", Channel: "C3"}, {Binding: "sre/other-bot", Channel: "C1"}}
	if !slices.Equal(got, want) {
		t.Errorf("targets of slack-b = %v; want %v", got, want)
	}
	if got := bindingTargets(snap, v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-c"}); len(got) != 0 {
		t.Errorf("targets of a connection no Binding names = %v; want none", got)
	}
	if got := bindingTargets(nil, v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"}); got != nil {
		t.Errorf("targets before the first load = %v; want none", got)
	}
}

// meanProbeSeconds is the mean of the observations of agentkourier_binding_probe_duration_seconds for condition, and how many there are.
func meanProbeSeconds(t *testing.T, m *Metrics, condition bindingcheck.ConditionType) (mean float64, n uint64) {
	t.Helper()
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != "agentkourier_binding_probe_duration_seconds" {
			continue
		}
		for _, s := range f.GetMetric() {
			if s.GetLabel()[0].GetValue() == string(condition) && s.GetHistogram().GetSampleCount() > 0 {
				return s.GetHistogram().GetSampleSum() / float64(s.GetHistogram().GetSampleCount()), s.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0, 0
}

// The Prober the broker builds, with everything wired: it probes the served connection's Bindings with that connection's bot and no
// other's (the channel the two connections share is asked about once, for the Binding that is on the served one), reports what it finds
// as metrics, and deletes the series of a Binding that a reload removes, once the Binding is gone from the targets.
func TestTheBrokersProberProbesTheServedConnectionAndPrunesWhatAReloadRemoves(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnections(t))
	slk := &accessFake{access: chat.Access{Member: true}}
	m := mustMetrics(t, nil)
	p := verification{
		Bindings: probeFunc(func(context.Context, string) error { time.Sleep(40 * time.Millisecond); return nil }), Platform: slk, Snaps: current,
		Revision: func(context.Context) string { return fmt.Sprint(current.p.Load()) },
		Conn:     Connection{Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"}},
		Metrics:  m, Log: slog.New(slog.DiscardHandler), Interval: time.Hour, Tick: 5 * time.Millisecond, MinRevisionGap: time.Millisecond,
	}.newProber()
	runProber(t, p)
	got := waitSettled(t, p, 2)
	if len(got) != 2 || got["sre/oncall"] == nil || got["ops/oncall"] == nil {
		t.Fatalf("probed %v; want sre/oncall and ops/oncall, the Bindings of slack-a", got)
	}
	slk.mu.Lock()
	asked := slices.Clone(slk.channels)
	slk.mu.Unlock()
	slices.Sort(asked)
	if asked = slices.Compact(asked); !slices.Equal(asked, []string{"C1", "C2"}) {
		t.Errorf("the bot was asked about %v; want C1 and C2, the channels of slack-a's Bindings, and never C3", asked)
	}
	for _, b := range []string{"sre/oncall", "ops/oncall"} {
		for _, want := range []string{
			"agentkourier_binding_ready{binding=" + b + "} 1",
			"agentkourier_binding_condition{binding=" + b + ",condition=BackendReachable,status=true} 1",
			"agentkourier_binding_condition{binding=" + b + ",condition=ChannelJoined,status=true} 1",
		} {
			if !slices.Contains(samples(t, m), want) {
				t.Errorf("no series %q in %v", want, seriesOf(t, m, "agentkourier_binding_"))
			}
		}
	}
	// Each probe is timed under its own condition: the backend's checks took the 40 ms the fake backend waited, the chat's took no time.
	if mean, n := meanProbeSeconds(t, m, bindingcheck.BackendReachable); n == 0 || mean < 0.04 {
		t.Errorf("BackendReachable probes: %d observed, mean %.4fs; want some, of at least 40 ms", n, mean)
	}
	if mean, n := meanProbeSeconds(t, m, bindingcheck.ChannelJoined); n == 0 || mean >= 0.02 {
		t.Errorf("ChannelJoined probes: %d observed, mean %.4fs; want some, of under 20 ms", n, mean)
	}
	if strings.Contains(strings.Join(samples(t, m), "\n"), "other-bot") || strings.Contains(strings.Join(samples(t, m), "\n"), "also-other") {
		t.Errorf("series for a Binding on the other connection: %v", seriesOf(t, m, "agentkourier_binding_"))
	}

	// The reload removes ops/oncall: the next run of the Prober finds it gone from the targets, and its series go with it.
	current.p.Store(twoConnections(t, "ops/oncall"))
	for deadline := time.Now().Add(10 * time.Second); strings.Contains(strings.Join(samples(t, m), "\n"), "ops/oncall"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the series of ops/oncall were not deleted: %v", seriesOf(t, m, "agentkourier_binding_"))
		}
	}
	if !slices.Contains(samples(t, m), "agentkourier_binding_ready{binding=sre/oncall} 1") {
		t.Errorf("the series of sre/oncall went with ops/oncall's: %v", seriesOf(t, m, "agentkourier_binding_"))
	}
}

// A reload that removes a Binding, or moves it to another ChatConnection than the one served, ends its verification, and its series go:
// an alert on them resolves without a word. Each such Binding gets one INFO line saying so, and a Binding still served gets none.
func TestABindingTheReloadRemovedOrMovedAwayGetsALineWhenItsVerificationStops(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnections(t))
	var buf logBuffer
	p := verification{
		Bindings: probeFunc(func(context.Context, string) error { return nil }), Platform: &accessFake{access: chat.Access{Member: true}}, Snaps: current,
		Revision: func(context.Context) string { return fmt.Sprint(current.p.Load()) },
		Conn:     Connection{Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"}},
		Metrics:  mustMetrics(t, nil), Log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Interval: time.Hour, Tick: 5 * time.Millisecond, MinRevisionGap: time.Millisecond,
	}.newProber()
	runProber(t, p)
	waitSettled(t, p, 2)
	if got := buf.String(); strings.Contains(got, "stopped verifying") {
		t.Fatalf("a line of stopped verification while both Bindings are served:\n%s", got)
	}

	// ops/oncall moves to slack-b and sre/oncall is removed.
	current.p.Store(twoConnectionsMoved(t, map[string]string{"ops/oncall": "slack-b"}, "sre/oncall"))
	eventually(t, 10*time.Second, "the lines of the stopped verifications", func() bool { return strings.Count(buf.String(), "stopped verifying") == 2 })
	for _, b := range []string{"sre/oncall", "ops/oncall"} {
		if n := countOf(buf.String(), "level=INFO", "stopped verifying a Binding", "binding="+b); n != 1 {
			t.Errorf("%d lines that the verification of %s stopped; want one\n%s", n, b, buf.String())
		}
	}
}

// The broker's Prober does not probe every Binding at each change of the revision: with no gap passed in, a revision that changes at
// every Tick, a Secret written to again and again, gives the first pass and no more inside minRevisionGap.
func TestTheBrokersProberHoldsRevisionPassesToTheMinimumGap(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnections(t, "ops/oncall"))
	var calls, revision atomic.Int64
	p := verification{
		Bindings: probeFunc(func(context.Context, string) error { calls.Add(1); return nil }), Platform: &accessFake{access: chat.Access{Member: true}},
		Snaps: current, Revision: func(context.Context) string { return fmt.Sprint(revision.Add(1)) },
		Conn:    Connection{Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"}},
		Metrics: mustMetrics(t, nil), Log: slog.New(slog.DiscardHandler), Interval: time.Hour, Tick: 2 * time.Millisecond,
	}.newProber()
	runProber(t, p)
	waitSettled(t, p, 1)
	before := revision.Load()
	eventually(t, 10*time.Second, "the revision did not move 50 times", func() bool { return revision.Load() >= before+50 })
	if got := calls.Load(); got != 1 {
		t.Errorf("%d backend probes while the revision changed at every Tick; want 1: the pass for a change waits %s", got, minRevisionGap)
	}
}

// A backend that never answers and a chat platform that never answers are cut by the adapters' own bounds, which the verification derives
// from the Prober's timeout (innerBound): they are reported as what they are, False/Unreachable and Unknown/ChatUnavailable, and not as
// the Prober's own Unknown/ProbeTimeout, which says nothing of either. Were the adapters given the Prober's timeout itself, the Prober
// would cut the probe first.
func TestTheBrokersProberReportsAHungBackendAndAHungChatAsWhatTheyAreAndNotAsProbeTimeout(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnections(t, "ops/oncall"))
	hungBackend := probeFunc(func(ctx context.Context, _ string) error { <-ctx.Done(); return fmt.Errorf("card: %w", ctx.Err()) })
	p := verification{
		Bindings: hungBackend, Platform: &accessFake{wait: true}, Snaps: current, Revision: func(context.Context) string { return "r" },
		Conn:    Connection{Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"}},
		Metrics: mustMetrics(t, nil), Log: slog.New(slog.DiscardHandler), Timeout: 300 * time.Millisecond, Interval: time.Hour, Tick: 5 * time.Millisecond,
	}.newProber()
	runProber(t, p)
	conds := waitSettled(t, p, 1)["sre/oncall"]
	if c := conds[bindingcheck.BackendReachable]; c.Status != bindingcheck.False || c.Reason != bindingcheck.ReasonUnreachable {
		t.Errorf("BackendReachable is %s/%s; want False/Unreachable", c.Status, c.Reason)
	}
	if c := conds[bindingcheck.ChannelJoined]; c.Status != bindingcheck.Unknown || c.Reason != bindingcheck.ReasonChatUnavailable {
		t.Errorf("ChannelJoined is %s/%s; want Unknown/ChatUnavailable", c.Status, c.Reason)
	}
}

// A failure that is not a probe's (Targets or Revision panicked or timed out, OnPrune panicked) has no Binding and no condition, and has a
// line of its own and not "a probe failed" with empty fields; a panic's stack is in the line of either, redacted and cut.
func TestProbeErrorGivesAFailureWithNoBindingItsOwnLineAndLogsAPanicsStack(t *testing.T) {
	stack := []byte("goroutine 7 [running]:\nruntime/debug.Stack()\ngithub.com/agentkourier/agent-kourier/internal/broker.listTargets()\n\tfile.go:12 +0x1")
	panicked := fmt.Errorf("calling Targets: %w", &bindingcheck.PanicError{Value: "boom Bearer abc.def.ghi", Stack: stack})

	out := logLines(func(l bindingLog) { l.probeError(bindingcheck.ProbeError{Err: panicked}) })
	for _, want := range []string{"level=WARN", "the prober failed", "calling Targets", "stack=", "runtime/debug.Stack()", "listTargets"} {
		if !strings.Contains(out, want) {
			t.Errorf("log line %q lacks %q", out, want)
		}
	}
	for _, bad := range []string{"a probe failed", "binding=", "condition=", "abc.def.ghi"} {
		if strings.Contains(out, bad) {
			t.Errorf("log line %q holds %q", out, bad)
		}
	}

	out = logLines(func(l bindingLog) {
		l.probeError(bindingcheck.ProbeError{Condition: bindingcheck.ChannelJoined, Target: bindingcheck.Target{Binding: "sre/oncall"}, Err: panicked})
	})
	for _, want := range []string{"a probe failed", "binding=sre/oncall", "condition=ChannelJoined", "stack="} {
		if !strings.Contains(out, want) {
			t.Errorf("log line %q lacks %q", out, want)
		}
	}

	out = logLines(func(l bindingLog) {
		l.probeError(bindingcheck.ProbeError{Err: errors.New("revision did not answer within 10s")})
	})
	if !strings.Contains(out, "the prober failed") || strings.Contains(out, "stack=") {
		t.Errorf("log line %q; want the prober's own line and no stack for an error that is no panic", out)
	}
}

// logLines runs fn with a logger that keeps every level and returns what it wrote.
func logLines(fn func(bindingLog)) string {
	var buf logBuffer
	fn(bindingLog{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	return buf.String()
}

// tr is a transition of the BackendReachable condition from old to new.
func tr(oldStatus bindingcheck.Status, oldReason bindingcheck.Reason, newStatus bindingcheck.Status, newReason bindingcheck.Reason) bindingcheck.Transition {
	t := bindingcheck.Transition{Binding: "sre/oncall", New: bindingcheck.Condition{Type: bindingcheck.BackendReachable, Status: newStatus, Reason: newReason, Message: "the backend answered 404"}}
	if oldStatus != "" {
		t.Old = bindingcheck.Condition{Type: bindingcheck.BackendReachable, Status: oldStatus, Reason: oldReason}
	}
	return t
}

// A condition that goes False is a WARN, so is Unknown/ChatCredentialsChanged, any other Unknown is an INFO (a first result too), and
// one that is True again is an INFO. Silent: a first result that is True, Pending, Unknown/ProbeFailed (probeError logs it with its
// error), and Ready, whose reason shifts as the conditions of a first pass arrive.
func TestTransitionLogsFalseAsWarnAndRecoveryAsInfoAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tr    bindingcheck.Transition
		level string
	}{
		{"true to false", tr(bindingcheck.True, bindingcheck.ReasonOK, bindingcheck.False, bindingcheck.ReasonAgentNotFound), "level=WARN"},
		{"first result is false", tr(bindingcheck.Unknown, bindingcheck.ReasonPending, bindingcheck.False, bindingcheck.ReasonAgentNotFound), "level=WARN"},
		{"false to another false", tr(bindingcheck.False, bindingcheck.ReasonAgentNotFound, bindingcheck.False, bindingcheck.ReasonAgentForbidden), "level=WARN"},
		{"false to true", tr(bindingcheck.False, bindingcheck.ReasonAgentNotFound, bindingcheck.True, bindingcheck.ReasonOK), "level=INFO"},
		{"unknown after a probe to true", tr(bindingcheck.Unknown, bindingcheck.ReasonProbeFailed, bindingcheck.True, bindingcheck.ReasonOK), "level=INFO"},
		{"first result is true", tr(bindingcheck.Unknown, bindingcheck.ReasonPending, bindingcheck.True, bindingcheck.ReasonOK), ""},
		{"true to unknown", tr(bindingcheck.True, bindingcheck.ReasonOK, bindingcheck.Unknown, bindingcheck.ReasonProbeTimeout), "level=INFO"},
		{"a first result that is unknown", tr(bindingcheck.Unknown, bindingcheck.ReasonPending, bindingcheck.Unknown, bindingcheck.ReasonUnexpectedStatus), "level=INFO"},
		{"unknown to another unknown", tr(bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable, bindingcheck.Unknown, bindingcheck.ReasonProbeTimeout), "level=INFO"},
		{"chat credentials changed", tr(bindingcheck.True, bindingcheck.ReasonOK, bindingcheck.Unknown, bindingcheck.ReasonChatCredentialsChanged), "level=WARN"},
		{"a first result that is chat credentials changed", tr(bindingcheck.Unknown, bindingcheck.ReasonPending, bindingcheck.Unknown, bindingcheck.ReasonChatCredentialsChanged), "level=WARN"},
		{"changed to unusable", tr(bindingcheck.Unknown, bindingcheck.ReasonChatCredentialsChanged, bindingcheck.Unknown, bindingcheck.ReasonChatCredentialsUnusable), "level=WARN"},
		{"unusable to changed", tr(bindingcheck.Unknown, bindingcheck.ReasonChatCredentialsUnusable, bindingcheck.Unknown, bindingcheck.ReasonChatCredentialsChanged), "level=WARN"},
		{"true to unusable", tr(bindingcheck.True, bindingcheck.ReasonOK, bindingcheck.Unknown, bindingcheck.ReasonChatCredentialsUnusable), "level=WARN"},
		{"true to a probe that could not tell", tr(bindingcheck.True, bindingcheck.ReasonOK, bindingcheck.Unknown, bindingcheck.ReasonProbeFailed), ""},
		{"a first result that is a probe that could not tell", tr(bindingcheck.Unknown, bindingcheck.ReasonPending, bindingcheck.Unknown, bindingcheck.ReasonProbeFailed), ""},
		{"Ready going false", func() bindingcheck.Transition {
			t := tr(bindingcheck.True, bindingcheck.ReasonOK, bindingcheck.False, "BackendReachable:AgentNotFound")
			t.New.Type = bindingcheck.Ready
			return t
		}(), ""},
		{"Ready shifting its reason in a first pass", func() bindingcheck.Transition {
			t := tr(bindingcheck.Unknown, "BackendReachable:Pending", bindingcheck.Unknown, "ChannelJoined:Pending")
			t.New.Type = bindingcheck.Ready
			return t
		}(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := logLines(func(l bindingLog) { l.transition(tc.tr) })
			if tc.level == "" {
				if out != "" {
					t.Errorf("logged %q; want nothing", out)
				}
				return
			}
			for _, want := range []string{tc.level, "binding=sre/oncall", "condition=BackendReachable", "reason=" + string(tc.tr.New.Reason)} {
				if !strings.Contains(out, want) {
					t.Errorf("log line %q lacks %q", out, want)
				}
			}
		})
	}
}

// A failed probe is a WARN with the checker's error as telemetry.LogText gives it: redacted, and cut. A Binding the config no longer has is
// no failure and is not logged.
func TestProbeErrorIsLoggedRedactedAndAGoneBindingIsNot(t *testing.T) {
	pe := bindingcheck.ProbeError{Condition: bindingcheck.BackendReachable, Target: bindingcheck.Target{Binding: "sre/oncall"},
		Err: errors.New("card: Authorization: Bearer abc.def.ghi rejected")}
	out := logLines(func(l bindingLog) { l.probeError(pe) })
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "binding=sre/oncall") || !strings.Contains(out, "condition=BackendReachable") {
		t.Errorf("log line %q; want a WARN naming the Binding and the condition", out)
	}
	if strings.Contains(out, "abc.def.ghi") {
		t.Errorf("log line %q holds a token", out)
	}
	long := bindingcheck.ProbeError{Err: errors.New(strings.Repeat("x", 5000))}
	if out := logLines(func(l bindingLog) { l.probeError(long) }); len(out) > 1000 {
		t.Errorf("a %d byte log line for a 5000 byte error; want it cut", len(out))
	}
	gone := bindingcheck.ProbeError{Condition: bindingcheck.BackendReachable, Target: bindingcheck.Target{Binding: "sre/oncall"},
		Err: fmt.Errorf("check the agent of Binding sre/oncall: %w", fmt.Errorf("%w: sre/oncall", session.ErrUnknownBinding))}
	if out := logLines(func(l bindingLog) { l.probeError(gone) }); out != "" {
		t.Errorf("logged %q for a Binding that left the config; want nothing", out)
	}
}

// runProber runs p until the test ends or the returned stop is called, which waits for Run to return.
func runProber(t *testing.T, p *bindingcheck.Prober) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

// waitSettled waits until each of the n Bindings p holds has left Pending in every condition it has, and returns the conditions.
func waitSettled(t *testing.T, p *bindingcheck.Prober, n int) map[string]map[bindingcheck.ConditionType]bindingcheck.Condition {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := map[string]map[bindingcheck.ConditionType]bindingcheck.Condition{}
		pending := false
		for b, conds := range p.State().Snapshot() {
			got[b] = map[bindingcheck.ConditionType]bindingcheck.Condition{}
			for _, c := range conds {
				got[b][c.Type] = c
				pending = pending || c.Pending()
			}
		}
		if len(got) == n && !pending {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Prober did not settle %d Bindings; conditions: %+v", n, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settled runs p until each of the n Bindings has left Pending, stops it, and returns the conditions.
func settled(t *testing.T, p *bindingcheck.Prober, n int) map[string]map[bindingcheck.ConditionType]bindingcheck.Condition {
	t.Helper()
	stop := runProber(t, p)
	defer stop()
	return waitSettled(t, p, n)
}

// Every outcome the classifiers can give is one the Prober accepts from a checker (Reason.reportable), and not recorded as a failed probe:
// the table of statuses and reasons goes through a real Prober, and the condition it holds is the one the classifier meant. An error the
// classifier cannot place is Unknown/ProbeFailed there, and the Prober tells OnProbeError.
func TestEveryClassifiedOutcomeIsOneTheProberTakesFromAChecker(t *testing.T) {
	status := func(code int) error { return &dialect.HTTPStatusError{Code: code} }
	errs := map[string]error{
		"sre/unreadable": agent.ErrTokenUnreadable, "sre/rejected": status(401), "sre/forbidden": status(403), "sre/missing": status(404),
		"sre/notready": status(409), "sre/throttled": status(429), "sre/broken": status(502), "sre/redirected": status(307),
		"sre/nocard": dialect.ErrInvalidAgentCard, "sre/nostream": agent.ErrStreamingUnsupported,
		"sre/extension": dialect.ErrUnsupportedExtension, "sre/nojsonrpc": dialect.ErrNoJSONRPCInterface,
		"sre/down": &net.OpError{Op: "dial"}, "sre/ok": nil, "sre/odd": errors.New("something unplaced"),
	}
	wants := map[string]want{
		"sre/unreadable": {bindingcheck.False, bindingcheck.ReasonCredentialsUnreadable}, "sre/rejected": {bindingcheck.False, bindingcheck.ReasonCredentialsRejected},
		"sre/forbidden": {bindingcheck.False, bindingcheck.ReasonAgentForbidden}, "sre/missing": {bindingcheck.False, bindingcheck.ReasonAgentNotFound},
		"sre/notready": {bindingcheck.False, bindingcheck.ReasonAgentNotReady}, "sre/throttled": {bindingcheck.Unknown, bindingcheck.ReasonUnexpectedStatus},
		"sre/broken": {bindingcheck.False, bindingcheck.ReasonBackendError}, "sre/redirected": {bindingcheck.False, bindingcheck.ReasonUnexpectedStatus},
		"sre/nocard": {bindingcheck.False, bindingcheck.ReasonInvalidAgentCard}, "sre/nostream": {bindingcheck.False, bindingcheck.ReasonStreamingUnsupported},
		"sre/extension": {bindingcheck.False, bindingcheck.ReasonUnsupportedExtension}, "sre/nojsonrpc": {bindingcheck.False, bindingcheck.ReasonNoJSONRPCInterface},
		"sre/down": {bindingcheck.False, bindingcheck.ReasonUnreachable}, "sre/ok": {bindingcheck.True, bindingcheck.ReasonOK},
		"sre/odd": {bindingcheck.Unknown, bindingcheck.ReasonProbeFailed},
	}
	var targets []bindingcheck.Target
	for b := range errs {
		targets = append(targets, bindingcheck.Target{Binding: b, Channel: "C1"})
	}
	var mu sync.Mutex
	var failed []string
	p := bindingcheck.NewProber(bindingcheck.Options{
		Targets: func() []bindingcheck.Target { return targets },
		Agents:  agentChecker{bindings: probeFunc(func(_ context.Context, b string) error { return errs[b] }), timeout: time.Second},
		Tick:    5 * time.Millisecond, Interval: time.Hour,
		OnProbeError: func(pe bindingcheck.ProbeError) { mu.Lock(); failed = append(failed, pe.Target.Binding); mu.Unlock() },
	})
	got := settled(t, p, len(targets))
	for b, w := range wants {
		c := got[b][bindingcheck.BackendReachable]
		if c.Status != w.status || c.Reason != w.reason {
			t.Errorf("%s: BackendReachable is %s/%s; want %s/%s", b, c.Status, c.Reason, w.status, w.reason)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(failed, []string{"sre/odd"}) {
		t.Errorf("OnProbeError was told of %v; want only sre/odd", failed)
	}
}

// The same for the channel checker: each of its outcomes is accepted, including the ones whose status a reason fixes.
func TestEveryChannelOutcomeIsOneTheProberTakesFromAChecker(t *testing.T) {
	fakes := map[string]*accessFake{
		"C-ok":      {access: chat.Access{Member: true}},
		"C-out":     {},
		"C-archive": {access: chat.Access{Archived: true}},
		"C-none":    {err: chat.ErrChannelNotFound},
		"C-scope":   {err: chat.ErrMissingScope},
		"C-auth":    {err: errors.New("rejected"), verdict: chat.Verdict{Class: chat.FatalAuth}},
		"C-busy":    {err: errors.New("503")},
	}
	wants := map[string]want{
		"C-ok": {bindingcheck.True, bindingcheck.ReasonOK}, "C-out": {bindingcheck.False, bindingcheck.ReasonNotInChannel},
		"C-archive": {bindingcheck.False, bindingcheck.ReasonChannelArchived}, "C-none": {bindingcheck.False, bindingcheck.ReasonChannelNotFound},
		"C-scope": {bindingcheck.False, bindingcheck.ReasonMissingScope}, "C-auth": {bindingcheck.False, bindingcheck.ReasonChatCredentialsRejected},
		"C-busy": {bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable},
	}
	var targets []bindingcheck.Target
	for ch := range fakes {
		targets = append(targets, bindingcheck.Target{Binding: "sre/" + ch, Channel: ch})
	}
	p := bindingcheck.NewProber(bindingcheck.Options{
		Targets:  func() []bindingcheck.Target { return targets },
		Channels: channelChecker{platform: byChannel(fakes), timeout: time.Second},
		Tick:     5 * time.Millisecond, Interval: time.Hour,
		OnProbeError: func(pe bindingcheck.ProbeError) { t.Errorf("a probe failed: %v", pe.Err) },
	})
	got := settled(t, p, len(targets))
	for ch, w := range wants {
		c := got["sre/"+ch][bindingcheck.ChannelJoined]
		if c.Status != w.status || c.Reason != w.reason {
			t.Errorf("%s: ChannelJoined is %s/%s; want %s/%s", ch, c.Status, c.Reason, w.status, w.reason)
		}
	}
}

// byChannel is a channelReader that answers each channel with its own fake.
type byChannel map[string]*accessFake

func (b byChannel) ChannelAccess(ctx context.Context, channel string) (chat.Access, error) {
	return b[channel].ChannelAccess(ctx, channel)
}

func (b byChannel) Classify(op chat.Op, err error) chat.Verdict {
	for _, f := range b {
		if errors.Is(err, f.err) {
			return f.Classify(op, err)
		}
	}
	return chat.Verdict{Class: chat.Transient}
}

// A Binding the config loses during a pass is left Unknown by a probe that could not tell, never False: no WARN and no false alarm for a
// Binding on its way out, and the Prober prunes it with the series that were set for it.
func TestABindingRemovedDuringAPassIsNeverFalseAndIsPruned(t *testing.T) {
	m := mustMetrics(t, nil)
	var buf logBuffer
	l := bindingLog{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	var mu sync.Mutex
	targets := []bindingcheck.Target{{Binding: "sre/oncall", Channel: "C1"}}
	removed := atomic.Bool{}
	var statuses []bindingcheck.Status
	p := bindingcheck.NewProber(bindingcheck.Options{
		Targets: func() []bindingcheck.Target { mu.Lock(); defer mu.Unlock(); return targets },
		Agents: agentChecker{timeout: time.Second, bindings: probeFunc(func(context.Context, string) error {
			if removed.Load() {
				return fmt.Errorf("%w: sre/oncall", session.ErrUnknownBinding)
			}
			return nil
		})},
		Channels: channelChecker{platform: &accessFake{access: chat.Access{Member: true}}, timeout: time.Second},
		Tick:     5 * time.Millisecond, Interval: 40 * time.Millisecond,
		OnTransition: func(t bindingcheck.Transition) {
			m.BindingCondition(t)
			l.transition(t)
			mu.Lock()
			statuses = append(statuses, t.New.Status)
			mu.Unlock()
		},
		OnProbeError: l.probeError,
		OnPrune:      m.BindingsPruned,
	})
	runProber(t, p)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("never saw %s; series %v", what, seriesOf(t, m, "agentkourier_binding_"))
			}
		}
	}
	waitFor("the Binding ready", func() bool {
		return slices.Contains(seriesOf(t, m, "agentkourier_binding_ready"), "agentkourier_binding_ready{binding=sre/oncall} 1")
	})
	removed.Store(true) // the next probe finds the Binding gone, and the next Tick finds it gone from the targets
	waitFor("the Binding's backend unknown", func() bool {
		return slices.Contains(seriesOf(t, m, "agentkourier_binding_condition"), "agentkourier_binding_condition{binding=sre/oncall,condition=BackendReachable,status=unknown} 1")
	})
	mu.Lock()
	targets = nil
	mu.Unlock()
	waitFor("the series pruned", func() bool { return len(seriesOf(t, m, "agentkourier_binding_")) == 0 })

	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(statuses, bindingcheck.False) {
		t.Errorf("a condition went False for a Binding that was removed: %v", statuses)
	}
	if got := buf.String(); got != "" {
		t.Errorf("logged %q for a Binding that was removed; want nothing", got)
	}
}
