package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agentkourier/agent-kourier/internal/backoff"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// Socket mode, Microsoft's public preview of a delivery with no public endpoint ("Connect an agent without the SDK" and "Configure
// delivery methods for agents" in Microsoft's documentation, read 2026-11-17), as it was measured on a test tenant on 2026-11-17 and
// 2026-11-18: a POST to https://<region>.botapi.skype.com/v3/websockets/connect with the bot's token answers a SignalR client URL and an
// access token for one socket, which expires after expiresIn seconds (600 measured); the client runs SignalR's negotiate at that URL,
// which was already the SignalR service's host and answered a connection token at once (a redirect to another host with a token of its
// own, which the client follows, is in SignalR's protocol and was not seen), opens the WebSocket and makes the handshake of SignalR's
// JSON protocol; the service calls SocketReady, then Activity with an envelope for each activity, whose completion is the reply frame, and
// SocketClosing before it closes a socket on purpose. amer and emea were ready in under a second; apac answered 403 "Socket mode is
// not enabled for this bot." while the others worked. A second socket of the bot in a region takes all of its traffic, newest first,
// and the older gets nothing and no SocketClosing; so only the leader runs a connection's sockets (the broker runs Client.Run on the
// leader alone). An activity sent while no socket is connected is not delivered later, nor to an HTTPS endpoint: it is lost, and Gap
// says when that could have happened.

// socketRegions are the regions whose connect endpoint the connection calls, each for a socket of its own.
func socketRegions() []string { return []string{"amer", "emea", "apac"} }

// defaultSocketURL is the connect endpoint of region.
func defaultSocketURL(region string) string {
	return "https://" + url.PathEscape(region) + ".botapi.skype.com/v3/websockets/connect"
}

// socketReadyWait bounds the open of a socket from the WebSocket's dial to the service's SocketReady, which Microsoft's documentation
// says comes within 30 seconds. A socket that is not ready by then is closed, and its region connects again.
const socketReadyWait = 30 * time.Second

// SignalR's keep-alive, from its protocol's defaults (Microsoft's page names no other): the client pings every socketPingEvery, and
// a socket on which the service has sent nothing for socketServerTimeout is taken as dead, closed, and its region connects again.
const (
	socketPingEvery     = 15 * time.Second
	socketServerTimeout = 30 * time.Second
)

// socketWriteWait bounds one write to a socket (a reply frame, a ping, the close), so that a connection the network left dead does not
// hold a writer; a write cut so ends the socket.
const socketWriteWait = 10 * time.Second

// socketReplaceLead is how long before a socket's token expires its region opens the next, which names the old as the one it
// replaces; a socket whose token lives less than twice the lead (only a fake's does) is replaced at half its life. The old socket is
// kept socketRetireAfter more, and then until the envelopes it is handling are answered, at most socketRetireMax more, and closed:
// Microsoft's documentation says to keep it a few seconds, and that the service closes it itself (Replaced) a minute after.
const (
	socketReplaceLead = time.Minute
	socketRetireAfter = 5 * time.Second
	socketRetireMax   = 30 * time.Second
)

// socketStopWait is how long a socket, as the connection stops, waits for the replies of the envelopes it is handling before it closes.
// Their work sees the stop at once, so a press is answered that it was received.
const socketStopWait = time.Second

// A region's connects back off after a failure: socketRetryBase, doubling up to socketRetryMax, each wait cut at random by up to a
// fifth or lengthened by as much. A socket that was ready for at least socketReconnectNow and then ended is followed by a connect at
// once; one that ended sooner is a failure too, so that a socket the service closes as it opens does not make a busy loop. A 429 waits
// its Retry-After, if it is longer, up to socketRetryAfterMax. A region that answers 403 (Socket mode not enabled for the bot in it), or
// whose socket was closed by a SocketClosing that says not to reconnect, waits socketRefusedRetry.
const (
	socketRetryBase     = time.Second
	socketRetryMax      = 15 * time.Second
	socketRetryAfterMax = 5 * time.Minute
	socketRefusedRetry  = 5 * time.Minute
	socketReconnectNow  = 15 * time.Second
)

// socketMaxFrame is the longest WebSocket message read; a longer one ends the socket, and its region connects again. An activity over
// HTTPS is held to the chat listener's 1 MiB, and a message may carry several records of SignalR's protocol.
const socketMaxFrame = 4 << 20

// socketMaxBody is the most of an answer to the connect call or to a negotiate that is read; each is a small JSON object.
const socketMaxBody = 64 << 10

// socketMaxRedirects bounds the redirects of one negotiate. SignalR's protocol lets a negotiate answer one; none was seen.
const socketMaxRedirects = 5

// socketMaxInFlight is how many envelopes a connection's sockets handle at once. One past it is answered 503 at once and not worked
// on. A message is answered as soon as it is on its channel's lane, so only presses, which wait for their decision, hold a place long.
const socketMaxInFlight = 32

// socketDefaultDeadline is the deadline of an envelope that names none: 25 seconds, the deadlineMs of every envelope measured. The
// answer is given socketReplyMargin before it, and an invoke's no later than invokeTimeout, as over HTTPS.
const (
	socketDefaultDeadline = 25 * time.Second
	socketReplyMargin     = time.Second
)

