// Package faketeams fakes, for tests, the part of Microsoft's services that a Microsoft Teams connection in socket mode talks to:
// the connect call of each region, Azure SignalR's negotiate and its WebSocket, and enough of SignalR's JSON hub protocol to call
// SocketReady, Activity and SocketClosing on the client and take its completions. It follows what was measured against the real
// service in November 2026 (connect, negotiate, handshake, the envelope and its reply frame), not more: what the real service does past
// that is not here. As measured, the connect call's URL is the SignalR service's own, whose negotiate answers a connection token at
// once; Redirect makes it answer a redirect first instead, which SignalR's protocol has and the service was not seen to answer.
package faketeams

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The fake's tokens. None of them is a real token.
const (
	// ConnectToken is the access token that the connect call issues, which the first negotiate must carry.
	ConnectToken = "fake-socket-connect-token"
	// ServiceToken is the token that a negotiate's redirect issues (see Redirect), which the next negotiate and the WebSocket must carry.
	ServiceToken = "fake-signalr-service-token" //nolint:gosec // G101: a fake's stand-in, no credential
)

// recordSeparator ends every message of SignalR's JSON protocol.
const recordSeparator = 0x1e

// Answer is what a region answers a connect call with when it does not accept it: the status, the Retry-After header if any,
// and the body.
type Answer struct {
	Status     int
	RetryAfter string
	Body       string
}

// Connect is one connect call the fake was made, as it came.
type Connect struct {
	Region        string
	Authorization string
	// Replaces is the x-ms-socket-replaces-connection-id header, or "".
	Replaces string
	// Status is what the fake answered.
	Status int
	// At is when it came.
	At time.Time
}

// SocketMode is the fake. Its zero value is not usable: New makes one, which serves until the test ends.
type SocketMode struct {
	srv *httptest.Server

	mu        sync.Mutex
	answers   map[string]Answer // by region: what a connect is answered with; none means 200
	silent    map[string]bool   // by region: its sockets never call SocketReady
	held      map[string]*Hold  // by region: its handshakes wait for the Hold's release
	redirect  bool              // the connect call's URL is a negotiate that redirects
	expiresIn int
	connects  []Connect
	sockets   []*Socket
	issued    map[string]*issue // the connection tokens issued, by token
	next      int
	changed   chan struct{} // closed and made anew at every change of connects or sockets
	wg        sync.WaitGroup
}

// issue is a connection token that a negotiate issued: the bearer token the negotiate carried, which its WebSocket must carry too, and
// whether a WebSocket took it.
type issue struct {
	bearer string
	used   bool
}

// Hold holds the handshakes of a region's sockets: each is read, and answered only once Release is called.
type Hold struct {
	// Held gets a value as each handshake is read and held.
	Held    chan struct{}
	release chan struct{}
	once    sync.Once
}

// Release lets the held handshakes, and the later ones, be answered.
func (h *Hold) Release() { h.once.Do(func() { close(h.release) }) }

// New starts the fake, over TLS, until the test ends. Its sockets' tokens expire after 600 seconds, as the real service's did.
func New(t testing.TB) *SocketMode {
	t.Helper()
	f := &SocketMode{answers: map[string]Answer{}, silent: map[string]bool{}, held: map[string]*Hold{}, expiresIn: 600,
		issued: map[string]*issue{}, changed: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{region}/v3/websockets/connect", f.connect)
	mux.HandleFunc("POST /client/negotiate", f.negotiateRedirect)
	mux.HandleFunc("POST /hub/negotiate", f.negotiate)
	mux.HandleFunc("GET /hub/", f.hub)
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(func() {
		f.mu.Lock()
		for _, h := range f.held {
			h.Release()
		}
		f.mu.Unlock()
		for _, s := range f.Sockets() {
			s.Drop()
		}
		f.srv.Close()
		f.wg.Wait()
	})
	return f
}

// URL is the connect endpoint of region, which a client's config names in place of https://<region>.botapi.skype.com/….
func (f *SocketMode) URL(region string) string {
	return f.srv.URL + "/" + region + "/v3/websockets/connect"
}

// Client is an HTTP client that trusts the fake's certificate.
func (f *SocketMode) Client() *http.Client { return f.srv.Client() }

// TLS is a TLS config that trusts the fake's certificate, for a WebSocket dialer.
func (f *SocketMode) TLS() *tls.Config {
	return f.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
}

// Answer makes the region answer each later connect call with a; the zero Answer makes it accept them again.
func (f *SocketMode) Answer(region string, a Answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.Status == 0 {
		delete(f.answers, region)
		return
	}
	f.answers[region] = a
}

// Silence makes the later sockets of region never call SocketReady, or, with silent false, call it again.
func (f *SocketMode) Silence(region string, silent bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.silent[region] = silent
}

// HoldHandshakes makes the later handshakes of region wait, read and unanswered, until the Hold it returns is released.
func (f *SocketMode) HoldHandshakes(region string) *Hold {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := &Hold{Held: make(chan struct{}, 16), release: make(chan struct{})}
	f.held[region] = h
	return h
}

// Redirect makes the later connect calls answer a URL whose negotiate redirects to the service, with a token of its own (ServiceToken),
// as SignalR's protocol allows; false, the default, answers the service's URL itself, as was measured.
func (f *SocketMode) Redirect(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redirect = on
}

// ExpireAfter makes the later connect calls say that their token expires after seconds.
func (f *SocketMode) ExpireAfter(seconds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expiresIn = seconds
}

// Connects is every connect call so far, in order.
func (f *SocketMode) Connects() []Connect {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Connect(nil), f.connects...)
}

