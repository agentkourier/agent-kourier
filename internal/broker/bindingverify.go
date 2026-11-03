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
	"net/url"
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

// minRevisionGap is the least time between a pass and one that a change of the config or of a Secret calls for. A change takes
// effect for the probes at once, but the revision also moves for writes the probes cannot see (a Secret's annotation, a controller
// that labels Secrets), and each pass probes every Binding.
const minRevisionGap = 30 * time.Second

// innerBound is the bound of an adapter's own call, given the Prober's timeout: four fifths of it, 8s in production. For the chat
// call it is the bound that takes effect: the Slack client puts its own (interact.PressAuthzTimeout, 10s) on the call underneath,
// and that one is larger, so it is reached only if the press bound is lowered below this one. The probe's bound is its own and
// does not follow the press bound.
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
}

var _ bindingcheck.AgentChecker = agentChecker{}

// CheckAgent implements bindingcheck.AgentChecker. A check the Prober's context ended is no verdict on the backend and comes
// back as an error, as does an error classifyAgent cannot place; the deadline of the checker's own bound is a backend that does
// not answer, Unreachable. (The Secrets are read from a cache that ignores its context, so a bound cannot cut a credentials read short;
// a token source that mints a token over the network would have to revisit that.) A Binding the config no longer has (a reload removed it during the pass) is no result either: the
// error says so and the Prober prunes the Binding at its next Tick.
func (a agentChecker) CheckAgent(ctx context.Context, binding string) (bindingcheck.Outcome, error) {
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	err := a.bindings.CheckAgent(cctx, binding)
	if err == nil {
		return bindingcheck.Outcome{Status: bindingcheck.True, Reason: bindingcheck.ReasonOK}, nil
	}
	if ctx.Err() != nil {
		return bindingcheck.Outcome{}, fmt.Errorf("the probe ended before the backend answered: %w", err)
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
// the class and a status code, and never the text of a status error or a card, which can quote a response. A
// request that failed carries its cause too, as telemetry.LogText gives the error under the *url.Error (so not the URL): that is
// what tells a name that does not resolve from a refused connection or a certificate that is not trusted, and the Message claims no more
// than that the request failed (a server that answers plain HTTP to an https request is a cause, not a silence), unless a deadline
// passed, when it says the backend did not answer in time. It reports false for an error it cannot place,
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
	case requestFailed(err):
		if timedOut(err) {
			return fail(bindingcheck.ReasonUnreachable, "the backend did not answer in time")
		}
		return fail(bindingcheck.ReasonUnreachable, "the request for the agent card failed: "+causeText(err))
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

// requestFailed reports whether err is the failure of the request for the card itself: a dial, DNS, TLS or read that failed, or a
// deadline that passed. A net.Error covers the *url.Error of the HTTP client, a *net.OpError, a *net.DNSError and
// context.DeadlineExceeded; the two TLS errors and an end of the stream are not net.Errors. (A scheme that is not http never gets this
// far: the API's validation and the dialect refuse the URL first.) A status the backend answered with is found before this is asked,
// since the HTTP client wraps that in a *url.Error too.
func requestFailed(err error) bool {
	var (
		netErr net.Error
		cert   *tls.CertificateVerificationError
		record tls.RecordHeaderError
	)
	return errors.As(err, &netErr) || errors.As(err, &cert) || errors.As(err, &record) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// timedOut reports whether err is a deadline that passed: a context deadline, or a network error that says it timed out.
func timedOut(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}

// causeText is the text of why a request failed, as a Message may carry it: the error the HTTP client wrapped, not the
// *url.Error around it, whose text is the request's URL, and through telemetry.LogText, which redacts and cuts it. It is what tells
// a name that does not resolve, a refused connection and a certificate that is not trusted apart.
func causeText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return telemetry.LogText(err)
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
}

var _ bindingcheck.ChannelChecker = channelChecker{}

// CheckChannel implements bindingcheck.ChannelChecker. It makes no call when the connection's credentials Secret differs in any key
// from the tokens the broker is using (ChatCredentialsChanged, or ChatCredentialsUnusable if it is gone or empty): the connection is
// then not running on what the Secret holds, so the read is withheld on any difference, even in a key the call does not use (the app
// token), and what the bot sees is not offered as a finding about the channel. A call the Prober's context ended is no verdict and comes back as an error; the
// deadline of the checker's own bound is a chat platform that does not answer, Unknown/ChatUnavailable.
func (c channelChecker) CheckChannel(ctx context.Context, channel string) (bindingcheck.Outcome, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	switch c.creds.check(cctx) {
	case credentialsChanged:
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatCredentialsChanged,
			Message: "the chat credentials changed after the connection started: restart Agent Kourier to use the new ones"}, nil
	case credentialsUnusable:
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatCredentialsUnusable,
			Message: "the chat credentials Secret, or a key of it, is gone or empty, or the Secret is of a type that is never read: the connection still uses the tokens it read, and a restart fails until the Secret is mended"}, nil
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
// rejects is False/ChatCredentialsRejected, and anything else is Unknown/ChatUnavailable. Its Message is a rate limit, a deadline that
// passed ("did not answer in time"), or, for everything else, "the chat platform read failed: " and the cause through telemetry.LogText and no claim
// about whether the platform answered: the Slack client's scrub keeps a deadline and a cancel as causes but drops other transport
// causes, so a refused connection and an answer that is an error cannot be told apart here, and the cause in the text says which it
// was (a code such as no_permission, or connection refused). A Message that changes is not a transition, so a cause that changes
// between passes adds no log line.
func (c channelChecker) classifyError(err error) bindingcheck.Outcome {
	switch {
	case errors.Is(err, chat.ErrChannelNotFound):
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonChannelNotFound,
			Message: "the channel does not exist, or the bot cannot see it (a private channel the bot is not in looks like this)"}
	case errors.Is(err, chat.ErrMissingScope):
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonMissingScope, Message: "the app lacks the permission to read the channel"}
	}
	unavailable := func(msg string) bindingcheck.Outcome {
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatUnavailable, Message: msg}
	}
	v := c.platform.Classify(chat.OpOther, err)
	switch {
	case v.Class == chat.FatalAuth:
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonChatCredentialsRejected,
			Message: "the chat platform rejected the bot's token: replace it and restart Agent Kourier"}
	case v.RetryAfter > 0:
		return unavailable("the chat platform rate-limited the read")
	case errors.Is(err, context.DeadlineExceeded):
		return unavailable("the chat platform did not answer the read in time")
	}
	return unavailable("the chat platform read failed: " + telemetry.LogText(err))
}