// recordSeparator ends each record of SignalR's JSON protocol.
const recordSeparator = 0x1e

// The reasons a region connects again, the label of agentkourier_teams_socket_reconnects_total: a closed set.
const (
	reasonExpiry        = "expiry"         // a replacement before the socket's token expired
	reasonClosed        = "closed"         // the socket closed, or could not be read or written
	reasonServerTimeout = "server_timeout" // the service sent nothing for socketServerTimeout
	reasonReadyTimeout  = "ready_timeout"  // no SocketReady within socketReadyWait
	reasonRateLimited   = "rate_limited"   // the connect call was answered 429
	reasonRefused       = "refused"        // the connect call was answered 403
	reasonUnavailable   = "unavailable"    // the connect call was answered 5xx
	reasonFailed        = "failed"         // the connect call, a negotiate, the dial or the handshake failed otherwise
	reasonClosingOther  = "closing_other"  // a SocketClosing with a code not listed in closingReasons
)

// closingReasons are the codes of SocketClosing that Microsoft documents, by the reason they are counted under.
var closingReasons = map[string]string{
	"SocketModeDisabled": "socket_mode_disabled",
	"Replaced":           "replaced",
	"TokenReused":        "token_reused",
	"NoIdentity":         "no_identity",
}

// The outcomes of an envelope that are a socket's own, besides the receiver's (requestAccepted and the rest), in
// agentkourier_teams_socket_envelopes_total.
const (
	envelopeBusy       = "busy"       // answered 503 at once: socketMaxInFlight were being handled
	envelopeUnanswered = "unanswered" // the reply frame could not be sent
)

// Gap is an interval in which no socket of a connection in socket mode was ready. Teams delivers no activity to the bot in it, and
// does not deliver it later, nor to an HTTPS endpoint (measured, 2026-11-18): the activities sent in it are lost, unless something
// else reads them (Microsoft Graph).
type Gap struct {
	// Start is when the last ready socket ended, or when the run began. End is when a socket was ready again.
	Start, End time.Time
	// Initial says the gap began with the run: activities sent before Start, since the connection's last run ended (in this process
	// or in a leader before it), were lost too, and the run cannot know since when.
	Initial bool
}

// SocketError is a refusal of the connect call of a region: its status, the wait it asked for, and the error it named, as Microsoft
// wrote it ("Socket mode is not enabled for this bot.", say). It holds no token.
type SocketError struct {
	Region     string
	Status     int
	RetryAfter time.Duration
	Message    string
}

