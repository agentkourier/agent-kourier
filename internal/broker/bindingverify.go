package broker

import (
	"cmp"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/identity"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// bindingProbeTimeout is how long the Prober gives one probe. The Prober cuts a probe at that bound and calls it
// Unknown/ProbeTimeout, which says nothing of the backend, so each adapter bounds its own call a little earlier (innerBound), and a
// backend or a chat platform that does not answer is reported as what it is (Unreachable, ChatUnavailable).
const bindingProbeTimeout = 10 * time.Second

// innerBound is the bound of an adapter's own call, given the Prober's timeout: four fifths of it. The chat bound is its own and
// not interact.PressAuthzTimeout, which the Slack client puts on the call underneath (members.go: ChannelAccess), so the bound that
// takes effect is the lesser of the two (10s in production, which is the greater); retuning the press bound moves the probe's only
// if it is lowered below this one.
func innerBound(outer time.Duration) time.Duration { return outer * 4 / 5 }

// agentProbe is what the agent checker needs of Bindings: (*Bindings).CheckAgent, a card fetch that sends the agent nothing.
type agentProbe interface {
	CheckAgent(ctx context.Context, binding string) error
}

// agentChecker is the bindingcheck.AgentChecker of a running broker: it asks Bindings to fetch the card of the Binding's agent,
// bounded by timeout, and says what the answer means for BackendReachable (classifyAgent). It runs no turn and sends the agent no
// message.
type agentChecker struct {
	bindings agentProbe
	timeout  time.Duration
	observe  func(time.Duration) // told how long each check took; nil is nobody
}

var _ bindingcheck.AgentChecker = agentChecker{}

// CheckAgent implements bindingcheck.AgentChecker. A check the Prober's context ended is no verdict on the backend and comes
// back as an error, as does an error classifyAgent cannot place and a credentials read that the checker's own bound cut short; the deadline of the checker's own bound is a backend that does
// not answer, Unreachable. A Binding the config no longer has (a reload removed it during the pass) is no result either: the
// error says so and the Prober prunes the Binding at its next Tick.
func (a agentChecker) CheckAgent(ctx context.Context, binding string) (bindingcheck.Outcome, error) {
	began := time.Now()
	defer func() {
		if a.observe != nil {
			a.observe(time.Since(began))
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	err := a.bindings.CheckAgent(cctx, binding)
	if err == nil {
		return bindingcheck.Outcome{Status: bindingcheck.True, Reason: bindingcheck.ReasonOK}, nil
	}
	if ctx.Err() != nil {
		return bindingcheck.Outcome{}, fmt.Errorf("the probe ended before the backend answered: %w", err)
	}
	if cctx.Err() != nil && (errors.Is(err, agent.ErrTokenUnreadable) || errors.Is(err, agent.ErrHeaderUnreadable)) {
		// A read of a Secret that the checker's own bound cut short did not find the Secret unreadable.
		return bindingcheck.Outcome{}, fmt.Errorf("the credentials were not read before the checker's bound passed: %w", err)
	}
	if out, ok := classifyAgent(err); ok {
		return out, nil
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) { // the checker's own bound passed, in whatever shape the error took
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonUnreachable,
			Message: fmt.Sprintf("the backend did not answer within %s", a.timeout)}, nil
	}
	return bindingcheck.Outcome{}, fmt.Errorf("check the agent of Binding %s: %w", binding, err)
}

// classifyAgent says what the error of a card fetch (agent.Pool.Check) means for BackendReachable. It is False when the answer
// says a turn would fail now, and Unknown for the statuses that say nothing of the agent (408, 425 and 429). The Message names
// the class and a status code, never the error's text, which can quote a response. It reports false for an error it cannot place,
// which the Prober records as a probe that could not tell.
func classifyAgent(err error) (bindingcheck.Outcome, bool) {
	fail := func(r bindingcheck.Reason, msg string) (bindingcheck.Outcome, bool) {
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: r, Message: msg}, true
	}
	var status *dialect.HTTPStatusError
	var notOK *agentcard.ErrStatusNotOK
	switch {
	case errors.Is(err, agent.ErrTokenUnreadable), errors.Is(err, agent.ErrHeaderUnreadable), errors.Is(err, agent.ErrTokenRequired):
		return fail(bindingcheck.ReasonCredentialsUnreadable, "the Binding's token, or a Secret behind a header of the backend, cannot be read")
	case errors.As(err, &status):
		return classifyStatus(status.Code), true
	case errors.As(err, &notOK):
		return classifyStatus(notOK.StatusCode), true
	case errors.Is(err, agent.ErrStreamingUnsupported):
		return fail(bindingcheck.ReasonStreamingUnsupported, "the agent card does not advertise streaming")
	case errors.Is(err, dialect.ErrUnsupportedExtension):
		return fail(bindingcheck.ReasonUnsupportedExtension, "the agent card requires an extension Agent Kourier does not speak")
	case errors.Is(err, dialect.ErrNoJSONRPCInterface):
		return fail(bindingcheck.ReasonNoJSONRPCInterface, "the agent card lists no JSON-RPC interface")
	case errors.Is(err, dialect.ErrInvalidAgentCard):
		return fail(bindingcheck.ReasonInvalidAgentCard, "the backend answered 200 with something that is not an agent card")
	case didNotAnswer(err):
		return fail(bindingcheck.ReasonUnreachable, "the backend did not answer: the connection, the TLS handshake or the response failed or timed out")
	}
	return bindingcheck.Outcome{}, false
}

// classifyStatus is classifyAgent for an HTTP status the card fetch got. A 2xx other than 200 is what the card resolver refuses
// with agentcard.ErrStatusNotOK.
func classifyStatus(code int) bindingcheck.Outcome {
	out := bindingcheck.Outcome{Status: bindingcheck.False, Message: fmt.Sprintf("the backend answered %d", code)}
	switch {
	case code == 401:
		out.Reason = bindingcheck.ReasonCredentialsRejected
	case code == 403:
		out.Reason, out.Message = bindingcheck.ReasonAgentForbidden, "the backend answered 403: the agent is not found or not permitted for this identity"
	case code == 404:
		out.Reason = bindingcheck.ReasonAgentNotFound
	case code == 409:
		out.Reason = bindingcheck.ReasonAgentNotReady
	case code == 408 || code == 425 || code == 429:
		out.Status, out.Reason, out.Message = bindingcheck.Unknown, bindingcheck.ReasonUnexpectedStatus, fmt.Sprintf("the backend answered %d, which says nothing about the agent", code)
	case code >= 500 || (code >= 200 && code < 300):
		out.Reason = bindingcheck.ReasonBackendError
	default: // a 3xx, often a redirect to a login page, and a 4xx no reason names
		out.Reason = bindingcheck.ReasonUnexpectedStatus
	}
	return out
}

// didNotAnswer reports whether err is a failure to get an answer: a dial, DNS, TLS or read that failed, or a deadline that
// passed. A net.Error covers the *url.Error of the HTTP client, a *net.OpError, a *net.DNSError and context.DeadlineExceeded; the
// two TLS errors and an end of the stream are not net.Errors. A status the backend answered with is found before this is asked,
// since the HTTP client wraps that in a *url.Error too.
func didNotAnswer(err error) bool {
	var (
		netErr net.Error
		cert   *tls.CertificateVerificationError
		record tls.RecordHeaderError
	)
	return errors.As(err, &netErr) || errors.As(err, &cert) || errors.As(err, &record) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// channelReader is what the channel checker needs of the chat platform: chat.Platform's ChannelAccess and Classify.
type channelReader interface {
	ChannelAccess(ctx context.Context, channel string) (chat.Access, error)
	Classify(op chat.Op, err error) chat.Verdict
}

// channelChecker is the bindingcheck.ChannelChecker of a running broker. It answers for the bot of the connection the broker
// serves, so the Targets it is given hold only Bindings on that connection (bindingTargets).
type channelChecker struct {
	platform channelReader
	timeout  time.Duration
	creds    credentialsWatch
	observe  func(time.Duration) // told how long each check took; nil is nobody
}

var _ bindingcheck.ChannelChecker = channelChecker{}

// CheckChannel implements bindingcheck.ChannelChecker. It makes no call when the connection's credentials Secret holds other tokens
// than the broker is using (ChatCredentialsChanged): the bot the broker holds is then not the one the Secret names, and what it
// sees is not a finding about the channel. A call the Prober's context ended is no verdict and comes back as an error; the
// deadline of the checker's own bound is a chat platform that does not answer, Unknown/ChatUnavailable.
func (c channelChecker) CheckChannel(ctx context.Context, channel string) (bindingcheck.Outcome, error) {
	began := time.Now()
	defer func() {
		if c.observe != nil {
			c.observe(time.Since(began))
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	switch c.creds.check(cctx) {
	case credentialsChanged:
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatCredentialsChanged,
			Message: "the chat credentials changed after the connection started: restart Agent Kourier to use the new ones"}, nil
	case credentialsGone:
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatCredentialsChanged,
			Message: "the chat credentials Secret, or a key of it, is gone or empty: the connection still uses the tokens it read, and a restart fails until the Secret is restored"}, nil
	}
	access, err := c.platform.ChannelAccess(cctx, channel)
	if err != nil {
		if ctx.Err() != nil {
			return bindingcheck.Outcome{}, fmt.Errorf("the probe ended before the chat platform answered: %w", err)
		}
		return c.classifyError(err), nil
	}
	switch {
	case access.Archived: // before Member: a platform may call the bot a member of a channel that was archived under it
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonChannelArchived, Message: "the channel is archived"}, nil
	case !access.Member:
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonNotInChannel,
			Message: "the bot is not in the channel (invite it), or the channel is private and the bot cannot see it"}, nil
	}
	return bindingcheck.Outcome{Status: bindingcheck.True, Reason: bindingcheck.ReasonOK}, nil
}