// RegionConnects is the connect calls of region so far, in order.
func (f *SocketMode) RegionConnects(region string) []Connect {
	var out []Connect
	for _, c := range f.Connects() {
		if c.Region == region {
			out = append(out, c)
		}
	}
	return out
}

// Sockets is every socket that got past the handshake so far, in order.
func (f *SocketMode) Sockets() []*Socket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*Socket(nil), f.sockets...)
}

// RegionSockets is the sockets of region so far, in order.
func (f *SocketMode) RegionSockets(region string) []*Socket {
	var out []*Socket
	for _, s := range f.Sockets() {
		if s.Region == region {
			out = append(out, s)
		}
	}
	return out
}

// WaitSocket waits up to within for the region's socket number n (0 the first) to be past its handshake, and fails the test if it
// is not.
func (f *SocketMode) WaitSocket(t testing.TB, region string, n int, within time.Duration) *Socket {
	t.Helper()
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for {
		f.mu.Lock()
		changed := f.changed
		f.mu.Unlock()
		if socks := f.RegionSockets(region); len(socks) > n {
			return socks[n]
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("faketeams: the socket %d of %s did not open within %s (%d did)", n, region, within, len(f.RegionSockets(region)))
			return nil
		}
	}
}

// WaitConnects waits up to within for at least n connect calls of region, and returns them all.
func (f *SocketMode) WaitConnects(t testing.TB, region string, n int, within time.Duration) []Connect {
	t.Helper()
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for {
		f.mu.Lock()
		changed := f.changed
		f.mu.Unlock()
		if got := f.RegionConnects(region); len(got) >= n {
			return got
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("faketeams: %d connect calls of %s within %s; want at least %d", len(f.RegionConnects(region)), region, within, n)
			return nil
		}
	}
}

// notify wakes the waits. The caller holds mu.
func (f *SocketMode) notify() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *SocketMode) connect(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")
	f.mu.Lock()
	a, refused := f.answers[region]
	c := Connect{Region: region, Authorization: r.Header.Get("Authorization"), Replaces: r.Header.Get("x-ms-socket-replaces-connection-id"),
		Status: http.StatusOK, At: time.Now()}
	if refused {
		c.Status = a.Status
	}
	if !refused && (!strings.HasPrefix(c.Authorization, "Bearer ") || len(c.Authorization) == len("Bearer ")) {
		c.Status = http.StatusUnauthorized
	}
	expires, path := f.expiresIn, "/hub/"
	if f.redirect {
		path = "/client/"
	}
	f.connects = append(f.connects, c)
	f.notify()
	f.mu.Unlock()
	switch {
	case refused:
		if a.RetryAfter != "" {
			w.Header().Set("Retry-After", a.RetryAfter)
		}
		w.WriteHeader(a.Status)
		_, _ = w.Write([]byte(a.Body))
	case c.Status == http.StatusUnauthorized:
		http.Error(w, `{"error":"no bot token"}`, http.StatusUnauthorized)
	default:
		writeJSON(w, map[string]any{"url": f.srv.URL + path + "?hub=bothub&region=" + region, "accessToken": ConnectToken,
			"expiresIn": expires, "protocolVersion": 1})
	}
}