func (e *SocketError) Error() string {
	s := fmt.Sprintf("teams socket mode: the connect call of %s answered %d", e.Region, e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// errReadyTimeout is the error of a socket the service did not call SocketReady on in time.
var errReadyTimeout = errors.New("teams socket mode: no SocketReady in time")

// sockets runs the sockets of one connection in socket mode, one for each region that answers, and hands their envelopes to the
// receiver. The waits are fields, for a test to shorten.
type sockets struct {
	conn         string
	recv         *Receiver
	tokens       *tokenSource
	client       *http.Client // follows no redirect, and bounds each call
	dialer       *websocket.Dialer
	connectURL   func(region string) string
	regions      []string
	log          *slog.Logger
	onConnection func(bool)
	onGap        func(Gap)
	slots        chan struct{}  // a place for each envelope being handled, socketMaxInFlight in all
	handlers     sync.WaitGroup // the envelopes being handled, which run waits for before it returns
	jitter       func() float64

	readyWait, pingEvery, serverTimeout, retireAfter, retireMax, stopWait, reconnectNow time.Duration
	retryBase, retryMax, refusedRetry                                                   time.Duration
	invokeWait                                                                          time.Duration // invokeTimeout: an invoke's answer is sent by it at the latest
}

func newSockets(conn string, recv *Receiver, tokens *tokenSource, client *http.Client, cfg Config, log *slog.Logger) *sockets {
	connectURL := cfg.SocketURL
	if connectURL == nil {
		connectURL = defaultSocketURL
	}
	return &sockets{conn: conn, recv: recv, tokens: tokens, client: client, connectURL: connectURL, regions: socketRegions(), log: log,
		dialer:       &websocket.Dialer{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg.SocketTLS, HandshakeTimeout: socketReadyWait},
		onConnection: cfg.OnConnection, onGap: cfg.OnSocketGap, slots: make(chan struct{}, socketMaxInFlight), jitter: rand.Float64,
		readyWait: socketReadyWait, pingEvery: socketPingEvery, serverTimeout: socketServerTimeout, retireAfter: socketRetireAfter,
		retireMax: socketRetireMax, stopWait: socketStopWait, reconnectNow: socketReconnectNow, retryBase: socketRetryBase,
		retryMax: socketRetryMax, refusedRetry: socketRefusedRetry, invokeWait: invokeTimeout}
}

// regionEvent is what a region tells the run: its socket is ready or is not, its connect was refused (403) or failed otherwise, or
// the bot's credentials were rejected (401), which ends the run.
type regionEvent struct {
	region string
	kind   int
	err    error
}

const (
	regionUp = iota
	regionDown
	regionRefused
	regionFailed
	regionRejected
)

// run runs a socket in each region until ctx ends, and returns ctx's error then. It returns sooner with the error of a region whose
// connect call said the bot's credentials are rejected (a *SocketError of status 401, or Entra's refusal of the bot's token), which
// Client.Classify says is FatalAuth, or with the error of the last refusal when every region has refused it (403) and none is ready.
// OnConnection is told true when a region's socket is ready and none was, and false when the last ready one ends; each interval with
// none ready is a Gap, logged and observed in agentkourier_teams_socket_gap_seconds, and given to OnSocketGap when it ends.
func (s *sockets) run(ctx context.Context) error {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan regionEvent)
	var wg sync.WaitGroup
	for _, region := range s.regions {
		wg.Go(func() {
			s.region(rctx, region, func(ev regionEvent) bool {
				select {
				case events <- ev:
					return true
				case <-rctx.Done():
					return false
				}
			})
		})
	}
	st := runState{ready: map[string]bool{}, refused: map[string]error{}, gapStart: time.Now(), initial: true}
	var err error
	for err == nil {
		select {
		case <-ctx.Done():
			err = fmt.Errorf("teams socket mode: %w", ctx.Err())
		case ev := <-events:
			err = s.apply(ctx, &st, ev)
		}
	}
	cancel()
	wg.Wait()         // no region sends once rctx has ended, and each has closed its sockets, so no envelope is taken after
	s.handlers.Wait() // their contexts have ended, so each answers at once; none counts a series after run returns
	for _, region := range s.regions {
		telemetry.SetGauge(ctx, "agentkourier_teams_socket_ready", 0, region, s.conn)
	}
	if len(st.ready) > 0 {
		if s.onConnection != nil {
			s.onConnection(false)
		}
	} else { // the gap that the stop ends: it is observed, and no socket follows it in this run to say so
		telemetry.Observe(ctx, "agentkourier_teams_socket_gap_seconds", time.Since(st.gapStart), s.conn)
	}
	return err
}

// runState is what run knows of the regions.
type runState struct {
	ready    map[string]bool
	refused  map[string]error // the regions whose last connect was answered 403, and the answer; a ready socket or another failure clears it
	gapStart time.Time        // when the gap began, while no region is ready
	initial  bool             // the gap began with the run
}

// apply takes one region's event, and returns the error that ends the run, if it is one.
func (s *sockets) apply(ctx context.Context, st *runState, ev regionEvent) error {
	switch ev.kind {
	case regionUp:
		delete(st.refused, ev.region)
		telemetry.SetGauge(ctx, "agentkourier_teams_socket_ready", 1, ev.region, s.conn)
		st.ready[ev.region] = true
		if len(st.ready) == 1 {
			s.gapEnded(ctx, Gap{Start: st.gapStart, End: time.Now(), Initial: st.initial})
			if s.onConnection != nil {
				s.onConnection(true)
			}
		}
	case regionDown:
		telemetry.SetGauge(ctx, "agentkourier_teams_socket_ready", 0, ev.region, s.conn)
		if !st.ready[ev.region] {
			return nil
		}
		delete(st.ready, ev.region)
		if len(st.ready) == 0 {
			st.gapStart, st.initial = time.Now(), false
			s.log.WarnContext(ctx, "teams: no socket of the connection is ready; what Teams sends until one is, it does not deliver, now or later",
				"since", st.gapStart.UTC().Format(time.RFC3339Nano))
			if s.onConnection != nil {
				s.onConnection(false)
			}
		}
	case regionFailed:
		delete(st.refused, ev.region)
	case regionRefused:
		st.refused[ev.region] = ev.err
		if len(st.ready) == 0 && len(st.refused) == len(s.regions) {
			return fmt.Errorf("teams socket mode: every region refused the bot's socket; check that the bot's delivery method is Socket mode: %w", ev.err)
		}
	case regionRejected:
		return ev.err
	}
	return nil
}

// gapEnded logs g, observes its length and gives it to OnSocketGap.
func (s *sockets) gapEnded(ctx context.Context, g Gap) {
	d := g.End.Sub(g.Start)
	telemetry.Observe(ctx, "agentkourier_teams_socket_gap_seconds", d, s.conn)
	if g.Initial {
		s.log.InfoContext(ctx, "teams: a socket is ready; taking activities over Socket mode", "afterSeconds", d.Seconds())
	} else {
		s.log.WarnContext(ctx, "teams: a socket is ready again; what Teams sent while none was is lost",
			"start", g.Start.UTC().Format(time.RFC3339Nano), "end", g.End.UTC().Format(time.RFC3339Nano), "seconds", d.Seconds())
	}
	if s.onGap != nil {
		s.onGap(g)
	}
}

// region keeps a socket of region ready until ctx ends: it connects, replaces the socket before its token expires, and connects again
// when it ends, with the waits that the constants above say. It tells the run what happens through report, which says false once the
// run takes no more; it returns then, and when ctx ends, having closed its socket.
func (s *sockets) region(ctx context.Context, region string, report func(regionEvent) bool) {
	var retiring sync.WaitGroup
	defer retiring.Wait()
	failures, reason := 0, ""
	var wait time.Duration
	for {
		if wait > 0 && !waitFor(ctx, wait) {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if reason != "" {
			telemetry.Count(ctx, "agentkourier_teams_socket_reconnects_total", region, reason, s.conn)
		}
		sk, err := s.open(ctx, region, "")
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var ok bool
			if wait, reason, ok = s.failed(ctx, region, err, &failures, report); !ok {
				return
			}
			continue
		}
		readyAt := time.Now()
		s.log.InfoContext(ctx, "teams: socket ready", "region", region)
		if !report(regionEvent{region: region, kind: regionUp}) {
			s.retire(ctx, sk, 0)
			return
		}
		sk, reason, wait = s.hold(ctx, region, sk, &retiring, report)
		if sk == nil { // ctx ended, or the run took no more: the socket is closed
			return
		}
		if !report(regionEvent{region: region, kind: regionDown}) {
			return
		}
		switch {
		case wait > 0: // a SocketClosing said not to reconnect
		case time.Since(readyAt) >= s.reconnectNow:
			failures = 0
		default:
			failures++
			wait = s.jittered(backoff.Doubling(s.retryBase, s.retryMax, failures))
		}
	}
}

// failed takes the failure err of a connect of region and says how long to wait before the next and why it is made; ok false means
// the region stops (the credentials were rejected, or the run takes no more).
func (s *sockets) failed(ctx context.Context, region string, err error, failures *int, report func(regionEvent) bool) (wait time.Duration, reason string, ok bool) {
	*failures++
	if credentialsRejected(err) {
		s.log.ErrorContext(ctx, "teams: the bot's credentials were rejected for Socket mode", "region", region, "error", err.Error())
		report(regionEvent{region: region, kind: regionRejected, err: err})
		return 0, "", false
	}
	wait, reason = s.retryWait(err, *failures)
	if reason == reasonRefused {
		s.log.WarnContext(ctx, "teams: a region refused the bot's socket; the other regions take its activities if they are ready",
			"region", region, "error", err.Error(), "after", wait)
		return wait, reason, report(regionEvent{region: region, kind: regionRefused, err: err})
	}
	s.log.WarnContext(ctx, "teams: a socket could not be opened; connecting again", "region", region, "error", err.Error(), "after", wait)
	return wait, reason, report(regionEvent{region: region, kind: regionFailed, err: err})
}

// retryWait is how long a region waits after the failure err, the failures-th in a row, of a connect or a replacement, and the reason
// of the next: a 403 waits refusedRetry, a 429 its Retry-After if that is longer than the backoff (up to socketRetryAfterMax), and the
// rest the backoff.
func (s *sockets) retryWait(err error, failures int) (time.Duration, string) {
	backoffWait := s.jittered(backoff.Doubling(s.retryBase, s.retryMax, failures))
	var se *SocketError
	switch {
	case errors.As(err, &se) && se.Status == http.StatusForbidden:
		return s.jittered(s.refusedRetry), reasonRefused
	case errors.As(err, &se) && se.Status == http.StatusTooManyRequests:
		return min(max(se.RetryAfter, backoffWait), socketRetryAfterMax), reasonRateLimited
	case errors.As(err, &se) && se.Status >= 500:
		return backoffWait, reasonUnavailable
	case errors.Is(err, errReadyTimeout):
		return backoffWait, reasonReadyTimeout
	default:
		return backoffWait, reasonFailed
	}
}

// hold keeps sk, the ready socket of region, and the ones that replace it, until one ends: it returns that socket (being retired in
// the background, which retiring waits for), why its region connects again and how long it waits first. When ctx ends, or a
// replacement finds the credentials rejected, it closes the socket and returns nil.
func (s *sockets) hold(ctx context.Context, region string, sk *socket, retiring *sync.WaitGroup, report func(regionEvent) bool) (*socket, string, time.Duration) {
	replaceAt := sk.replaceAt()
	tries := 0
	for {
		timer := time.NewTimer(time.Until(replaceAt))
		select {
		case <-ctx.Done():
			timer.Stop()
			s.retire(ctx, sk, 0)
			return nil, "", 0
		case <-sk.over:
			timer.Stop()
			retiring.Go(func() { s.retire(ctx, sk, 0) })
			reason, reconnect := sk.why()
			s.log.WarnContext(ctx, "teams: a socket ended; connecting again", "region", region, "reason", reason, "reconnect", reconnect)
			if !reconnect {
				return sk, reason, s.jittered(s.refusedRetry)
			}
			return sk, reason, 0
		case <-timer.C:
		}
		telemetry.Count(ctx, "agentkourier_teams_socket_reconnects_total", region, reasonExpiry, s.conn)
		next, err := s.open(ctx, region, sk.id)
		if err != nil {
			if ctx.Err() != nil {
				s.retire(ctx, sk, 0)
				return nil, "", 0
			}
			if credentialsRejected(err) {
				s.log.ErrorContext(ctx, "teams: the bot's credentials were rejected for Socket mode", "region", region, "error", err.Error())
				report(regionEvent{region: region, kind: regionRejected, err: err})
				s.retire(ctx, sk, 0)
				return nil, "", 0
			}
			tries++
			// The old socket serves until its token expires; then the service closes it, and the region connects as after any end. The
			// next try waits as a connect would after the same failure: a 429's Retry-After, a 403's slow wait.
			wait, _ := s.retryWait(err, tries)
			replaceAt = time.Now().Add(wait)
			s.log.WarnContext(ctx, "teams: a socket could not be replaced; the old one serves until it ends", "region", region, "error", err.Error(), "after", wait)
			continue
		}
		old := sk
		retiring.Go(func() { s.retire(ctx, old, s.retireAfter) })
		s.log.DebugContext(ctx, "teams: socket replaced before its token expired", "region", region)
		sk, replaceAt, tries = next, next.replaceAt(), 0
	}
}

// credentialsRejected says whether err refuses the bot's credentials: a connect call answered 401, or Microsoft Entra's refusal of the
// bot's token. No retry mends it.
func credentialsRejected(err error) bool {
	var se *SocketError
	var te *TokenRequestError
	return errors.As(err, &se) && se.Status == http.StatusUnauthorized || errors.As(err, &te) && te.rejected() && !te.Graph
}

// jittered is d lengthened or cut at random by up to a fifth.
func (s *sockets) jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*min(max(s.jitter(), 0), 1)))
}

