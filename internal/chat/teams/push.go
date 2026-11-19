package teams

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/backoff"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// This file takes Microsoft Graph's change notifications of the bound channels' messages, the alert route of a Teams connection: Teams
// sends a bot no other bot's post, the cards that Workflows posts for an alert among them, even with the resource-specific permission
// (Microsoft's documentation, and measured on a test tenant on 2026-11-18), while Graph tells of every post. subscriptions.go makes the
// subscriptions; this answers what Graph sends to the connection's notificationURL and hands each new post on to the sink.
//
// Measured on a test tenant in November 2026: the validation handshake, a POST with ?validationToken= and an empty text/plain body, to the
// notification URL and to the lifecycle notification URL, at every create and every renew, answered with the token decoded, as text/plain;
// a notification of a channel's message names its resource and IDs only (includeResourceData false), so the message is read from Graph;
// a `created` comes for every post and reply, the bot's own among them, and an `updated` for every edit; a Workflows post is a message of
// the application 358f0194-6b0e-4dd3-af35-c24fe8a9ec87 whose body is only an <attachment> tag and whose card is a JSON string in its
// attachments, and whose attachment's ID changes between two reads; a system event has the messageType unknownFutureValue. Microsoft's
// documentation, not measured: a body that holds several notifications; the lifecycle notifications (reauthorizationRequired,
// subscriptionRemoved, missed) and that their URL may be the notification URL; that a notification not answered 2xx within 3 seconds is
// sent again, for up to 4 hours, and that a slow endpoint is sent notifications late or not at all; the resource's form
// teams('…')/channels('…')/messages('…') with /replies('…') for a reply.

// notificationMaxBody caps a body of Graph's notifications. A notification names IDs only, about half a KiB, and Microsoft documents no
// most that a body holds; a longer body is no batch of Graph's, and is answered 413.
const notificationMaxBody = 256 << 10

// notificationMaxItems caps the notifications of one body: a body with more is answered 400 and none of it is taken. Microsoft documents
// no most; a body of Graph's at notificationMaxBody holds about 500. Anyone may send a body, before any clientState is checked, so the
// cap bounds the work one request causes.
const notificationMaxItems = 1000

// A notified message is read with one GET, cut at fetchTimeout, on its channel's lane. A read that may pass on another try (a 429, a 5xx,
// a timeout) is made again, fetchAttempts times in all, after fetchRetry and twice as long after each further failure up to fetchRetryMax,
// or after a 429's Retry-After when that is longer, up to fetchRetryMax; the channel's later activities wait behind it. A read that fails
// for good is dropped and counted: Graph does not send a notification again once it was answered 202.
const (
	fetchTimeout  = 10 * time.Second
	fetchAttempts = 3
	fetchRetry    = time.Second
	fetchRetryMax = 4 * time.Second
)

// clientStateInfo is HKDF's info of the clientState of the connection's subscriptions: what it is for, so that no other use of the client
// secret derives it (the buttons' key is buttonKeyInfo's).
const clientStateInfo = "agent-kourier/teams/graph-client-state/v1"

