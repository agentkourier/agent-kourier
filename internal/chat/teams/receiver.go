package teams

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/backoff"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// stopDrain bounds how long Serve, as it stops, goes on handing to the sink and the store the activities it had already answered 200.
// The broker stops a connection in the intake step of its shutdown, whose bound is 4 s, after the chat listener's drain of up to 2 s,
// so the drain keeps to half of what is left. What it has not handed on by then is lost: it was answered 200, and Teams does not send
// again an activity it was answered 200 for (see Receiver).
const stopDrain = time.Second

// How a lane hands the sink a post it did not take. The sink's error means a retry may cure it (chat.Sink), and on Slack the platform
// makes that retry by sending the event again; Teams does not send again an activity answered 200 (see Receiver), so the lane makes it, at most sinkAttempts times
// in all, waiting sinkRetryBase after the first failure and twice as long after each further one, up to sinkRetryMax. The channel's
// later activities wait behind it, so the bound keeps one failing post from holding its lane for longer than about 7 s of waits.
const (
	sinkAttempts  = 4
	sinkRetryBase = time.Second
	sinkRetryMax  = 4 * time.Second
)

// refusalLogEvery paces the WARN line of a refusal of a token the Bot Framework signed, for each reason: the first at once, and then one
// line for each interval, which says how many were refused since the last. A connection whose app ID or clock is wrong refuses every
// request, and would otherwise write a line for each.
const refusalLogEvery = 10 * time.Second

// refusalLog paces the WARN lines of refused tokens, by reason. The handlers of requests that come at once call it, so it has a lock.
type refusalLog struct {
	mu         sync.Mutex
	last       map[Reason]time.Time // guarded by mu
	suppressed map[Reason]int       // guarded by mu
}

// admit says whether a refusal for reason at now gets a line, and how many were refused for it since the last line.
func (l *refusalLog) admit(reason Reason, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.last[reason]; ok && now.Sub(last) < refusalLogEvery {
		l.suppressed[reason]++
		return false, 0
	}
	n := l.suppressed[reason]
	l.last[reason], l.suppressed[reason] = now, 0
	return true, n
}

// ReceiverConfig configures a Receiver.
type ReceiverConfig struct {
	// Connection is the ChatConnection, "namespace/name", that the activities come to. Required.
	Connection string
	// TenantID is the connection's Microsoft Entra tenant (spec.teams.tenantID): an activity of another tenant is refused. Required.
	TenantID string
	// AppID is the bot's Microsoft Entra application ID (spec.teams.appID): the audience a token must name, and the bot's Teams ID,
	// 28:<AppID>. Required.
	AppID string
	// MetadataURL is the Bot Framework's OpenID metadata, which names its signing keys; DefaultMetadataURL if empty. A test sets a fake's.
	MetadataURL string
	// HTTPClient fetches the metadata and the keys; http.DefaultClient if nil. Each fetch is bounded by keysFetchTimeout whatever the
	// client's own timeout.
	HTTPClient *http.Client
	// Conversations keeps what the activities say about their channels; a MemoryConversations if nil.
	Conversations ConversationStore
	// Invokes answers invoke activities; with none, every invoke is answered 200 with no body.
	Invokes InvokeHandler
	// Logger is slog.Default() if nil. The caller names the connection on it.
	Logger *slog.Logger
	// Now is the clock of the tokens, the keys' age and the dedup; time.Now if nil.
	Now func() time.Time
}

// Receiver takes what Microsoft Teams sends a connection's bot over HTTPS, at the connection's route on the chat listener
// (/chat/teams/<namespace>/<name>), and hands it on: a person's post to the Sink of the live Serve, an invoke to the InvokeHandler, and
// what an activity says of its channel to the ConversationStore. It is safe for concurrent use.
//
// Teams waits about 25 seconds for the answer to a message activity and about 14 for an invoke, while the chat listener gives a handler
// 3 seconds. So the request does only what is quick: the token, the tenant, the dedup and a place on its channel's lane, and it answers
// then; the sink and the store are called after, on the lane, one activity of a channel at a time and in the order the requests were
// taken, with the lanes of different channels running at once. An invoke is answered in its request, by the InvokeHandler, which must
// answer at once.
//
// What Teams sends again was measured on a test tenant on 2026-11-18: an activity answered 200, or one whose request it held until it
// gave up, never; a message answered 503 at once, once, about 0.6 seconds later, with the same ID and body; an invoke answered 503, and
// other status codes, were not measured. So the receiver releases the dedup key of an activity it answers 503, for a second delivery to
// be taken.
type Receiver struct {
	conn, tenant, appID string
	verifier            *verifier
	convs               ConversationStore
	invokes             InvokeHandler
	log                 *slog.Logger
	seen                *recent
	sinkRetryBase       time.Duration // sinkRetryBase; a test shortens it
	keysCheckEvery      time.Duration // keysCheckEvery; a test shortens it
	now                 func() time.Time
	refusals            refusalLog

	// live is the Serve that takes activities, or nil when none does.
	live atomic.Pointer[serving]

	recMu    sync.Mutex
	recorded map[string]Conversation // by channel: what this receiver last wrote to the store; guarded by recMu
}