// retire closes sk after grace, or once it has ended, and once the envelopes it is handling are answered, waiting at most retireMax for
// them, or stopWait once ctx has ended.
func (s *sockets) retire(ctx context.Context, sk *socket, grace time.Duration) {
	if grace > 0 {
		t := time.NewTimer(grace)
		select {
		case <-t.C:
		case <-ctx.Done():
		case <-sk.done:
		}
		t.Stop()
	}
	bound := s.retireMax
	if ctx.Err() != nil {
		bound = s.stopWait
	}
	answered := make(chan struct{})
	go func() {
		sk.inflight.Wait() // each envelope's context ends by its deadline, so this ends
		close(answered)
	}()
	t := time.NewTimer(bound)
	select {
	case <-answered:
	case <-t.C:
	}
	t.Stop()
	sk.close()
	<-sk.done
}

// socket is one open socket of a region.
type socket struct {
	region  string
	id      string // the connection ID that SocketReady named, which a replacement names
	ws      *websocket.Conn
	expires time.Time
	life    time.Duration

	wmu       sync.Mutex // one writer at a time
	ready     chan string
	over      chan struct{} // closed once the socket is done for: it ended, or the service said it is closing
	overOnce  sync.Once
	done      chan struct{} // closed once the read loop has returned and the connection is closed
	closeOnce sync.Once
	inflight  sync.WaitGroup

	mu        sync.Mutex
	reason    string // why the socket is over; guarded by mu
	reconnect bool   // whether the region may connect again at once; guarded by mu
}