// clientStateOf is the clientState of the subscriptions of the connection conn: HKDF-SHA256 of its client secret, under clientStateInfo
// and the connection, 32 bytes in unpadded base64url (43 characters; Graph takes up to 128). Every replica, and the broker after a
// restart, derives the same, which a notification is checked against: a listing of the subscriptions gives no clientState back (null,
// measured), so it cannot be read again from Graph. It is never logged.
func clientStateOf(clientSecret, conn string) (string, error) {
	key, err := hkdf.Key(sha256.New, []byte(clientSecret), nil, clientStateInfo+"\x00"+conn, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("teams: deriving the clientState of the Graph subscriptions: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}

// The kinds of what Graph sends to the route, a label of agentkourier_teams_graph_notifications_total.
const (
	notifyValidation = "validation" // the validation handshake of a create or a renew
	notifyChange     = "change"     // a change of a channel's messages
	notifyLifecycle  = "lifecycle"  // an event of a subscription's life
)

// The outcomes of what Graph sends, the other label of agentkourier_teams_graph_notifications_total: a closed set.
const (
	notifiedAnswered       = "answered"                 // validation: the token was given back
	notifiedTaken          = "taken"                    // change: a post handed to the sink
	notifiedNotTaken       = "not_taken"                // change: a post the sink did not take in its tries
	notifiedDuplicate      = "duplicate"                // change: a post that an activity or an earlier notification already handed on
	notifiedOwn            = "own"                      // change: a post of the connection's own bot
	notifiedIgnored        = "ignored"                  // change: no post: a system event, a deleted message, one with no sender or text
	notifiedEdit           = "edit"                     // change: an edit or a deletion, which no trigger reads
	notifiedNotNamed       = "not_named"                // change: another bot's post that no trigger of the channel names
	notifiedGone           = "gone"                     // change: the message was gone when it was read (404)
	notifiedFetchFailed    = "fetch_failed"             // change: the message could not be read
	notifiedMalformed      = "malformed"                // change: a resource or a change type the adapter cannot read
	notifiedClientState    = "client_state"             // the clientState is not the connection's
	notifiedWrongTenant    = "wrong_tenant"             // the tenant is not the connection's
	notifiedUnknownSub     = "unknown_subscription"     // a subscription the connection does not hold
	notifiedShed           = "shed"                     // change: its channel's lane, or all of them, held as many as they may (503)
	notifiedNotRunning     = "not_running"              // the connection was not taking them (503)
	notifiedReauthorize    = "reauthorization_required" // lifecycle: the subscription is renewed
	notifiedRemoved        = "subscription_removed"     // lifecycle: the subscription is made anew
	notifiedMissed         = "missed"                   // lifecycle: Graph did not deliver some notifications
	notifiedOtherLifecycle = "other"                    // lifecycle: an event the adapter does not know
)

// notification is one item of a body of Graph's notifications: a change of a channel's messages, or an event of a subscription's life.
// Every field is untrusted until clientState is checked.
type notification struct {
	SubscriptionID string `json:"subscriptionId"`
	ClientState    string `json:"clientState"`
	ChangeType     string `json:"changeType"`
	Resource       string `json:"resource"`
	TenantID       string `json:"tenantId"`
	LifecycleEvent string `json:"lifecycleEvent"`
}

// push is the Graph intake of a connection with a notificationURL: its subscriptions, and the route's answers to what Graph sends.
type push struct {
	c     *Client
	url   string // the connection's notificationURL, the subscriptions' notification and lifecycle URL
	state string // the clientState (clientStateOf); never logged
	subs  *subscriber
	// onMissed is Config.OnNotificationsMissed.
	onMissed func(channel string)
	// refusals paces the WARN lines of bodies that anyone may send: one too long, and one with notifications whose clientState is not
	// the connection's.
	refusals      refusalLog[string]
	fetchTimeout  time.Duration // fetchTimeout; a test shortens it
	fetchRetry    time.Duration // fetchRetry; a test shortens it
	fetchRetryMax time.Duration // fetchRetryMax; a test shortens it
}

// clientStateOK says whether got is the connection's clientState, in constant time.
func (p *push) clientStateOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(p.state)) == 1
}

// count counts one in agentkourier_teams_graph_notifications_total.
func (p *push) count(ctx context.Context, kind, outcome string) {
	telemetry.Count(ctx, "agentkourier_teams_graph_notifications_total", kind, outcome, p.c.conn)
}

// isNotificationBody says whether body is a body of Graph's notifications, {"value": [...]}, and not an activity, which has a type.
func isNotificationBody(body []byte) bool {
	var probe struct {
		Value json.RawMessage `json:"value"`
		Type  *string         `json:"type"`
	}
	if json.Unmarshal(body, &probe) != nil || probe.Type != nil {
		return false
	}
	v := bytes.TrimSpace(probe.Value)
	return len(v) > 0 && v[0] == '['
}

// validation is the token of Graph's validation handshake that req carries, decoded, and false when it carries none.
func validation(req *http.Request) (string, bool) {
	q, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil || !q.Has("validationToken") {
		return "", false
	}
	return q.Get("validationToken"), true
}

// serveGraph answers req if it is Graph's: the validation handshake (200 with the token decoded, as text/plain, which Graph sends to the
// notification URL and to the lifecycle URL at each create and renew, with nothing else to check), or a body of notifications (what
// takeNotifications says: 202, or 503 for Graph to send it again). A body past notificationMaxBody is 413. It says false for a request that
// is neither, whose req.Body it leaves as the same bytes, for the activity's handler.
func (p *push) serveGraph(w http.ResponseWriter, req *http.Request) bool {
	ctx := req.Context()
	if token, ok := validation(req); ok {
		p.count(p.c.recv.telemetryContext(ctx), notifyValidation, notifiedAnswered)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, token)
		return true
	}
	body, err := io.ReadAll(req.Body) // the chat listener has read it whole and capped it: this reads memory
	if err != nil {
		http.Error(w, "the body could not be read", http.StatusBadRequest)
		return true
	}
	if !isNotificationBody(body) {
		req.Body = io.NopCloser(bytes.NewReader(body))
		return false
	}
	if len(body) > notificationMaxBody {
		if ok, n := p.refusals.admit("too_long", p.c.recv.now()); ok {
			p.c.log.WarnContext(ctx, "teams: a body of Graph notifications was refused for its length; answered 413", "bytes", len(body),
				"alsoRefusedSinceLastLine", n)
		}
		http.Error(w, "the body is too long for Microsoft Graph's notifications", http.StatusRequestEntityTooLarge)
		return true
	}
	status, text := p.takeNotifications(ctx, body)
	if text != "" {
		http.Error(w, text, status)
		return true
	}
	w.WriteHeader(status)
	return true
}

