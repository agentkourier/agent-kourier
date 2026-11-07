package broker

import (
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
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

// The one sentence of a request or a read that failed, whatever the cause: it reaches the status of a Binding, which its owner reads
// and may not be allowed to read the broker's log, so it says only that the cause is there. It hides the cause and the address, and no
// more: a status still tells whether the backend answered (and with which status code), whether it did not answer in time, and whether
// the request failed some other way. It names no further class of cause (a name that does not resolve, a refused connection, a
// certificate that is not trusted), which would let a tenant tell more of which internal hosts answer.
const (
	msgCardRequestFailed = "the request for the agent card failed; the operator's log has the cause"
	msgChatReadFailed    = "the chat platform read failed; the operator's log has the cause"
	// msgCardTooLarge is what a card past dialect.MaxCardBytes is reported as: the bound is Agent Kourier's, not the backend's, so
	// it is the one number a status may carry.
	msgCardTooLarge = "the agent card is larger than 1 MiB, which Agent Kourier does not read; the operator's log has the cause"
)

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
// a token source that mints a token over the network would have to revisit that.) A Binding the config no longer has (a reload removed it
// during the pass) comes back as an error too, so until the pass ends it reads Unknown/ProbeFailed with no line of its own (probeError), and
// the Prober prunes it at the first Tick after the pass.
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
// the class and a status code, and never the text of a status error or a card, which can quote a response. The exception is
// CredentialsUnreadable (credentialsOutcome): one sentence for each cause, which may name a Secret, a key or a header of the Binding's own
// namespace and nothing of another. A
// request that failed has its cause in Outcome.Cause, for the log only, as telemetry.LogText gives the error under the *url.Error (so
// not the URL): that is what tells a name that does not resolve from a refused connection or a certificate that is not trusted. The
// Message is one sentence, whatever the cause, because it reaches the Binding's status, which its owner reads, and a transport error names
// hosts of other namespaces, the broker's own address and an ephemeral port that a pass later would change. It claims no more
// than that the request failed (a server that answers plain HTTP to an https request is a cause, not a silence), unless a deadline
// passed, when it says the backend did not answer in time. A deadline is False although a turn's connect waits longer (the pool's
// ConnectTimeout, 30s, against the probe's 8s): a backend that never answers, as behind a firewall, must not stay Unknown, so one that
// is only slower than the probe's bound reads False too. A card past dialect.MaxCardBytes is InvalidAgentCard, with one sentence that names no
// host (msgCardTooLarge). It reports false for an error it cannot place, which the Prober records as a
// probe that could not tell.
func classifyAgent(err error) (bindingcheck.Outcome, bool) {
	fail := func(r bindingcheck.Reason, msg string) (bindingcheck.Outcome, bool) {
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: r, Message: msg}, true
	}
	var status *dialect.HTTPStatusError
	var notOK *agentcard.ErrStatusNotOK
	switch {
	case errors.Is(err, agent.ErrTokenUnreadable), errors.Is(err, agent.ErrHeaderUnreadable), errors.Is(err, agent.ErrTokenRequired):
		return credentialsOutcome(err), true
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
	case errors.Is(err, dialect.ErrResponseTooLarge): // before requestFailed: the client's *url.Error around it is a net.Error
		out, ok := fail(bindingcheck.ReasonInvalidAgentCard, msgCardTooLarge)
		out.Cause = causeText(err)
		return out, ok
	case requestFailed(err):
		if timedOut(err) {
			return fail(bindingcheck.ReasonUnreachable, "the backend did not answer in time")
		}
		out, ok := fail(bindingcheck.ReasonUnreachable, msgCardRequestFailed)
		out.Cause = causeText(err)
		return out, ok
	}
	return bindingcheck.Outcome{}, false
}