// replaceAt is when the socket's region opens the next.
func (sk *socket) replaceAt() time.Time {
	return sk.expires.Add(-min(socketReplaceLead, sk.life/2))
}

// end marks the socket over, for reason, unless it already is.
func (sk *socket) end(reason string, reconnect bool) {
	sk.overOnce.Do(func() {
		sk.mu.Lock()
		sk.reason, sk.reconnect = reason, reconnect
		sk.mu.Unlock()
		close(sk.over)
	})
}

// why is the reason the socket is over, and whether its region may connect again at once.
func (sk *socket) why() (string, bool) {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	return sk.reason, sk.reconnect
}

// send writes one record of SignalR's protocol.
func (sk *socket) send(m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	sk.wmu.Lock()
	defer sk.wmu.Unlock()
	_ = sk.ws.SetWriteDeadline(time.Now().Add(socketWriteWait))
	return sk.ws.WriteMessage(websocket.TextMessage, append(b, recordSeparator))
}

// close sends a normal close and closes the connection, once.
func (sk *socket) close() {
	sk.closeOnce.Do(func() {
		// WriteControl may run beside a writer, so a write that the network holds does not hold the close.
		_ = sk.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = sk.ws.Close()
	})
}

// session is the connect call's answer.
type session struct {
	URL         string `json:"url"`
	AccessToken string `json:"accessToken"`
	ExpiresIn   int    `json:"expiresIn"`
}