// takeNotifications works on a body of Graph's notifications and says what to answer, each item as take says. The answer is 202 once
// every item was taken or dropped, and 503 when one could not be put on its channel's lane or the connection takes nothing now: Graph
// sends such a body again (Microsoft's documentation, not measured), and the items taken the first time are then duplicates.
func (p *push) takeNotifications(ctx context.Context, body []byte) (int, string) {
	r := p.c.recv
	tctx := r.telemetryContext(ctx)
	var batch struct {
		Value []notification `json:"value"`
	}
	if err := json.Unmarshal(body, &batch); err != nil {
		p.c.log.DebugContext(ctx, "teams: a body of Graph notifications could not be read; answered 400", "bytes", len(body))
		return http.StatusBadRequest, "not a body of Microsoft Graph's notifications"
	}
	if len(batch.Value) > notificationMaxItems {
		p.c.log.DebugContext(ctx, "teams: a body of Graph notifications was refused for its number of notifications; answered 400",
			"notifications", len(batch.Value))
		return http.StatusBadRequest, "too many notifications in one body"
	}
	s := r.live.Load()
	again, mismatched := false, 0
	for _, n := range batch.Value {
		kind, outcome, retry := p.take(ctx, s, n)
		again = again || retry
		if outcome == notifiedClientState {
			mismatched++
		}
		if outcome != "" {
			p.count(tctx, kind, outcome)
		}
	}
	if mismatched > 0 {
		if ok, n := p.refusals.admit(notifiedClientState, r.now()); ok {
			p.c.log.WarnContext(ctx, "teams: Graph notifications whose clientState is not the connection's were dropped", "count", mismatched,
				"bodiesAlsoDroppedSinceLastLine", n)
		}
	}
	if again {
		return http.StatusServiceUnavailable, "the connection cannot take the notifications now"
	}
	return http.StatusAccepted, ""
}