// classifyError says what an error of ChannelAccess means for ChannelJoined: the two sentinels are False, a credential the platform
// rejects is False/ChatCredentialsRejected, and anything else (a timeout, a rate limit, a failure of the platform) is Unknown.
func (c channelChecker) classifyError(err error) bindingcheck.Outcome {
	switch {
	case errors.Is(err, chat.ErrChannelNotFound):
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonChannelNotFound,
			Message: "the channel does not exist, or the bot cannot see it (a private channel the bot is not in looks like this)"}
	case errors.Is(err, chat.ErrMissingScope):
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonMissingScope, Message: "the app lacks the permission to read the channel"}
	}
	v := c.platform.Classify(chat.OpOther, err)
	switch {
	case v.Class == chat.FatalAuth:
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonChatCredentialsRejected,
			Message: "the chat platform rejected the bot's token: replace it and restart Agent Kourier"}
	case v.RetryAfter > 0:
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatUnavailable, Message: "the chat platform rate-limited the read"}
	}
	return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatUnavailable, Message: "the chat platform did not answer the read"}
}

// credentialsChange is what credentialsWatch found.
type credentialsChange int

const (
	credentialsSame    credentialsChange = iota // the tokens are the ones in use, or the check could not tell
	credentialsChanged                          // the Secret holds other tokens than the connection is using
	credentialsGone                             // the Secret, or one of the keys, is gone or empty
)