// connect makes the connect call of region with the bot's token, naming replaces as the socket the new one replaces if it is set.
func (s *sockets) connect(ctx context.Context, region, replaces string) (session, error) {
	var sess session
	tok, err := s.tokens.token(ctx)
	if err != nil {
		return sess, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.connectURL(region), http.NoBody)
	if err != nil {
		return sess, fmt.Errorf("teams socket mode: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	if replaces != "" {
		req.Header.Set("x-ms-socket-replaces-connection-id", replaces)
	}
	data, status, header, err := s.post(req)
	if err != nil {
		return sess, fmt.Errorf("teams socket mode: the connect call of %s: %w", region, err)
	}
	if status != http.StatusOK {
		e := &SocketError{Region: region, Status: status, RetryAfter: retryAfter(header.Get("Retry-After"), time.Now())}
		var wire struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(data, &wire) == nil {
			var msg string
			if json.Unmarshal(wire.Error, &msg) == nil {
				e.Message = quote(msg)
			}
		}
		return sess, e
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		return sess, fmt.Errorf("teams socket mode: the connect call of %s answered no session: %w", region, err)
	}
	if sess.AccessToken == "" || !isHTTPS(sess.URL) {
		return sess, fmt.Errorf("teams socket mode: the connect call of %s answered no https URL or no token", region)
	}
	if sess.ExpiresIn <= 0 { // a socket with no life would be replaced as soon as it is ready, again and again
		return sess, fmt.Errorf("teams socket mode: the connect call of %s answered an expiresIn of %d seconds", region, sess.ExpiresIn)
	}
	return sess, nil
}

// post makes req and returns at most socketMaxBody of its answer, its status and its header. An error repeats no URL, whose query a
// service may put a token in.
func (s *sockets) post(req *http.Request) ([]byte, int, http.Header, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, socketMaxBody))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("reading the answer: %w", err)
	}
	return data, resp.StatusCode, resp.Header, nil
}

// isHTTPS says whether raw is an https URL with a host: no token is sent to anything else.
func isHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// negotiateURL is raw with /negotiate before its query and negotiateVersion=1 in it, as SignalR's clients make it.
func negotiateURL(raw string) string {
	path, query, _ := strings.Cut(raw, "?")
	u := strings.TrimSuffix(path, "/") + "/negotiate?"
	if query != "" {
		u += query + "&"
	}
	return u + "negotiateVersion=1"
}

// negotiate runs SignalR's negotiate at raw with token, following a redirect if one is answered (SignalR's protocol has them; the
// service measured answered none), and returns the WebSocket's URL and the token it takes.
func (s *sockets) negotiate(ctx context.Context, raw, token string) (string, string, error) {
	for range socketMaxRedirects {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, negotiateURL(raw), http.NoBody)
		if err != nil {
			return "", "", fmt.Errorf("teams socket mode: negotiate: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		data, status, _, err := s.post(req)
		if err != nil {
			return "", "", fmt.Errorf("teams socket mode: negotiate: %w", err)
		}
		if status != http.StatusOK {
			return "", "", fmt.Errorf("teams socket mode: negotiate answered %d", status)
		}
		var n struct {
			URL             string `json:"url"`
			AccessToken     string `json:"accessToken"`
			ConnectionID    string `json:"connectionId"`
			ConnectionToken string `json:"connectionToken"`
			NegotiateVer    int    `json:"negotiateVersion"`
			Error           string `json:"error"`
			Transports      []struct {
				Transport string `json:"transport"`
			} `json:"availableTransports"`
		}
		if err := json.Unmarshal(data, &n); err != nil {
			return "", "", fmt.Errorf("teams socket mode: negotiate answered no JSON: %w", err)
		}
		if n.Error != "" {
			return "", "", fmt.Errorf("teams socket mode: negotiate: %s", quote(n.Error))
		}
		if n.URL != "" { // a redirect to the service, with a token of its own
			if !isHTTPS(n.URL) || n.AccessToken == "" {
				return "", "", errors.New("teams socket mode: negotiate redirected to no https URL, or with no token")
			}
			raw, token = n.URL, n.AccessToken
			continue
		}
		ws := false
		for _, t := range n.Transports {
			ws = ws || t.Transport == "WebSockets"
		}
		if !ws {
			return "", "", errors.New("teams socket mode: negotiate offers no WebSockets")
		}
		id := n.ConnectionToken
		if n.NegotiateVer == 0 {
			id = n.ConnectionID
		}
		sep := "?"
		if strings.Contains(raw, "?") {
			sep = "&"
		}
		return "wss" + strings.TrimPrefix(raw, "https") + sep + "id=" + url.QueryEscape(id), token, nil
	}
	return "", "", errors.New("teams socket mode: negotiate redirected too many times")
}

// hubMessage is a record of SignalR's JSON protocol: an invocation (1), a completion (3), a ping (6) or a close (7).
type hubMessage struct {
	Type           int               `json:"type"`
	InvocationID   string            `json:"invocationId,omitempty"`
	Target         string            `json:"target,omitempty"`
	Arguments      []json.RawMessage `json:"arguments,omitempty"`
	Result         any               `json:"result,omitempty"`
	Error          string            `json:"error,omitempty"`
	AllowReconnect bool              `json:"allowReconnect,omitempty"`
}

// open opens a socket of region, replacing the socket replaces if it is set, and returns it once the service has called SocketReady.
func (s *sockets) open(ctx context.Context, region, replaces string) (*socket, error) {
	sess, err := s.connect(ctx, region, replaces)
	if err != nil {
		return nil, err
	}
	issued := time.Now()
	wsURL, token, err := s.negotiate(ctx, sess.URL, sess.AccessToken)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(s.readyWait)
	dctx, cancel := context.WithDeadline(ctx, deadline)
	ws, resp, err := s.dialer.DialContext(dctx, wsURL, http.Header{"Authorization": {"Bearer " + token}})
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w: the WebSocket did not open: %w", errReadyTimeout, err)
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("teams socket mode: the WebSocket of %s: %w", region, err)
	}
	// Until open returns, the end of ctx closes the connection, whatever it waits for: the handshake's write and read, and SocketReady.
	// A run told to stop opens no socket after it, so none of a stopped run becomes the bot's newest and takes its traffic.
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	ws.SetReadLimit(socketMaxFrame)
	_ = ws.SetReadDeadline(deadline)
	_ = ws.SetWriteDeadline(deadline)
	life := time.Duration(sess.ExpiresIn) * time.Second
	sk := &socket{region: region, ws: ws, life: life, expires: issued.Add(life), ready: make(chan string, 1),
		over: make(chan struct{}), done: make(chan struct{})}
	pending, err := sk.handshake()
	if err != nil {
		_ = ws.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("teams socket mode: the handshake of %s: %w", region, err)
	}
	go s.read(ctx, sk, pending)
	go s.ping(sk)
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case id := <-sk.ready:
		if ctx.Err() != nil { // the select may take ready over a ctx that ended too
			sk.close()
			<-sk.done
			return nil, ctx.Err()
		}
		sk.id = id
		return sk, nil
	case <-sk.over:
		reason, _ := sk.why()
		sk.close()
		<-sk.done
		return nil, fmt.Errorf("teams socket mode: the socket of %s ended before SocketReady (%s)", region, reason)
	case <-t.C:
		sk.close()
		<-sk.done
		return nil, errReadyTimeout
	case <-ctx.Done():
		sk.close()
		<-sk.done
		return nil, ctx.Err()
	}
}