// credentialsOutcome is the BackendReachable outcome for a token or a header Secret that cannot be read, or a token the Binding requires
// and does not have: False/CredentialsUnreadable, with a Message that says which, so that a team can mend it without asking whoever runs
// Agent Kourier. The message names the Secret, the key and what is wrong with it (credentialsProblem) when the Secret is in the Binding's own
// namespace: the token's always is, a header's is when the AgentBackend is in the Binding's namespace. A header's Secret in another
// namespace is not named, nor is what is wrong with it: a status says nothing of what another namespace holds (spec D50). An
// error that carries no Secret (a token source that is not the Secret's) and one that has no cause credentialsProblem knows get a
// sentence that names none. No message has the text of the error, which the log has, redacted, as the Cause.
func credentialsOutcome(err error) bindingcheck.Outcome {
	out := bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonCredentialsUnreadable}
	var ref *identity.SecretError
	switch {
	case errors.Is(err, agent.ErrTokenRequired):
		out.Message = "the Binding requires a bearer token and none was set"
		return out
	case errors.As(err, &ref) && ref.Namespace == ref.BindingNamespace:
		subject := fmt.Sprintf("the token Secret %q", ref.Name)
		if ref.Header != "" {
			subject = fmt.Sprintf("the Secret %q behind header %q of the AgentBackend", ref.Name, ref.Header)
		}
		out.Message = credentialsProblem(err, subject, ref.Key, ref.Namespace)
	case errors.Is(err, agent.ErrHeaderUnreadable):
		out.Message = "a Secret behind a header of the AgentBackend cannot be read; the operator's log has the cause"
	default:
		out.Message = "the Binding's token cannot be read; the operator's log has the cause"
	}
	out.Cause = causeText(err)
	return out
}