// credentialsChange is what credentialsWatch found.
type credentialsChange int

const (
	credentialsSame     credentialsChange = iota // the tokens are the ones in use, or the check could not tell
	credentialsChanged                           // the Secret holds other tokens than the connection is using
	credentialsUnusable                          // the Secret, or one of the keys, is gone, empty or of a type that is never read
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

// check compares the tokens in the Secret now with the baseline. A Secret that does not exist, a key that is missing or empty, and a
// Secret of a type that is never read are credentialsUnusable: they are the reads that ChooseConnection fails on for good (a restart
// would fail the same way), where a restart is what a token that differs (credentialsChanged) wants. Any other failure to read a key
// (a cache that has not synced, say, which a restart does not mend either, but which passes) and no baseline mean it cannot tell for
// that key, which is no change.
func (w credentialsWatch) check(ctx context.Context) credentialsChange {
	if w.secrets == nil || w.where == nil || len(w.baseline) == 0 {
		return credentialsSame
	}
	namespace, name, ok := w.where()
	if !ok {
		return credentialsSame
	}
	found := credentialsSame
	for key, was := range w.baseline { // in any order: the worst finding wins (max, and unusable is the largest)
		now, err := w.secrets.Value(ctx, namespace, name, key)
		switch {
		case err == nil && !now.IsEmpty():
			if subtle.ConstantTimeCompare([]byte(now.Reveal()), []byte(was.Reveal())) != 1 {
				found = max(found, credentialsChanged)
			}
		case err == nil, errors.Is(err, secret.ErrKeyNotFound), errors.Is(err, secret.ErrEmptyValue), errors.Is(err, secret.ErrUnsupportedType),
			errors.Is(err, config.ErrSecretNotFound):
			found = credentialsUnusable
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

// bindingLog is the log of the verification of Bindings: one line when a condition changes to False, Unknown or True again, one for a
// Binding that stops being verified, and one for each probe whose checker could not tell. A probe that finds what it found before adds
// no line of the first kinds; the last is warned of at every probe, since the condition stays Unknown/ProbeFailed with no change to report.
type bindingLog struct{ log *slog.Logger }

// transition is bindingcheck.Options.OnTransition, with a line for every change of a condition to a status and reason that
// has not been told:
//   - False is a WARN, and so are Unknown/ChatCredentialsChanged, which asks for a restart, and Unknown/ChatCredentialsUnusable,
//     which asks for the Secret to be mended;
//   - any other Unknown is an INFO, a Binding's first result included;
//   - True after a condition that was not True (and was not Pending) is an INFO.
//
// Nothing is logged for Ready, which holds back what the line for the condition that causes it says, and for Unknown/ProbeFailed,
// which probeError logs with the error that caused it. The Prober reports no transition for the Pending a Binding's probed conditions
// are stored with when it is first seen, so each probed condition's transition here has an Old condition and a result for New. A Ready
// can move from one Pending reason to another (the backend probe ends first, and Ready is Pending on the channel instead): a consumer of
// transitions that reads Ready tells that with Condition.Pending, and this one never logs Ready.
func (l bindingLog) transition(tr bindingcheck.Transition) {
	c := tr.New
	if c.Type == bindingcheck.Ready || c.Reason == bindingcheck.ReasonProbeFailed {
		return
	}
	attrs := []any{"binding", tr.Binding, "condition", string(c.Type), "status", string(c.Status), "reason", string(c.Reason), "detail", telemetry.LogText(c.Message)}
	switch {
	case c.Status == bindingcheck.False:
		l.log.Warn("binding verification: a condition is false", attrs...)
	case c.Status == bindingcheck.Unknown && (c.Reason == bindingcheck.ReasonChatCredentialsChanged || c.Reason == bindingcheck.ReasonChatCredentialsUnusable):
		l.log.Warn("binding verification: a condition is unknown", attrs...)
	case c.Status == bindingcheck.Unknown:
		l.log.Info("binding verification: a condition is unknown", attrs...)
	case c.Status == bindingcheck.True && tr.Old.Status != bindingcheck.True && tr.Old.Reason != bindingcheck.ReasonPending:
		l.log.Info("binding verification: a condition is now true", append(attrs, "was", string(tr.Old.Reason))...)
	}
}

// probeError is bindingcheck.Options.OnProbeError: the line for a probe whose checker could not tell, which leaves the condition
// Unknown/ProbeFailed. The error is the checker's own and may quote a response, so it goes in as telemetry.LogText. A Binding the
// config no longer has is not a failure and has no line: the Prober prunes it at its next Tick. A failure with no Binding and no
// condition is not a probe's but the Prober's own (Targets, Revision or OnPrune failed or panicked), and has a line of its own. A
// panic's stack is logged too, cut and redacted as LogText does.
func (l bindingLog) probeError(pe bindingcheck.ProbeError) {
	if errors.Is(pe.Err, session.ErrUnknownBinding) {
		return
	}
	attrs := []any{"error", telemetry.LogText(pe.Err)}
	var panicked *bindingcheck.PanicError
	if errors.As(pe.Err, &panicked) {
		attrs = append(attrs, "stack", telemetry.LogText(string(panicked.Stack)))
	}
	if pe.Target.Binding == "" && pe.Condition == "" {
		l.log.Warn("binding verification: the prober failed", attrs...)
		return
	}
	l.log.Warn("binding verification: a probe failed", append([]any{"binding", pe.Target.Binding, "condition", string(pe.Condition)}, attrs...)...)
}

// pruned is bindingcheck.Options.OnPrune: the line for each Binding the Prober stops verifying because it left the Targets, which is
// because a reload removed it or moved it to another ChatConnection than the one served. Its series go with it, so an alert on them
// would resolve without a word; this line is the word.
func (l bindingLog) pruned(names []string) {
	for _, name := range names {
		l.log.Info("binding verification: stopped verifying a Binding: the config removed it, or moved it to another ChatConnection than the one served", "binding", name)
	}
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
	// MinRevisionGap is the Prober's: the least time between a pass and one that only a change of the config or of a Secret calls
	// for. Zero takes minRevisionGap.
	MinRevisionGap time.Duration
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

// agentFunc and channelFunc are the checker interfaces made of functions.
type (
	agentFunc   func(ctx context.Context, binding string) (bindingcheck.Outcome, error)
	channelFunc func(ctx context.Context, channel string) (bindingcheck.Outcome, error)
)

// CheckAgent implements bindingcheck.AgentChecker.
func (f agentFunc) CheckAgent(ctx context.Context, binding string) (bindingcheck.Outcome, error) {
	return f(ctx, binding)
}

// CheckChannel implements bindingcheck.ChannelChecker.
func (f channelFunc) CheckChannel(ctx context.Context, channel string) (bindingcheck.Outcome, error) {
	return f(ctx, channel)
}

// timed runs a check and observes how long it took under cond, whatever it found.
func (v verification) timed(cond bindingcheck.ConditionType, check func() (bindingcheck.Outcome, error)) (bindingcheck.Outcome, error) {
	began := time.Now()
	defer func() { v.Metrics.BindingProbed(cond, time.Since(began)) }()
	return check()
}

// newProber returns the Prober of the verification: its State is the conditions of the Bindings, which the hooks report as
// metrics and log lines and nothing else, and it writes nothing else. It is the leader half's to run and to stop (leaderHalf.start).
// Revision changes call for a pass at most once every minRevisionGap.
func (v verification) newProber() *bindingcheck.Prober {
	timeout := cmp.Or(v.Timeout, bindingProbeTimeout)
	agents := agentChecker{bindings: v.Bindings, timeout: innerBound(timeout)}
	channels := channelChecker{
		platform: v.Platform, timeout: innerBound(timeout),
		creds: credentialsWatch{baseline: v.Conn.Credentials, secrets: v.Secrets, where: v.credentialsSecret},
	}
	l := bindingLog{log: v.Log}
	return bindingcheck.NewProber(bindingcheck.Options{
		Targets:  func() []bindingcheck.Target { return bindingTargets(v.Snaps.Current(), v.Conn.Ref) },
		Revision: v.Revision,
		Agents: agentFunc(func(ctx context.Context, binding string) (bindingcheck.Outcome, error) {
			return v.timed(bindingcheck.BackendReachable, func() (bindingcheck.Outcome, error) { return agents.CheckAgent(ctx, binding) })
		}),
		Channels: channelFunc(func(ctx context.Context, channel string) (bindingcheck.Outcome, error) {
			return v.timed(bindingcheck.ChannelJoined, func() (bindingcheck.Outcome, error) { return channels.CheckChannel(ctx, channel) })
		}),
		Interval: v.Interval, Tick: v.Tick, Timeout: timeout, MinRevisionGap: cmp.Or(v.MinRevisionGap, minRevisionGap),
		OnTransition: func(tr bindingcheck.Transition) { v.Metrics.BindingCondition(tr); l.transition(tr) },
		OnProbeError: l.probeError,
		OnPrune:      func(names []string) { v.Metrics.BindingsPruned(names); l.pruned(names) },
	})
}