// serving is one Serve, as the handler finds it. The handler submits under mu held for reading, and Serve sets closed under mu held
// for writing as it stops, before it waits for the lanes, so that nothing is submitted while it waits.
type serving struct {
	// ctx is the context of the work on the lanes: Serve's values, and an end of its own, after the drain.
	ctx   context.Context
	sink  chat.Sink
	lanes *lanes

	mu     sync.RWMutex
	closed bool // guarded by mu
}

// work is one accepted activity for its lane: what it tells the store, if anything, the post for the sink, if it is one, and its dedup
// key, which is released if the post is not taken.
type work struct {
	conv *Conversation
	msg  *chat.Inbound
	key  string
}

// NewReceiver returns a Receiver for cfg, with the verifier of cfg.AppID's tokens. It refuses a config with no connection, tenant or
// app, which would take any tenant's activities or no token. It takes no request until Serve runs.
func NewReceiver(cfg ReceiverConfig) (*Receiver, error) {
	switch {
	case cfg.Connection == "":
		return nil, errors.New("teams receiver: no connection")
	case cfg.TenantID == "":
		return nil, errors.New("teams receiver: no tenant ID")
	case cfg.AppID == "":
		return nil, errors.New("teams receiver: no app ID")
	}
	r := &Receiver{conn: cfg.Connection, tenant: cfg.TenantID, appID: cfg.AppID, convs: cfg.Conversations, invokes: cfg.Invokes,
		log: cfg.Logger, recorded: map[string]Conversation{}, sinkRetryBase: sinkRetryBase,
		keysCheckEvery: keysCheckEvery}
	if r.convs == nil {
		r.convs = NewMemoryConversations()
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	r.now, r.refusals = now, refusalLog{last: map[Reason]time.Time{}, suppressed: map[Reason]int{}}
	r.verifier = newVerifier(cfg.AppID, cfg.MetadataURL, cfg.HTTPClient, now)
	r.seen = newRecent(now)
	return r, nil
}

// Serve hands what the handler accepts to sink and to the store, until ctx ends. It starts a fetch of the signing keys so that the
// first token finds them, and checks their age every keysCheckEvery, so that they are fetched again in the background before they are
// too old even when no request comes. When ctx ends it stops taking requests (the handler answers 503), goes on handing on what it had accepted
// for at most stopDrain, then ends the context of that work (the sink's call that is running sees it, and what waits is dropped),
// waits for the lanes to stop, and returns ctx's error. One Serve runs at a time; a second replaces the first as the one the
// handler submits to.
func (r *Receiver) Serve(ctx context.Context, sink chat.Sink) error {
	wctx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWork()
	s := &serving{ctx: wctx, sink: sink, lanes: newLanes(channelBacklog, pendingActivities)}
	r.live.Store(s)
	r.verifier.warm(ctx)
	r.log.InfoContext(ctx, "teams: taking activities over https")
	check := time.NewTicker(r.keysCheckEvery)
	for running := true; running; {
		select {
		case <-check.C:
			r.verifier.warm(ctx)
		case <-ctx.Done():
			running = false
		}
	}
	check.Stop()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	r.live.CompareAndSwap(s, nil)
	drained := make(chan struct{})
	go func() {
		s.lanes.wait()
		close(drained)
	}()
	timer := time.NewTimer(stopDrain)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		n := s.lanes.held()
		stopWork() // what is still on the lanes sees the end and is not handed on
		<-drained  // nothing calls the sink or the store after Serve returns
		r.log.WarnContext(wctx, "teams: activities answered 200 were dropped as the connection stopped, and Teams does not send them again", "count", n)
	}
	return fmt.Errorf("teams https: %w", ctx.Err())
}