// credentialsProblem says what is wrong with the key of a Secret that subject names, in namespace, from the sentinel the read wrapped. A
// denial that the API server answered forbidden says what grant mends it. ErrAccessDenied also covers unauthorized (the broker's own token
// expired, or the authorizer is down), which no grant mends, so that and every cause it cannot tell (a cache not yet synced, an error
// nobody placed) say only that the read failed. It is never the error's text.
func credentialsProblem(err error, subject, key, namespace string) string {
	switch {
	case errors.Is(err, k8ssecrets.ErrAccessDenied) && apierrors.IsForbidden(err):
		return fmt.Sprintf("%s cannot be read: the API server denied Agent Kourier's ServiceAccount access to it; it needs list and watch on it in namespace %s", subject, namespace)
	case errors.Is(err, config.ErrSecretNotFound):
		return fmt.Sprintf("%s does not exist in namespace %s", subject, namespace)
	case errors.Is(err, secret.ErrKeyNotFound):
		return fmt.Sprintf("%s has no key %q", subject, key)
	case errors.Is(err, secret.ErrEmptyValue):
		return fmt.Sprintf("key %q of %s is empty", key, subject)
	case errors.Is(err, identity.ErrControlCharacter):
		return fmt.Sprintf("key %q of %s holds a control character, such as the trailing newline that kubectl create secret --from-file keeps", key, subject)
	case errors.Is(err, secret.ErrUnsupportedType):
		return subject + " is not of type Opaque, the only type Agent Kourier reads"
	}
	return subject + " cannot be read now; the operator's log has the cause"
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
// deadline that passed. A net.Error covers the *url.Error of the HTTP client, which holds a TLS failure too, a *net.OpError, a
// *net.DNSError and context.DeadlineExceeded; an end of the stream is not a net.Error. (A scheme that is not http never gets this
// far: the API's validation and the dialect refuse the URL first.) A status the backend answered with is found before this is asked,
// since the HTTP client wraps that in a *url.Error too.
func requestFailed(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// timedOut reports whether err is a deadline that passed: a context deadline, or a network error that says it timed out.
func timedOut(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}

// causeText is the text of why a request failed, as an Outcome's Cause carries it: the error the HTTP client wrapped, not the
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
	if out, held := c.creds.hold(cctx); held {
		return out, nil
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
// passed ("did not answer in time"), or, for everything else, "the chat platform read failed" with the cause through telemetry.LogText in
// Outcome.Cause, for the log, and no claim about whether the platform answered: the Slack client's scrub keeps a deadline and a cancel as
// causes but drops other transport causes, so a refused connection and an answer that is an error cannot be told apart here, and the cause
// says which it was (a code such as no_permission, or connection refused). The Message is one sentence because it reaches a status. A
// Cause that changes is not a transition, so a cause that changes between passes adds no log line.
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
	out := unavailable(msgChatReadFailed)
	out.Cause = telemetry.LogText(err)
	return out
}

// groupReader is what the group checker needs of the chat platform: chat.Platform's GroupsReadable and Classify.
type groupReader interface {
	GroupsReadable(ctx context.Context, groups []string) error
	Classify(op chat.Op, err error) chat.Verdict
}

// platformReader is what the verification needs of the chat platform: both reads, so that every Binding the writer expects an
// ApproversReadable for has a prober that tracks it.
type platformReader interface {
	channelReader
	groupReader
}

// groupChecker is the bindingcheck.GroupChecker of a running broker. Like channelChecker it answers for the bot of the connection the
// broker serves, so the Targets it is given hold only Bindings on that connection (bindingTargets). The Prober makes one for each pass
// (ForPass), whose probes share their reads of a group.
type groupChecker struct {
	platform groupReader
	timeout  time.Duration
	creds    credentialsWatch
	reads    *groupReads // the reads of the pass, shared by its probes; nil: each probe reads its groups itself
}

var _ bindingcheck.PassGroupChecker = groupChecker{}

// ForPass implements bindingcheck.PassGroupChecker with forPass.
func (c groupChecker) ForPass() bindingcheck.GroupChecker { return c.forPass() }

// forPass returns the checker of a pass, which reads each group once for all the pass's probes.
func (c groupChecker) forPass() groupChecker {
	c.reads = &groupReads{reads: map[string]*groupRead{}}
	return c
}

// CheckGroups implements bindingcheck.GroupChecker. It makes no call when the connection's credentials differ from the tokens the broker
// is using, for the reason given at CheckChannel (credentialsWatch.hold). A call the Prober's context ended is no verdict and comes back as
// an error; the deadline of the checker's own bound is a chat platform that does not answer, Unknown/ChatUnavailable. One call reads all
// of a Binding's groups, at most 10 (the API's validation caps them), one request to the platform each, in the Binding's order, and stops at
// the first it cannot read, so a second bad group is told once the first is mended. In a pass, a group another probe has read or is
// reading is not read again (groupReads.readable), so the reads of a pass are at most one for each group its Bindings name.
func (c groupChecker) CheckGroups(ctx context.Context, groups []string) (bindingcheck.Outcome, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if out, held := c.creds.hold(cctx); held {
		return out, nil
	}
	var err error
	for _, g := range groups {
		if err = c.reads.readable(cctx, c.platform, g); err != nil {
			break
		}
	}
	if err == nil {
		return bindingcheck.Outcome{Status: bindingcheck.True, Reason: bindingcheck.ReasonOK}, nil
	}
	if ctx.Err() != nil {
		return bindingcheck.Outcome{}, fmt.Errorf("the probe ended before the chat platform answered: %w", err)
	}
	return c.classifyError(groups, err), nil
}

// groupReads is what the probes of one pass have read of approver groups, by group ID, so that a group several Bindings name is read
// once in the pass. Only the platform's answers are kept: a read that its own probe's bound or the Prober's context cut short says nothing
// of the group, and is forgotten.
type groupReads struct {
	mu    sync.Mutex
	reads map[string]*groupRead
}

// groupRead is one read of a group: done is closed when it has ended, and then err is what the platform answered, unless cut is set.
type groupRead struct {
	done chan struct{}
	err  error
	cut  bool // the reader's context ended the read, which is no answer: a probe that waited for it reads the group itself
}

// readable reads group on platform, as GroupsReadable of that group alone, or is told what another probe of the pass read of it. A probe
// that finds a read under way waits for it, but no longer than its own ctx: when ctx ends first, the error is one of a platform that did
// not answer in time, as its own read's would be. A nil r reads every time.
func (r *groupReads) readable(ctx context.Context, platform groupReader, group string) error {
	if r == nil {
		return platform.GroupsReadable(ctx, []string{group})
	}
	for {
		r.mu.Lock()
		read, found := r.reads[group]
		if !found {
			read = &groupRead{done: make(chan struct{})}
			r.reads[group] = read
		}
		r.mu.Unlock()
		if !found {
			return r.read(ctx, platform, group, read)
		}
		select {
		case <-read.done:
			if !read.cut {
				return read.err
			}
		case <-ctx.Done():
			return &chat.GroupError{Group: group, Err: fmt.Errorf("waiting for another probe's read of the group: %w", ctx.Err())}
		}
	}
}

// read makes the read of group that readable registered as read, and ends it for the probes waiting on it. A read that ctx cut short, or
// one the platform panicked in (which the Prober recovers), is no answer: it is forgotten, and a probe that waited for it reads again.
func (r *groupReads) read(ctx context.Context, platform groupReader, group string, read *groupRead) error {
	read.cut = true // until the platform answers
	defer func() {
		if read.cut {
			r.mu.Lock()
			delete(r.reads, group)
			r.mu.Unlock()
		}
		close(read.done)
	}()
	read.err = platform.GroupsReadable(ctx, []string{group})
	read.cut = read.err != nil && ctx.Err() != nil
	return read.err
}

// classifyError says what an error of GroupsReadable means for ApproversReadable: a group that is not there, a missing scope, and
// any other refusal that no retry mends (a plan without user groups, a token of a type the read does not take) are False, a
// credential the platform rejects is False/ChatCredentialsRejected, and anything else is Unknown/ChatUnavailable, worded as
// channelChecker.classifyError words it. Which group failed is told by its place in the Binding's own list and its ID, which are the
// Binding's words and no one else's: an error that names a group the Binding does not have, which the platform does not do, names none.
// The platform's own words are in the Cause, for the log.
func (c groupChecker) classifyError(groups []string, err error) bindingcheck.Outcome {
	which := "an approver group"
	var ge *chat.GroupError
	if errors.As(err, &ge) {
		if i := slices.Index(groups, ge.Group); i >= 0 {
			which = fmt.Sprintf("approver group spec.interactions.approverGroups[%d] (%s)", i, groups[i])
		}
	}
	switch {
	case errors.Is(err, chat.ErrGroupNotFound):
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonGroupNotFound,
			Message: which + " does not exist, or is disabled, or the bot cannot see it"}
	case errors.Is(err, chat.ErrMissingScope):
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonMissingScope, Message: "the app lacks the permission to read user groups"}
	}
	unavailable := func(msg string) bindingcheck.Outcome {
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatUnavailable, Message: msg}
	}
	v := c.platform.Classify(chat.OpOther, err)
	switch {
	case v.Class == chat.FatalAuth:
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonChatCredentialsRejected,
			Message: "the chat platform rejected the bot's token: replace it and restart Agent Kourier"}
	case errors.Is(err, chat.ErrGroupsMisconfigured):
		return bindingcheck.Outcome{Status: bindingcheck.False, Reason: bindingcheck.ReasonGroupsUnreadable,
			Message: "the chat platform refuses to let the bot read " + which + ": the workspace may have no user groups, or the app's token may not be allowed to read them; the operator's log has the cause",
			Cause:   telemetry.LogText(err)}
	case v.RetryAfter > 0:
		return unavailable("the chat platform rate-limited the read")
	case errors.Is(err, context.DeadlineExceeded):
		return unavailable("the chat platform did not answer the read in time")
	}
	out := unavailable(msgChatReadFailed)
	out.Cause = telemetry.LogText(err)
	return out
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