// take works on one notification, with s the live Serve or nil, and says its kind, its outcome if it has one now ("" for one put on its
// channel's lane, which intake counts), and whether Graph is to send it again. The clientState is checked first, in constant time: a
// notification whose clientState is not the connection's is dropped, and if it names a subscription the connection holds, that subscription
// is made anew (a subscription adopted from before a change of the client secret has the old clientState, which cannot be read back).
// Then its tenant. While the connection does not run, or its subscriptions are being listed still at the start of a run, Graph is to send
// it again (503); then its subscription must be one the connection holds. A lifecycle event goes to the subscriptions (lifecycle);
// a `created` whose resource is a message of the subscription's channel is put on that channel's lane (intake); an `updated` or a
// `deleted` is dropped, since a trigger reads new posts.
func (p *push) take(ctx context.Context, s *serving, n notification) (kind, outcome string, retry bool) {
	kind = notifyChange
	if n.LifecycleEvent != "" {
		kind = notifyLifecycle
	}
	if !p.clientStateOK(n.ClientState) { // logged once a body, and paced, by takeNotifications: anyone may send one
		p.subs.mismatch(n.SubscriptionID)
		return kind, notifiedClientState, false
	}
	if n.TenantID != p.c.tenant {
		p.c.log.WarnContext(ctx, "teams: a Graph notification of another tenant was dropped", "tenant", quote(n.TenantID))
		return kind, notifiedWrongTenant, false
	}
	if s == nil || !p.subs.ready.Load() {
		// Not running, or the subscriptions of this run are being listed still: Graph sends it again, when they are known.
		return kind, notifiedNotRunning, true
	}
	sub, ok := p.subs.byID(n.SubscriptionID)
	if !ok {
		p.c.log.DebugContext(ctx, "teams: a Graph notification of a subscription the connection does not hold was dropped",
			"kind", kind, "subscription", quote(n.SubscriptionID))
		return kind, notifiedUnknownSub, false
	}
	if kind == notifyLifecycle {
		return kind, p.lifecycle(ctx, sub, n.LifecycleEvent), false
	}
	switch n.ChangeType {
	case "created":
	case "updated", "deleted":
		return kind, notifiedEdit, false
	default:
		return kind, notifiedMalformed, false
	}
	ref, ok := parseMessageResource(n.Resource)
	if !ok || ref.group != sub.group || ref.channel != sub.channel {
		p.c.log.DebugContext(ctx, "teams: a Graph notification whose resource is not a message of its subscription's channel was dropped",
			"resource", quote(n.Resource))
		return kind, notifiedMalformed, false
	}
	shed, open := s.submit(sub.channel, func() { p.intake(s, sub, ref) }) //nolint:contextcheck // the lane runs on Serve's context, after the request is answered
	switch {
	case !open:
		return kind, notifiedNotRunning, true
	case shed != "":
		p.c.log.WarnContext(ctx, "teams: a Graph notification was not taken; answered 503, after which Graph sends it again",
			"channel", sub.channel, "shed", shed)
		return kind, notifiedShed, true
	}
	return kind, "", false
}

// lifecycle acts on the lifecycle event of sub, and returns the outcome it is counted under. reauthorizationRequired renews the
// subscription and subscriptionRemoved makes it anew, both on the subscriptions' loop, at once. missed means that Graph did not deliver
// some notifications of the channel: it is logged at WARN, told to Config.OnNotificationsMissed, and the channel is read back once, within
// the look-back (missed, poll.go); what was posted before that is not read.
func (p *push) lifecycle(ctx context.Context, sub subscription, event string) string {
	switch event {
	case "reauthorizationRequired":
		p.subs.flag(sub.id, flagRenew)
		return notifiedReauthorize
	case "subscriptionRemoved":
		p.c.log.WarnContext(ctx, "teams: Microsoft Graph removed the subscription of a channel; it is made anew", "channel", sub.channel)
		p.subs.flag(sub.id, flagRecreate)
		return notifiedRemoved
	case "missed":
		p.c.log.WarnContext(ctx, "teams: Microsoft Graph did not deliver some notifications of a channel; its recent posts are read back",
			"channel", sub.channel, "lookback", pollLookback.String())
		if p.onMissed != nil {
			p.onMissed(sub.channel)
		}
		p.missed(ctx, sub)
		return notifiedMissed
	}
	p.c.log.DebugContext(ctx, "teams: a lifecycle event the adapter does not know was dropped", "event", quote(event))
	return notifiedOtherLifecycle
}

// messageRef is the message a notification names: the team's Microsoft Entra group, the channel, the root of the message's thread ("" for
// a message that is a root) and its ID.
type messageRef struct {
	group, channel, root, id string
}

// parseMessageResource reads a notification's resource: teams('{group}')/channels('{channel}')/messages('{id}'), with /replies('{id}')
// after the root's for a reply, as Microsoft documents it, or the same as path segments (teams/{group}/channels/{channel}/messages/{id}),
// with or without a leading "/". The IDs must be message IDs (isMessageID).
func parseMessageResource(res string) (messageRef, bool) {
	segs, ok := resourceSegments(res)
	if !ok || (len(segs) != 3 && len(segs) != 4) || segs[0].name != "teams" || segs[1].name != "channels" || segs[2].name != "messages" {
		return messageRef{}, false
	}
	ref := messageRef{group: segs[0].key, channel: segs[1].key, id: segs[2].key}
	if len(segs) == 4 {
		if segs[3].name != "replies" || !isMessageID(segs[2].key) {
			return messageRef{}, false
		}
		ref.root, ref.id = segs[2].key, segs[3].key
	}
	if ref.group == "" || ref.channel == "" || !isMessageID(ref.id) {
		return messageRef{}, false
	}
	return ref, true
}

// resourceSegment is a segment of a Graph resource: a collection's name and the key of one of its items ("" for the collection itself).
type resourceSegment struct{ name, key string }