// handle records w's conversation and gives its post to s's sink, unless the work's context has ended. Then it drops w, and releases
// w's dedup key if w carries a post, so that the same activity is taken if it comes again; an activity with no post keeps its key,
// since it was answered (an invoke by the InvokeHandler) and only its record is lost.
func (r *Receiver) handle(s *serving, w work) {
	if s.ctx.Err() != nil {
		if w.msg != nil {
			r.release(w)
		}
		return
	}
	if w.conv != nil {
		r.remember(s.ctx, *w.conv)
	}
	if w.msg != nil {
		r.deliver(s, w)
	}
}

// deliver gives w's post to s's sink, again after a failure, up to sinkAttempts times in all or until the work's context ends. A post
// the sink never takes is dropped: its dedup key is released, so that the same activity is taken if it comes again, and the drop is
// logged at WARN.
func (r *Receiver) deliver(s *serving, w work) {
	for attempt := 1; ; attempt++ {
		err := s.sink.Message(s.ctx, *w.msg)
		if err == nil {
			return
		}
		if attempt == sinkAttempts || s.ctx.Err() != nil || !waitFor(s.ctx, backoff.Doubling(r.sinkRetryBase, sinkRetryMax, attempt)) {
			r.release(w)
			r.log.WarnContext(s.ctx, "teams: the sink did not take a message, which is dropped",
				"channel", quote(w.msg.At.Channel), "message", w.msg.At.ID, "attempts", attempt, "error", err.Error())
			return
		}
	}
}

// waitFor waits d, and says false if ctx ends first.
func waitFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// release forgets w's dedup key, for an activity whose post was not taken.
func (r *Receiver) release(w work) {
	if w.key != "" {
		r.seen.forget(w.key)
	}
}

// remember writes c to the store unless it adds nothing to what this receiver last wrote for its channel.
func (r *Receiver) remember(ctx context.Context, c Conversation) {
	r.recMu.Lock()
	last, ok := r.recorded[c.Channel]
	r.recMu.Unlock()
	merged := last.merge(c)
	if ok && merged == last {
		return
	}
	if err := r.convs.Remember(ctx, r.conn, c); err != nil {
		r.log.WarnContext(ctx, "teams: the channel's service URL and team could not be recorded", "channel", quote(c.Channel), "error", err.Error())
		return
	}
	r.recMu.Lock()
	r.recorded[c.Channel] = merged
	r.recMu.Unlock()
}

// submit queues fn on the lane of channel unless Serve has stopped (open false) or the lanes shed it (shed, a reason). It never blocks.
func (s *serving) submit(channel string, fn func()) (shed string, open bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return "", false
	}
	return s.lanes.submit(channel, fn), true
}