// credentialsWatch tells whether the Secret that holds the connection's credentials holds other tokens than the connection is
// using. The baseline is Connection.Credentials, which is what the chat platform was built with, and the Secrets are read at the
// probe, from the cache of the Secrets' reader, so a write that leaves the tokens alone (an annotation, a label) is no change, and no
// call is made to the chat platform. A value is compared and never kept, shown or returned.
type credentialsWatch struct {
	baseline map[string]secret.Value
	secrets  identity.SecretReader // nil: no change is ever told
	// where says which Secret the connection's credentials are in now, by the current config: false when the config no longer has the
	// connection, which the check cannot tell anything of.
	where func() (namespace, name string, ok bool)
}

// check compares the tokens in the Secret now with the baseline. A Secret that does not exist, or a key that is missing or empty, is
// credentialsGone; a token that differs is credentialsChanged. Any other failure to read a key, and no baseline, means it cannot tell
// for that key, which is no change.
func (w credentialsWatch) check(ctx context.Context) credentialsChange {
	if w.secrets == nil || w.where == nil || len(w.baseline) == 0 {
		return credentialsSame
	}
	namespace, name, ok := w.where()
	if !ok {
		return credentialsSame
	}
	found := credentialsSame
	for key, was := range w.baseline {
		now, err := w.secrets.Value(ctx, namespace, name, key)
		switch {
		case err == nil && !now.IsEmpty():
			if subtle.ConstantTimeCompare([]byte(now.Reveal()), []byte(was.Reveal())) != 1 {
				found = max(found, credentialsChanged)
			}
		case err == nil, errors.Is(err, secret.ErrKeyNotFound), errors.Is(err, secret.ErrEmptyValue), errors.Is(err, config.ErrSecretNotFound):
			found = credentialsGone
		}
	}
	return found
}