// resourceSegments reads res as segments, each name('key'), or name followed by its key as the next path segment; a trailing name has no
// key. A key is unescaped as a path segment.
func resourceSegments(res string) ([]resourceSegment, bool) {
	parts := strings.Split(strings.TrimPrefix(res, "/"), "/")
	var out []resourceSegment
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if name, rest, ok := strings.Cut(part, "('"); ok {
			key, closed := strings.CutSuffix(rest, "')")
			k, err := url.PathUnescape(key)
			if !closed || name == "" || err != nil {
				return nil, false
			}
			out = append(out, resourceSegment{name, k})
			continue
		}
		if part == "" {
			return nil, false
		}
		seg := resourceSegment{name: part}
		if i+1 < len(parts) {
			k, err := url.PathUnescape(parts[i+1])
			if err != nil || k == "" {
				return nil, false
			}
			seg.key = k
			i++
		}
		out = append(out, seg)
	}
	return out, len(out) > 0
}

// intake reads the message ref of sub's channel from Graph, on the channel's lane of s, and hands it to the sink as Run hands a person's
// post, unless it is no post for the sink:
//   - one that this Client posted (Client.sent: a post, or the card of a post's buttons) is the bot's own, and is not read;
//   - one that an activity, or an earlier notification, already handed on is a duplicate. The dedup is the receiver's, by channel and
//     message ID, so a person's post that Teams sends as an activity too (a mention of the bot, or any post under the resource-specific
//     permission) goes to the sink once, whichever comes first. The key is claimed after the read, so a read that fails takes nothing
//     from the activity;
//   - the message is decoded as a thread's message is (historyOf): the bot's own post, and anything that is no post (a system event, which
//     Graph gives the messageType unknownFutureValue; a deleted message; one with no sender or text), is dropped;
//   - another app's post (an alert's card that Workflows posted) is a bot's message, its sender the app's ID, its body its cards' text: it
//     is handed on only if the sink takes that bot in the channel (chat.BotFilter, or chat.BotMentionFilter for a mention of the bot), as on
//     Slack;
//   - a person's post is the person's, with Mentioned from Graph's mentions list.
//
// Its thread is the root that Graph's resource names, and none for a root. Each outcome is counted.
func (p *push) intake(s *serving, sub subscription, ref messageRef) {
	ctx := s.ctx
	if ctx.Err() != nil { // the drain at the stop ran out: Serve counts it as stopped
		return
	}
	r := p.c.recv
	tctx := r.telemetryContext(ctx)
	key := messageKey(sub.channel, ref.id)
	switch {
	case p.c.posted.has(key): // the Client posted it: no read, as the read would find it the bot's own
		p.count(tctx, notifyChange, notifiedOwn)
		return
	case r.seen.has(key):
		p.count(tctx, notifyChange, notifiedDuplicate)
		return
	}
	m, err := p.fetch(ctx, sub, ref)
	switch {
	case isNotFound(err):
		p.count(tctx, notifyChange, notifiedGone)
		return
	case err != nil && ctx.Err() != nil: // cut by the stop: Serve counts it as stopped
		return
	case err != nil:
		p.c.log.WarnContext(ctx, "teams: a notified message could not be read from Microsoft Graph, and is dropped", "channel", sub.channel,
			"message", ref.id, "error", err.Error())
		p.count(tctx, notifyChange, notifiedFetchFailed)
		return
	}
	root := ref.root
	if root == "" {
		root = ref.id
	}
	p.count(tctx, notifyChange, p.c.handOn(s, sub.channel, root, m))
}

// handOn hands the message m of channel, of the thread root, read from Graph (a notified message, or one that a poll or a catch-up read
// found, poll.go), to s's sink as Run hands a person's post, unless it is no post for the sink, and returns its outcome: own, ignored,
// not_named, duplicate, taken or not_taken (the notified* constants). It runs on the channel's lane. The message is decoded as a thread's
// message is (historyOf): the bot's own post, and anything that is no post, is dropped; another app's post is handed on only if the sink
// takes that bot in the channel (botTaken); the dedup key (messageKey) is claimed last, so a message dropped for another reason takes
// nothing from its activity.
func (c *Client) handOn(s *serving, channel, root string, m graphMessage) string {
	r := c.recv
	key := messageKey(channel, m.ID)
	h := c.historyOf(channel, root, m)
	switch {
	case h.Own:
		return notifiedOwn
	case h.Ignored:
		return notifiedIgnored
	}
	msg := h.Message
	if msg.Bot != nil && !botTaken(s.sink, msg) {
		return notifiedNotNamed
	}
	if !r.seen.claim(key) {
		return notifiedDuplicate
	}
	if r.deliver(s, work{msg: &msg, key: key}, lostGraphNotTaken) {
		return notifiedTaken
	}
	return notifiedNotTaken
}