// handshake makes SignalR's handshake: the JSON protocol, version 1, answered by an empty record. It returns what followed the answer
// in its message, the records that the read loop takes first.
func (sk *socket) handshake() ([]byte, error) {
	if err := sk.ws.WriteMessage(websocket.TextMessage, append([]byte(`{"protocol":"json","version":1}`), recordSeparator)); err != nil {
		return nil, err
	}
	_, msg, err := sk.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	first, rest, _ := bytes.Cut(msg, []byte{recordSeparator})
	var hs struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(first, &hs); err != nil {
		return nil, fmt.Errorf("the answer is no JSON: %w", err)
	}
	if hs.Error != "" {
		return nil, fmt.Errorf("refused: %s", quote(hs.Error))
	}
	return rest, nil
}

// ping sends SignalR's ping every pingEvery until the socket is done.
func (s *sockets) ping(sk *socket) {
	t := time.NewTicker(s.pingEvery)
	defer t.Stop()
	for {
		select {
		case <-sk.done:
			return
		case <-t.C:
			if err := sk.send(hubMessage{Type: 6}); err != nil {
				sk.end(reasonClosed, true)
				sk.close()
				return
			}
		}
	}
}

// read takes the records of pending, then reads the socket's messages until it ends, and closes it then. Every message moves the
// deadline of the next to serverTimeout on.
func (s *sockets) read(ctx context.Context, sk *socket, pending []byte) {
	defer close(sk.done)
	defer func() { _ = sk.ws.Close() }()
	for _, rec := range bytes.Split(pending, []byte{recordSeparator}) {
		if len(rec) > 0 && !s.record(ctx, sk, rec) {
			return
		}
	}
	for {
		_, msg, err := sk.ws.ReadMessage()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				sk.end(reasonServerTimeout, true)
			} else {
				sk.end(reasonClosed, true)
			}
			return
		}
		_ = sk.ws.SetReadDeadline(time.Now().Add(s.serverTimeout))
		for _, rec := range bytes.Split(msg, []byte{recordSeparator}) {
			if len(rec) > 0 && !s.record(ctx, sk, rec) {
				return
			}
		}
	}
}

// record handles one record, and says false when it closes the socket.
func (s *sockets) record(ctx context.Context, sk *socket, rec []byte) bool {
	var m hubMessage
	if err := json.Unmarshal(rec, &m); err != nil {
		s.log.DebugContext(ctx, "teams: a socket's record is no JSON; ignored", "region", sk.region, "bytes", len(rec))
		return true
	}
	switch m.Type {
	case 1:
		var arg json.RawMessage
		if len(m.Arguments) > 0 {
			arg = m.Arguments[0]
		}
		switch m.Target {
		case "Activity":
			s.dispatch(ctx, sk, m.InvocationID, arg)
			return true
		case "SocketReady":
			var f struct {
				ConnectionID string `json:"connectionId"`
			}
			_ = json.Unmarshal(arg, &f)
			select {
			case sk.ready <- f.ConnectionID:
			default:
			}
		case "SocketClosing":
			var f struct {
				Code      string `json:"code"`
				Reconnect bool   `json:"reconnect"`
			}
			_ = json.Unmarshal(arg, &f)
			reason, ok := closingReasons[f.Code]
			if !ok {
				reason = reasonClosingOther
			}
			s.log.InfoContext(ctx, "teams: the service is closing a socket", "region", sk.region, "code", quote(f.Code), "reconnect", f.Reconnect)
			sk.end(reason, f.Reconnect)
		default:
			s.log.DebugContext(ctx, "teams: a socket's call of an unknown target; ignored", "region", sk.region, "target", quote(m.Target))
		}
		if m.InvocationID != "" { // a call that waits for a result, which none of these has
			_ = sk.send(hubMessage{Type: 3, InvocationID: m.InvocationID})
		}
	case 7:
		s.log.InfoContext(ctx, "teams: the service closed a socket", "region", sk.region, "error", quote(m.Error))
		sk.end(reasonClosed, true)
		return false
	}
	return true
}