// bindingTargets lists the Bindings to probe: those of snap whose ChatConnection is conn, the one this process serves. The
// channel checker reads with that connection's bot, so a Binding on any other would be told the wrong bot's answer. A
// Binding's key is namespace/name, which is what CheckAgent looks it up by.
func bindingTargets(snap *config.Snapshot, conn v1alpha1.ObjectRef) []bindingcheck.Target {
	if snap == nil {
		return nil
	}
	var out []bindingcheck.Target
	for _, rb := range snap.Bindings() {
		if rb.Connection.Namespace != conn.Namespace || rb.Connection.Name != conn.Name {
			continue
		}
		out = append(out, bindingcheck.Target{
			Binding: v1alpha1.NamespacedName(rb.Binding.Namespace, rb.Binding.Name),
			Channel: rb.Binding.Spec.Chat.Channel,
		})
	}
	return out
}

// bindingLog is the log of the verification of Bindings: one line when a condition changes to False, Unknown or True again, and one for
// a probe whose checker could not tell. It writes nothing for a probe that found what it found before.
type bindingLog struct{ log *slog.Logger }

// transition is bindingcheck.Options.OnTransition, with a line for every change of a condition to a status and reason that
// has not been told:
//   - False is a WARN, and so is Unknown/ChatCredentialsChanged, which asks for a restart;
//   - any other Unknown is an INFO, a Binding's first result included;
//   - True after a condition that was not True (and was not Pending) is an INFO.
//
// Nothing is logged for Ready, which holds back what the line for the condition that causes it says; for Pending, which is no
// result; and for Unknown/ProbeFailed, which probeError logs with the error that caused it.
func (l bindingLog) transition(tr bindingcheck.Transition) {
	c := tr.New
	if c.Type == bindingcheck.Ready || c.Reason == bindingcheck.ReasonPending || c.Reason == bindingcheck.ReasonProbeFailed {
		return
	}
	attrs := []any{"binding", tr.Binding, "condition", string(c.Type), "status", string(c.Status), "reason", string(c.Reason), "detail", telemetry.LogText(c.Message)}
	switch {
	case c.Status == bindingcheck.False:
		l.log.Warn("binding verification: a condition is false", attrs...)
	case c.Status == bindingcheck.Unknown && c.Reason == bindingcheck.ReasonChatCredentialsChanged:
		l.log.Warn("binding verification: a condition is unknown", attrs...)
	case c.Status == bindingcheck.Unknown:
		l.log.Info("binding verification: a condition is unknown", attrs...)
	case c.Status == bindingcheck.True && tr.Old.Status != "" && tr.Old.Status != bindingcheck.True && tr.Old.Reason != bindingcheck.ReasonPending:
		l.log.Info("binding verification: a condition is true again", append(attrs, "was", string(tr.Old.Reason))...)
	}
}