// hold is the outcome of a probe that the credentials hold back, and whether they do: Unknown/ChatCredentialsChanged when the Secret holds
// other tokens than the connection is using, and Unknown/ChatCredentialsUnusable when it, or a key of it, is gone, empty or of a type that is
// never read. The probe then makes no call, because what a bot the Secret no longer names sees is not a finding about the channel or the
// groups. Both checkers that read with the connection's bot use it, so they word it alike.
func (w credentialsWatch) hold(ctx context.Context) (bindingcheck.Outcome, bool) {
	switch w.check(ctx) {
	case credentialsChanged:
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatCredentialsChanged,
			Message: "the chat credentials changed after the connection started: restart Agent Kourier to use the new ones"}, true
	case credentialsUnusable:
		return bindingcheck.Outcome{Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonChatCredentialsUnusable,
			Message: "the chat credentials Secret, or a key of it, is gone or empty, or the Secret is of a type that is never read: the connection still uses the tokens it read, and a restart fails until the Secret is mended"}, true
	}
	return bindingcheck.Outcome{}, false
}

// asksForApprovers reports whether a Binding has an ApproversReadable condition: it asks for tool approvals, and from user groups. A
// Binding that asks for approvals from anyone in the channel has no group to read, and the API refuses groups without approvals.
func asksForApprovers(spec v1alpha1.BindingSpec) bool {
	return spec.Interactions.ToolApprovals && len(spec.Interactions.ApproverGroups) > 0
}

// bindingTargets lists the Bindings to probe: those of snap whose ChatConnection is conn, the one this process serves. The
// channel and group checkers read with that connection's bot, so a Binding on any other would be told the wrong bot's answer. A
// Binding's key is namespace/name, which is what CheckAgent looks it up by. A Binding that asks for tool approvals from user groups
// (asksForApprovers) carries them in its Target, a copy of its own list.
func bindingTargets(snap *config.Snapshot, conn v1alpha1.ObjectRef) []bindingcheck.Target {
	if snap == nil {
		return nil
	}
	var out []bindingcheck.Target
	for _, rb := range snap.Bindings() {
		if rb.Connection.Namespace != conn.Namespace || rb.Connection.Name != conn.Name {
			continue
		}
		t := bindingcheck.Target{
			Binding:    v1alpha1.NamespacedName(rb.Binding.Namespace, rb.Binding.Name),
			Channel:    rb.Binding.Spec.Chat.Channel,
			Generation: rb.Binding.Generation,
			UID:        string(rb.Binding.UID),
		}
		if asksForApprovers(rb.Binding.Spec) {
			t.ApproverGroups = slices.Clone(rb.Binding.Spec.Interactions.ApproverGroups)
		}
		out = append(out, t)
	}
	return out
}