// envelope is what an Activity call carries.
type envelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelopeId"`
	DeadlineMs int             `json:"deadlineMs"`
	Payload    json.RawMessage `json:"payload"`
}

// replyFrame is the result of an Activity call: the envelope's answer.
type replyFrame struct {
	ProtocolVersion int             `json:"protocolVersion"`
	EnvelopeID      string          `json:"envelopeId"`
	Status          int             `json:"status"`
	Body            json.RawMessage `json:"body,omitempty"`
	RecvAt          int64           `json:"recvAt"`
	TS              int64           `json:"ts"`
}

// dispatch handles an Activity call on a goroutine of its own, if one of the connection's places is free, and answers 503 at once if
// none is.
func (s *sockets) dispatch(ctx context.Context, sk *socket, invocationID string, arg json.RawMessage) {
	recvAt := time.Now()
	select {
	case s.slots <- struct{}{}:
	default:
		var env envelope
		_ = json.Unmarshal(arg, &env)
		s.recv.count(ctx, "agentkourier_teams_activities_shed_total", shedSocketBusy)
		s.reply(ctx, sk, invocationID, envelopeType(env.Type), env.EnvelopeID, recvAt,
			answer{status: http.StatusServiceUnavailable, outcome: envelopeBusy})
		return
	}
	sk.inflight.Add(1)
	s.handlers.Add(1)
	go func() {
		defer s.handlers.Done()
		defer sk.inflight.Done()
		defer func() { <-s.slots }()
		s.handle(ctx, sk, invocationID, arg, recvAt)
	}()
}

// envelopeType is the label of an envelope's type: activity, invoke or other.
func envelopeType(t string) string {
	if t == "activity" || t == "invoke" {
		return t
	}
	return "other"
}

// handle hands the envelope's activity to the receiver, within the envelope's deadline less socketReplyMargin (and invokeWait for
// an invoke), and sends its answer as the reply frame.
func (s *sockets) handle(ctx context.Context, sk *socket, invocationID string, arg json.RawMessage, recvAt time.Time) {
	var env envelope
	if err := json.Unmarshal(arg, &env); err != nil || envelopeType(env.Type) == "other" {
		s.log.DebugContext(ctx, "teams: an envelope of no type the adapter knows; answered 400", "region", sk.region)
		s.reply(ctx, sk, invocationID, envelopeType(env.Type), env.EnvelopeID, recvAt,
			answer{status: http.StatusBadRequest, outcome: requestMalformed})
		return
	}
	deadline := socketDefaultDeadline
	if env.DeadlineMs > 0 {
		deadline = time.Duration(env.DeadlineMs) * time.Millisecond
	}
	wait := deadline - socketReplyMargin
	if env.Type == "invoke" {
		wait = min(wait, s.invokeWait)
	}
	ectx, cancel := context.WithDeadline(ctx, recvAt.Add(max(wait, 0)))
	defer cancel()
	s.reply(ctx, sk, invocationID, env.Type, env.EnvelopeID, recvAt, s.recv.takePayload(ectx, env.Payload))
}

// reply sends ans as the reply frame of the Activity call invocationID, and counts the envelope by its type and outcome. An invoke's
// body goes in the frame when it is JSON.
func (s *sockets) reply(ctx context.Context, sk *socket, invocationID, typ, envelopeID string, recvAt time.Time, ans answer) {
	outcome := ans.outcome
	if invocationID == "" {
		s.log.DebugContext(ctx, "teams: an Activity call with no invocation ID; it has no reply", "region", sk.region)
	} else {
		f := replyFrame{ProtocolVersion: 1, EnvelopeID: envelopeID, Status: ans.status, RecvAt: recvAt.UnixMilli(), TS: time.Now().UnixMilli()}
		if typ == "invoke" && json.Valid(ans.body) {
			f.Body = ans.body
		}
		if err := sk.send(hubMessage{Type: 3, InvocationID: invocationID, Result: f}); err != nil {
			outcome = envelopeUnanswered
			s.log.WarnContext(ctx, "teams: an envelope's reply frame could not be sent", "region", sk.region, "type", typ, "error", err.Error())
		}
	}
	telemetry.Count(ctx, "agentkourier_teams_socket_envelopes_total", typ, outcome, s.conn)
}