// probeError is bindingcheck.Options.OnProbeError: the line for a probe whose checker could not tell, which leaves the condition
// Unknown/ProbeFailed. The error is the checker's own and may quote a response, so it goes in as telemetry.LogText. A Binding the
// config no longer has is not a failure and has no line: the Prober prunes it at its next Tick.
func (l bindingLog) probeError(pe bindingcheck.ProbeError) {
	if errors.Is(pe.Err, session.ErrUnknownBinding) {
		return
	}
	l.log.Warn("binding verification: a probe failed", "binding", pe.Target.Binding, "condition", string(pe.Condition), "error", telemetry.LogText(pe.Err))
}

// verification is what the verification of Bindings is built from.
type verification struct {
	// Bindings fetches agent cards (Bindings.CheckAgent), Platform reads the channel as the bot of Conn, and Snaps and Revision
	// are the config (Reloader.Current and Reloader.Revision).
	Bindings agentProbe
	Platform channelReader
	Snaps    Snapshots
	Revision func(context.Context) string
	// Conn is the ChatConnection this process serves: the Bindings on any other are not probed, because Platform is Conn's bot. Its
	// Credentials are the tokens Platform was built with, which Secrets is read against to tell a changed Secret.
	Conn    Connection
	Secrets identity.SecretReader // nil: a change of the chat credentials is not told
	Metrics *Metrics
	Log     *slog.Logger
	// Timeout is how long the Prober gives one probe (bindingProbeTimeout if zero). The adapters bound their own calls by innerBound
	// of it, so a backend or chat platform that does not answer is Unreachable or ChatUnavailable and not ProbeTimeout.
	Timeout time.Duration
	// Interval and Tick are the Prober's. Zero takes its defaults (5 minutes, 5 seconds); a test passes less.
	Interval, Tick time.Duration
}

// credentialsSecret is where the connection's credentials are in the current config: the Secret its ChatConnection names, in its
// namespace. A reload that names another Secret is a change the connection does not see until a restart.
func (v verification) credentialsSecret() (namespace, name string, ok bool) {
	snap := v.Snaps.Current()
	if snap == nil {
		return "", "", false
	}
	for _, c := range snap.ChatConnections() {
		if c.Namespace == v.Conn.Ref.Namespace && c.Name == v.Conn.Ref.Name {
			return c.Namespace, c.Spec.CredentialsSecretRef.Name, true
		}
	}
	return "", "", false
}

// newProber returns the Prober of the verification: its State is the conditions of the Bindings, which the hooks report as
// metrics and log lines and nothing else, and it writes nothing else. It is the leader half's to run and to stop (leaderHalf.start).
func (v verification) newProber() *bindingcheck.Prober {
	timeout := cmp.Or(v.Timeout, bindingProbeTimeout)
	probed := func(cond bindingcheck.ConditionType) func(time.Duration) {
		return func(d time.Duration) { v.Metrics.BindingProbed(cond, d) }
	}
	l := bindingLog{log: v.Log}
	return bindingcheck.NewProber(bindingcheck.Options{
		Targets:  func() []bindingcheck.Target { return bindingTargets(v.Snaps.Current(), v.Conn.Ref) },
		Revision: v.Revision,
		Agents:   agentChecker{bindings: v.Bindings, timeout: innerBound(timeout), observe: probed(bindingcheck.BackendReachable)},
		Channels: channelChecker{
			platform: v.Platform, timeout: innerBound(timeout), observe: probed(bindingcheck.ChannelJoined),
			creds: credentialsWatch{baseline: v.Conn.Credentials, secrets: v.Secrets, where: v.credentialsSecret},
		},
		Interval: v.Interval, Tick: v.Tick, Timeout: timeout,
		OnTransition: func(tr bindingcheck.Transition) { v.Metrics.BindingCondition(tr); l.transition(tr) },
		OnProbeError: l.probeError,
		OnPrune:      v.Metrics.BindingsPruned,
	})
}