// ServeHTTP answers one request Teams sends the connection. In order: the token is verified (401, or 503 when the keys cannot be
// read), the body read as an activity of Teams (400), the token's service URL checked against the activity's (401), and the tenant
// against the connection's (403); none of these work on the activity. With no Serve live it is 503, and nothing is claimed for the
// dedup. Then:
//   - A message puts its post, if it is a person's, and its conversation on its channel's lane, and is answered 200 at once; an ID
//     with ':' is 400, and lanes that shed it 503.
//   - An invoke is answered by the InvokeHandler in the request, whatever the lanes do with its conversation; one whose replyToId
//     holds ':' is 400.
//   - Anything else (an edit, a deletion, an install event) puts its conversation on the lane, and is answered 200, or 503 if shed.
//
// An activity that came before, by its type, channel and ID, within the dedup window, is answered 200 and handed on to nothing.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	tok, err := r.verifier.verify(ctx, req.Header.Get("Authorization"))
	if err != nil {
		r.refuseToken(ctx, w, err)
		return
	}
	body, err := io.ReadAll(req.Body) // the chat listener has read it whole and capped it: this reads memory
	if err != nil {
		http.Error(w, "the body could not be read", http.StatusBadRequest)
		return
	}
	a, err := decodeActivity(body)
	if err != nil {
		r.log.DebugContext(ctx, "teams: a verified request is no activity; answered 400", "bytes", len(body), "reason", err.Error())
		http.Error(w, "not an activity of Microsoft Teams", http.StatusBadRequest)
		return
	}
	if err := tok.checkServiceURL(a.ServiceURL); err != nil {
		r.refuseToken(ctx, w, err)
		return
	}
	if got := a.tenant(); got != r.tenant {
		r.log.WarnContext(ctx, "teams: an activity of another tenant was refused; the bot is installed outside the connection's tenant",
			"tenant", quote(got), "connectionTenant", r.tenant, "type", quote(a.Type))
		http.Error(w, "the activity is not of the connection's tenant", http.StatusForbidden)
		return
	}
	s := r.live.Load()
	if s == nil {
		http.Error(w, "the connection does not take activities now", http.StatusServiceUnavailable)
		return
	}
	var (
		conv *Conversation
		msg  *chat.Inbound
		inv  *Invoke
	)
	if c, ok := a.conversationRecord(); ok {
		conv = &c
	}
	switch a.kind() {
	case kindMessage:
		m, v, why := a.message(r.conn, r.appID)
		switch v {
		case invalid:
			r.log.DebugContext(ctx, "teams: a message was refused; answered 400", "reason", why)
			http.Error(w, "the message has no ID that can name it", http.StatusBadRequest)
			return
		case forward:
			msg = &m
		default:
			r.log.DebugContext(ctx, "teams: a message is no person's post; acknowledged", "reason", why)
		}
	case kindInvoke:
		i, ok := a.invoke(r.conn)
		if !ok {
			r.log.DebugContext(ctx, "teams: an invoke was refused, since its replyToId holds ':'; answered 400")
			http.Error(w, "the invoke names no message that an ID can name", http.StatusBadRequest)
			return
		}
		inv = &i
	}
	channel, _ := splitConversation(a.Conversation.ID)
	key := ""
	if a.ID != "" {
		key = a.Type + "\x00" + channel + "\x00" + a.ID
		if !r.seen.claim(key) {
			r.log.DebugContext(ctx, "teams: an activity came again; acknowledged", "type", quote(a.Type), "id", a.ID)
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	if conv != nil || msg != nil {
		item := work{conv: conv, msg: msg, key: key}
		shed, open := s.submit(channel, func() { r.handle(s, item) })
		// An invoke is answered even when its record is shed: the record is all it puts on the lane, and the next activity of the
		// channel carries the same.
		if !open || (shed != "" && inv == nil) {
			if key != "" {
				r.seen.forget(key)
			}
			r.log.WarnContext(ctx, "teams: an activity was not taken; answered 503, after which Teams sends a message again once (an invoke was not measured)",
				"type", quote(a.Type), "channel", quote(channel), "shed", shed)
			http.Error(w, "the connection cannot take the activity now", http.StatusServiceUnavailable)
			return
		}
	}
	if inv != nil {
		r.answerInvoke(ctx, w, *inv)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// answerInvoke writes the InvokeHandler's answer to inv, or 200 with no body when there is no handler.
func (r *Receiver) answerInvoke(ctx context.Context, w http.ResponseWriter, inv Invoke) {
	if r.invokes == nil {
		r.log.DebugContext(ctx, "teams: an invoke was answered with no body; nothing handles invokes", "name", quote(inv.Name))
		w.WriteHeader(http.StatusOK)
		return
	}
	ans := r.invokes.Invoke(ctx, inv)
	status := ans.Status
	if status == 0 {
		status = http.StatusOK
	}
	if len(ans.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(ans.Body)
}

// refuseToken answers a request whose token was refused: 503 when the keys could not be read, which is no fault of the request, and 401
// otherwise. A refusal of a token the Bot Framework signed (Reason.signed) is logged at WARN, paced by refusalLog, since it means the
// connection's app ID or the broker's clock is wrong and the bot hears nothing; the others, which anyone can cause, at DEBUG. The log
// names the reason and never the token.
func (r *Receiver) refuseToken(ctx context.Context, w http.ResponseWriter, err error) {
	var te *TokenError
	if !errors.As(err, &te) {
		te = &TokenError{}
	}
	switch {
	case te.Reason == ReasonKeysUnavailable:
		r.log.WarnContext(ctx, "teams: a request was refused: the Bot Framework's signing keys could not be read", "error", err.Error())
		http.Error(w, "the token cannot be verified now", http.StatusServiceUnavailable)
		return
	case te.Reason.signed():
		if ok, n := r.refusals.admit(te.Reason, r.now()); ok {
			r.log.WarnContext(ctx, "teams: a token the Bot Framework signed was refused; check the connection's app ID and the broker's clock",
				"reason", string(te.Reason), "error", err.Error(), "alsoRefusedSinceLastLine", n)
		}
	default:
		r.log.DebugContext(ctx, "teams: a request was refused: its token is not the Bot Framework's for the connection's bot", "error", err.Error())
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

var _ http.Handler = (*Receiver)(nil)