// botTaken says whether the sink takes the bot's message m: a mention of Agent Kourier by its BotMentionFilter, anything else by its
// BotFilter, and nothing from a sink that cannot say (a platform fails closed, chat.BotFilter).
func botTaken(sink chat.Sink, m chat.Inbound) bool {
	if m.Mentioned {
		f, ok := sink.(chat.BotMentionFilter)
		return ok && f.AcceptsBotMention(m.At.Channel, m.Bot.ID)
	}
	f, ok := sink.(chat.BotFilter)
	return ok && f.AcceptsBot(m.At.Channel, m.Bot.ID)
}

// errAnotherMessage is a read of a notified message that Graph answered with another.
var errAnotherMessage = errors.New("teams: Microsoft Graph answered the notified message with another")

// fetch reads the message ref of sub's channel, GET /teams/{group}/channels/{channel}/messages/{id}, or …/messages/{root}/replies/{id} for
// a reply, and tries again as the constants above say when Classify calls the failure Transient.
func (p *push) fetch(ctx context.Context, sub subscription, ref messageRef) (graphMessage, error) {
	segs := []string{"teams", sub.group, "channels", sub.channel, "messages", ref.id}
	if ref.root != "" {
		segs = []string{"teams", sub.group, "channels", sub.channel, "messages", ref.root, "replies", ref.id}
	}
	link := p.c.graph.resource(segs...)
	for attempt := 1; ; attempt++ {
		m, err := p.fetchOnce(ctx, link)
		if err == nil && m.ID != ref.id {
			err = errAnotherMessage
		}
		if err == nil {
			return m, nil
		}
		v := p.c.Classify(chat.OpOther, err)
		if v.Class != chat.Transient || errors.Is(err, errAnotherMessage) || attempt == fetchAttempts || ctx.Err() != nil {
			return graphMessage{}, err
		}
		wait := backoff.Doubling(p.fetchRetry, p.fetchRetryMax, attempt)
		if v.RetryAfter > wait {
			wait = min(v.RetryAfter, p.fetchRetryMax)
		}
		if !waitFor(ctx, wait) {
			return graphMessage{}, err
		}
	}
}

// fetchOnce is one GET of a notified message, cut at fetchTimeout and counted as FetchMessage.
func (p *push) fetchOnce(ctx context.Context, link string) (m graphMessage, err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.FetchMessage")
	defer func() { p.c.finish(ctx, op, "FetchMessage", err) }()
	ctx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
	defer cancel()
	err = p.c.graph.get(ctx, link, &m)
	return m, err
}

// httpsRoute is the handler of a connection in http mode with a notificationURL: Graph's requests (push.serveGraph), and the Receiver for
// every other, which verifies its token. It asks the chat listener for the Receiver's deadline.
type httpsRoute struct {
	p    *push
	recv *Receiver
}

func (h httpsRoute) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !h.p.serveGraph(w, req) {
		h.recv.ServeHTTP(w, req)
	}
}

// HandlerTimeout implements chat.HTTPDeadline: the Receiver's.
func (h httpsRoute) HandlerTimeout() time.Duration { return h.recv.HandlerTimeout() }

// graphRoute is the handler of a connection in socket mode with a notificationURL: Graph's requests alone. An activity comes on the
// connection's sockets and never over HTTPS, so anything else is answered 400 and taken by nothing.
type graphRoute struct{ p *push }

func (h graphRoute) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !h.p.serveGraph(w, req) {
		h.p.c.log.DebugContext(req.Context(), "teams: a request that is not Microsoft Graph's came to a connection in socket mode; answered 400")
		http.Error(w, "the connection is in socket mode: it takes only Microsoft Graph's notifications over HTTPS", http.StatusBadRequest)
	}
}

var (
	_ http.Handler      = httpsRoute{}
	_ chat.HTTPDeadline = httpsRoute{}
	_ http.Handler      = graphRoute{}
)