// bindingLog is the log of the verification of Bindings: one line when a condition changes to False or Unknown, or to True after a result that was not True, one for a
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
	if c.Cause != "" { // what the Message leaves out of the status: the line is for an operator
		attrs = append(attrs, "cause", telemetry.LogText(c.Cause))
	}
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
// config no longer has is not a failure and has no line: the Prober prunes it at the first Tick after the pass. A failure with no Binding and no
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

// dropped is bindingcheck.Options.OnDrop: the line for a condition the Prober stops verifying because the Binding no longer asks for it (an
// edit took its approver groups away). Its series go with it, so an alert on them would resolve without a word; this line is the word.
func (l bindingLog) dropped(binding string, cond bindingcheck.ConditionType) {
	l.log.Info("binding verification: stopped verifying a condition of a Binding: the Binding no longer asks for it", "binding", binding, "condition", string(cond))
}

// verification is what the verification of Bindings is built from.
type verification struct {
	// Bindings fetches agent cards (Bindings.CheckAgent), Platform reads the channel and the approver groups as the bot of Conn, and Snaps
	// and Revision are the config (Reloader.Current and Reloader.Revision).
	Bindings agentProbe
	Platform platformReader
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
	// Status, when set, is told that something moved: each transition, and the end of each pass, which moves the conditions'
	// observed generation although nothing transitions. It is set with the crds config source and nil with files, which the
	// verification then serves exactly as it did.
	Status *statusWriter
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

// timedGroups is a groupChecker whose probes the verification times under ApproversReadable, its passes' included.
type timedGroups struct {
	v verification
	c groupChecker
}

// CheckGroups implements bindingcheck.GroupChecker.
func (t timedGroups) CheckGroups(ctx context.Context, groups []string) (bindingcheck.Outcome, error) {
	return t.v.timed(bindingcheck.ApproversReadable, func() (bindingcheck.Outcome, error) { return t.c.CheckGroups(ctx, groups) })
}

// ForPass implements bindingcheck.PassGroupChecker.
func (t timedGroups) ForPass() bindingcheck.GroupChecker {
	return timedGroups{v: t.v, c: t.c.forPass()}
}

// timed runs a check and observes how long it took under cond, whatever it found.
func (v verification) timed(cond bindingcheck.ConditionType, check func() (bindingcheck.Outcome, error)) (bindingcheck.Outcome, error) {
	began := time.Now()
	defer func() { v.Metrics.BindingProbed(cond, time.Since(began)) }()
	return check()
}

// notifyStatus tells the status writer, if there is one, that the Prober's findings moved. It must be quick: it is a hook of the
// Prober, called with the results held up.
func (v verification) notifyStatus() {
	if v.Status != nil {
		v.Status.notify()
	}
}

// newProber returns the Prober of the verification: its State is the conditions of the Bindings, which the hooks report as
// metrics and log lines and, with the crds config source, tell the status writer of (Status), which reads the State and writes the
// status; the Prober writes nothing itself. It is the leader half's to run and to stop (leaderHalf.start).
// Revision changes call for a pass at most once every minRevisionGap.
func (v verification) newProber() *bindingcheck.Prober {
	timeout := cmp.Or(v.Timeout, bindingProbeTimeout)
	agents := agentChecker{bindings: v.Bindings, timeout: innerBound(timeout)}
	channels := channelChecker{
		platform: v.Platform, timeout: innerBound(timeout),
		creds: credentialsWatch{baseline: v.Conn.Credentials, secrets: v.Secrets, where: v.credentialsSecret},
	}
	groupsChecker := groupChecker{
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
		Groups:   timedGroups{v: v, c: groupsChecker},
		Interval: v.Interval, Tick: v.Tick, Timeout: timeout, MinRevisionGap: cmp.Or(v.MinRevisionGap, minRevisionGap),
		OnTransition: func(tr bindingcheck.Transition) { v.Metrics.BindingCondition(tr); l.transition(tr); v.notifyStatus() },
		OnDrop: func(binding string, cond bindingcheck.ConditionType) {
			v.Metrics.BindingConditionDropped(binding, cond)
			l.dropped(binding, cond)
			v.notifyStatus()
		},
		OnPass:       v.notifyStatus,
		OnProbeError: l.probeError,
		OnPrune:      func(names []string) { v.Metrics.BindingsPruned(names); l.pruned(names) },
	})
}