// negotiateRedirect is a negotiate that redirects (Redirect): it answers the service's URL and a token for it.
func (f *SocketMode) negotiateRedirect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+ConnectToken || r.URL.Query().Get("negotiateVersion") != "1" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"url": f.srv.URL + "/hub/?hub=bothub&region=" + r.URL.Query().Get("region"), "accessToken": ServiceToken})
}

// negotiate is the service's negotiate, which issues a connection token for one WebSocket. It takes the connect call's token, or, after
// a redirect, the redirect's.
func (f *SocketMode) negotiate(w http.ResponseWriter, r *http.Request) {
	bearer := r.Header.Get("Authorization")
	f.mu.Lock()
	want := "Bearer " + ConnectToken
	if f.redirect {
		want = "Bearer " + ServiceToken
	}
	f.mu.Unlock()
	if bearer != want || r.URL.Query().Get("negotiateVersion") != "1" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.next++
	n := f.next
	tok := "fake-connection-token-" + strconv.Itoa(n)
	f.issued[tok] = &issue{bearer: bearer}
	f.mu.Unlock()
	writeJSON(w, map[string]any{"negotiateVersion": 1, "connectionId": "fake-connection-" + strconv.Itoa(n), "connectionToken": tok,
		"availableTransports": []any{map[string]any{"transport": "WebSockets", "transferFormats": []string{"Text"}}}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// hub is the WebSocket: the handshake, SocketReady unless the region is silent, then the client's messages until it closes. A
// connection token is taken once.
func (f *SocketMode) hub(w http.ResponseWriter, r *http.Request) {
	tok, region := r.URL.Query().Get("id"), r.URL.Query().Get("region")
	f.mu.Lock()
	is := f.issued[tok]
	fresh := is != nil && !is.used && r.Header.Get("Authorization") == is.bearer
	if fresh {
		is.used = true
	}
	silent, hold := f.silent[region], f.held[region]
	f.mu.Unlock()
	if !fresh {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	up := websocket.Upgrader{}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	_, hs, err := ws.ReadMessage()
	if err != nil || string(bytes.TrimSuffix(hs, []byte{recordSeparator})) != `{"protocol":"json","version":1}` {
		_ = ws.Close()
		return
	}
	if hold != nil {
		hold.Held <- struct{}{}
		<-hold.release
	}
	s := &Socket{Region: region, ws: ws, results: map[string]chan Frame{}, closed: make(chan struct{})}
	if err := s.write([]byte("{}")); err != nil {
		_ = ws.Close()
		return
	}
	f.mu.Lock()
	f.next++
	s.ConnectionID = "fake-socket-" + strconv.Itoa(f.next)
	for i := len(f.connects) - 1; i >= 0; i-- {
		if f.connects[i].Region == region && f.connects[i].Status == http.StatusOK {
			s.Replaces = f.connects[i].Replaces
			break
		}
	}
	f.sockets = append(f.sockets, s)
	f.notify()
	f.mu.Unlock()
	if !silent {
		if err := s.invoke("SocketReady", "", map[string]any{"protocolVersion": 1, "botKey": "fake-bot", "connectionId": s.ConnectionID}); err == nil {
			s.mu.Lock()
			s.ready = true
			s.mu.Unlock()
		}
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		s.read()
	}()
}

// Envelope is what an Activity call carries.
type Envelope struct {
	Type        string          `json:"type"`
	EnvelopeID  string          `json:"envelopeId"`
	DeadlineMs  int             `json:"deadlineMs,omitempty"`
	AckRequired bool            `json:"ackRequired"`
	Payload     json.RawMessage `json:"payload"`
}

// Frame is the client's reply frame, the result of its completion of an Activity call.
type Frame struct {
	ProtocolVersion int             `json:"protocolVersion"`
	EnvelopeID      string          `json:"envelopeId"`
	Status          int             `json:"status"`
	Body            json.RawMessage `json:"body,omitempty"`
	RecvAt          int64           `json:"recvAt"`
	TS              int64           `json:"ts"`
}

// Socket is one WebSocket that got past the handshake.
type Socket struct {
	Region string
	// ConnectionID is the ID that SocketReady gave the socket, which a replacement names.
	ConnectionID string
	// Replaces is the connection ID that the socket's connect call named as the one it replaces, or "".
	Replaces string

	ws     *websocket.Conn
	wmu    sync.Mutex
	mu     sync.Mutex
	nextID int
	// results are the completions awaited, by invocation ID: an Activity call's channel gets its frame.
	results map[string]chan Frame
	pings   int
	ready   bool // SocketReady was sent
	// closedBy says whether the client's close frame ended the socket (and not the fake's Drop or a read error).
	closedBy bool
	closed   chan struct{}
}

type hubMessage struct {
	Type         int               `json:"type"`
	InvocationID string            `json:"invocationId,omitempty"`
	Target       string            `json:"target,omitempty"`
	Arguments    []json.RawMessage `json:"arguments,omitempty"`
	Result       json.RawMessage   `json:"result,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func (s *Socket) write(rec []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.ws.WriteMessage(websocket.TextMessage, append(rec, recordSeparator))
}

// invoke calls target on the client with arg, and with an invocation ID if id is not "".
func (s *Socket) invoke(target, id string, arg any) error {
	b, err := json.Marshal(arg)
	if err != nil {
		return err
	}
	m, err := json.Marshal(hubMessage{Type: 1, InvocationID: id, Target: target, Arguments: []json.RawMessage{b}})
	if err != nil {
		return err
	}
	return s.write(m)
}

// read takes the client's messages until the socket ends: completions go to their waits, pings are counted.
func (s *Socket) read() {
	defer close(s.closed)
	for {
		_, msg, err := s.ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.mu.Lock()
				s.closedBy = true
				s.mu.Unlock()
			}
			_ = s.ws.Close()
			return
		}
		for _, rec := range bytes.Split(msg, []byte{recordSeparator}) {
			var m hubMessage
			if len(rec) == 0 || json.Unmarshal(rec, &m) != nil {
				continue
			}
			s.mu.Lock()
			switch m.Type {
			case 3:
				if ch := s.results[m.InvocationID]; ch != nil {
					var fr Frame
					_ = json.Unmarshal(m.Result, &fr)
					select {
					case ch <- fr:
					default:
					}
				}
			case 6:
				s.pings++
			}
			s.mu.Unlock()
		}
	}
}

// Activity calls Activity on the client with env and returns the invocation ID whose completion Result waits for.
func (s *Socket) Activity(t testing.TB, env Envelope) string {
	t.Helper()
	s.mu.Lock()
	s.nextID++
	id := strconv.Itoa(s.nextID)
	s.results[id] = make(chan Frame, 1)
	s.mu.Unlock()
	if err := s.invoke("Activity", id, env); err != nil {
		t.Fatalf("faketeams: Activity on %s: %v", s.Region, err)
	}
	return id
}

// Result waits up to within for the reply frame of the Activity call id, and says false if none came.
func (s *Socket) Result(id string, within time.Duration) (Frame, bool) {
	s.mu.Lock()
	ch := s.results[id]
	s.mu.Unlock()
	if ch == nil {
		return Frame{}, false
	}
	t := time.NewTimer(within)
	defer t.Stop()
	select {
	case fr := <-ch:
		return fr, true
	case <-t.C:
		return Frame{}, false
	}
}

// Closing calls SocketClosing on the client with code and reconnect, as the service does before it closes a socket.
func (s *Socket) Closing(t testing.TB, code string, reconnect bool) {
	t.Helper()
	if err := s.invoke("SocketClosing", "", map[string]any{"code": code, "reconnect": reconnect}); err != nil {
		t.Fatalf("faketeams: SocketClosing on %s: %v", s.Region, err)
	}
}

// Ping sends SignalR's ping, as the service does every 15 seconds.
func (s *Socket) Ping() error { return s.write([]byte(`{"type":6}`)) }

// Drop ends the socket at the fake's side, with no close frame.
func (s *Socket) Drop() { _ = s.ws.NetConn().Close() }

// Closed is closed once the socket has ended, at either side.
func (s *Socket) Closed() <-chan struct{} { return s.closed }

// ClosedByClient says whether the client ended the socket with a close frame.
func (s *Socket) ClosedByClient() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedBy
}

// Ready says whether the fake sent the socket SocketReady.
func (s *Socket) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// Pings is how many SignalR pings the client has sent.
func (s *Socket) Pings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// String names the socket in a test's messages.
func (s *Socket) String() string { return fmt.Sprintf("%s socket %s", s.Region, s.ConnectionID) }
