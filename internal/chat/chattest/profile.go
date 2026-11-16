package chattest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
)

// ProfileSubject is a profile of the web platform under test (web.Profile), and what its sender does. Every profile's tests call
// ProfileConformance with one: it holds the profile to what the web platform counts on, through the platform's own handler.
type ProfileSubject struct {
	// Profile is the profile, made with Secret.
	Profile web.Profile
	// Secret is the secret the profile was made with: a fake constant. The suite signs with it, and with another to forge.
	Secret string
	// Sign returns the headers that sign body at the time at with secret, as the profile's sender signs a request. The suite takes
	// each header it returns away in turn, and expects the request refused without it.
	Sign func(secret string, body []byte, at time.Time) http.Header
	// MaxSkew is how far from the receiver's clock the time of a signature may be, either way, or 0 for a profile whose signature
	// carries no time: such a profile has no replay window, and a replayed delivery is dropped by its ID alone.
	MaxSkew time.Duration
	// MaxBody is the most bytes of a body the profile takes; a longer one is refused 413. The suite makes a body of the bound, and one
	// past it, by padding the first event that has something to take with trailing spaces, which a JSON profile reads as the same
	// document, so the first such event must be within the bound.
	MaxBody int
	// Events are the profile's documented events, every one, each with what it decodes to. At least one has something to take.
	Events []ProfileEvent
	// Malformed are bodies the profile's schema refuses, beyond the suite's own (an empty body, one that is not JSON, one cut short):
	// an unknown version, an unknown field, an ID of the wrong shape.
	Malformed []ProfileCase
	// CheckReply says whether a request of the profile's replies is one its receiver takes: signed with Secret, in the profile's
	// envelope. It is required of a profile that sends replies.
	CheckReply func(h http.Header, body []byte) error
}

// ProfileEvent is one documented event of a profile.
type ProfileEvent struct {
	Name string
	// Header holds what the sender sends beside the signature (GitHub's event name and delivery ID), and Body the body.
	Header http.Header
	Body   []byte
	// Want is what the event decodes to, or nil for an event that carries nothing to take, such as a ping.
	Want *web.Event
}

// ProfileCase is a request of a profile that is refused.
type ProfileCase struct {
	Name   string
	Header http.Header
	Body   []byte
}

// ProfileConformance fails t for every way the profile of s breaks what the web platform counts on. A profile's tests call it, and
// every profile must.
func ProfileConformance(t *testing.T, s ProfileSubject) {
	t.Helper()
	for _, v := range CheckProfile(t.Context(), s) {
		t.Error(v)
	}
}

// CheckProfile runs the whole of ProfileConformance and returns what it found, without failing anything. It checks, through the web
// platform's handler with the profile:
//
//   - that every documented event decodes to what it says, and reaches the Sink as that, or that nothing does;
//   - that a repeat of an event reaches the Sink with the same chat.MessageID, which the reply queue drops, and that no two events share
//     one;
//   - that a request is refused 401, with nothing handed on, when it is not signed, when any one of its signature's headers is missing,
//     when it is signed with another secret, when its body is not the one signed, and when the time of its signature is past the skew
//     either way, while one within it is taken;
//   - that a body that is empty, not JSON, cut short or of the subject's own malformed cases is refused 400, one past the bound 413 and
//     one of the bound taken, and a method other than POST 405;
//   - that a profile without replies makes no request, and Post, Update, Ephemeral and Fits say web.ErrNoReplies, and that a profile
//     with replies sends a post and an update to the connection's URL, each one its receiver takes (CheckReply).
func CheckProfile(ctx context.Context, s ProfileSubject) []Violation {
	first, ok := s.firstEvent()
	if v, bad := s.setup(first, ok); bad {
		return []Violation{v}
	}
	c := &profileChecker{ctx: ctx, s: s, first: first, sink: &recordingSink{}, sent: &recordingTransport{}}
	cfg := web.Config{Connection: profileConn, Profile: s.Profile, Transport: c.sent, Now: func() time.Time { return profileNow },
		Logger: slog.New(slog.DiscardHandler)}
	if s.Profile.Encoder() != nil {
		cfg.URL = profileURL
	}
	p, err := web.New(cfg)
	if err != nil {
		return []Violation{{Rule: RuleContract, Method: "New", Case: "config", Detail: fmt.Sprintf("the platform refused the profile: %v", err)}}
	}
	c.p = p
	if got, want := p.Capabilities(), s.Profile.Caps(); !reflect.DeepEqual(got, want) {
		c.add("Capabilities", "caps", "the platform's are %+v, want the profile's %+v", got, want)
	}
	if w := c.send(http.MethodPost, first.Header, first.Body, s.Sign(s.Secret, first.Body, profileNow)); w.Code != http.StatusServiceUnavailable {
		c.add("ServeHTTP", "before Run", "answered %d, want 503: no Run takes the event", w.Code)
	}
	stop, running := c.run(ctx)
	defer stop()
	if !running {
		return c.found
	}
	c.events()
	c.dedup()
	c.signatures()
	c.malformed()
	c.bounds()
	c.replies(ctx)
	return c.found
}

// The connection, URL and clock of the suite's platform.
const (
	profileConn = "conformance/web"
	profileURL  = "https://receiver.conformance.test/replies"
)

var profileNow = time.Unix(1_800_000_000, 0)

// firstEvent is the first event that has something to take, which the checks of signatures and bodies send.
func (s ProfileSubject) firstEvent() (ProfileEvent, bool) {
	for _, e := range s.Events {
		if e.Want != nil {
			return e, true
		}
	}
	return ProfileEvent{}, false
}

// setup says what the subject lacks, if anything.
func (s ProfileSubject) setup(first ProfileEvent, ok bool) (Violation, bool) {
	bad := func(detail string) (Violation, bool) {
		return Violation{Rule: RuleSetup, Case: "profile subject", Detail: detail}, true
	}
	switch {
	case s.Profile == nil || s.Sign == nil || s.Secret == "":
		return bad("Profile, Sign and Secret are all required")
	case s.MaxBody <= 0:
		return bad("MaxBody is required")
	case !ok:
		return bad("no event has something to take: the checks have nothing to send")
	case len(first.Body) > s.MaxBody:
		return bad("the first event to take is over MaxBody: the bound is made by padding it")
	case s.Profile.Encoder() != nil && s.CheckReply == nil:
		return bad("CheckReply is required of a profile that sends replies")
	}
	for _, e := range s.Events {
		if e.Name == "" {
			return bad("an event has no name")
		}
	}
	for _, m := range s.Malformed {
		if m.Name == "" {
			return bad("a malformed case has no name")
		}
	}
	return Violation{}, false
}

type profileChecker struct {
	ctx   context.Context
	s     ProfileSubject
	p     *web.Platform
	first ProfileEvent
	sink  *recordingSink
	sent  *recordingTransport
	found []Violation
}

func (c *profileChecker) add(method, cs, format string, args ...any) {
	c.found = append(c.found, Violation{Rule: RuleContract, Method: method, Case: cs, Detail: fmt.Sprintf(format, args...)})
}

// run starts the platform's Run on the suite's sink, and returns once it takes requests: the first event, sent until it is not
// answered 503, is then taken away from the sink.
func (c *profileChecker) run(ctx context.Context) (stop func(), running bool) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.p.Run(ctx, c.sink)
	}()
	stop = func() { cancel(); <-done }
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if w := c.signed(c.first.Header, c.first.Body); w.Code != http.StatusServiceUnavailable {
			c.sink.take()
			return stop, true
		}
	}
	c.add("Run", "running", "the platform took no request within 5s of Run")
	return stop, false
}

// send serves one request with the method, the headers h and sig (the signature's), and body.
func (c *profileChecker) send(method string, h http.Header, body []byte, sig http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(c.ctx, method, "/chat/web/"+profileConn, bytes.NewReader(body))
	for k, vs := range h {
		req.Header[k] = slices.Clone(vs)
	}
	for k, vs := range sig {
		req.Header[k] = slices.Clone(vs)
	}
	handler, _ := c.p.HTTPHandler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// signed sends body with h, signed now with the subject's secret.
func (c *profileChecker) signed(h http.Header, body []byte) *httptest.ResponseRecorder {
	return c.send(http.MethodPost, h, body, c.s.Sign(c.s.Secret, body, profileNow))
}

// inbound is what the Sink is given for ev.
func (c *profileChecker) inbound(ev web.Event) chat.Inbound {
	return chat.Inbound{At: c.p.RefAt(ev.Channel, ev.Thread, ev.ID), User: ev.User, Text: ev.Text, Mentioned: ev.Mentioned}
}

// events checks that every documented event decodes to what it says, and reaches the Sink as that.
func (c *profileChecker) events() {
	for _, e := range c.s.Events {
		ev, ok, err := c.s.Profile.Decode(e.Header, e.Body)
		switch {
		case err != nil:
			c.add("Decode", e.Name, "a documented event was refused: %v", err)
		case ok != (e.Want != nil):
			c.add("Decode", e.Name, "said there is something to take: %t, want %t", ok, e.Want != nil)
		case ok && ev != *e.Want:
			c.add("Decode", e.Name, "decoded %+v, want %+v", ev, *e.Want)
		}
		w := c.signed(e.Header, e.Body)
		got := c.sink.take()
		var want []chat.Inbound
		if e.Want != nil {
			want = []chat.Inbound{c.inbound(*e.Want)}
		}
		if w.Code != http.StatusAccepted {
			c.add("ServeHTTP", e.Name, "answered %d, want 202", w.Code)
		} else if !slices.Equal(got, want) {
			c.add("ServeHTTP", e.Name, "the Sink was given %+v, want %+v", got, want)
		}
	}
}

// dedup checks that each event to take reaches the Sink with the same MessageID each time it is delivered, and with one that no other
// event has: the reply queue drops a message whose ID it has taken.
func (c *profileChecker) dedup() {
	seen := map[string]string{}
	for _, e := range c.s.Events {
		if e.Want == nil {
			continue
		}
		c.signed(e.Header, e.Body)
		c.signed(e.Header, e.Body)
		got := c.sink.take()
		if len(got) != 2 {
			c.add("ServeHTTP", "repeat: "+e.Name, "two deliveries gave the Sink %d messages, want 2: the Sink drops a repeat, not the platform", len(got))
			continue
		}
		id := chat.MessageID(got[0].At)
		if again := chat.MessageID(got[1].At); again != id {
			c.add("Decode", "repeat: "+e.Name, "two deliveries of one event have the IDs %q and %q, want one: the repeat would be taken again", id, again)
		}
		if other, ok := seen[id]; ok {
			c.add("Decode", "distinct IDs", "%q and %q have the one ID %q: the second would be dropped as a repeat of the first", other, e.Name, id)
		}
		seen[id] = e.Name
	}
}

// refused checks that a request is answered code and that nothing reaches the Sink.
func (c *profileChecker) refused(method, cs string, w *httptest.ResponseRecorder, code int) {
	if w.Code != code {
		c.add(method, cs, "answered %d, want %d", w.Code, code)
	}
	if got := c.sink.take(); len(got) > 0 {
		c.add(method, cs, "the Sink was given %d messages, want none", len(got))
	}
}

// signatures checks that a request that is not signed, or not signed now with the secret over its body, is refused.
func (c *profileChecker) signatures() {
	e, s := c.first, c.s
	sig := s.Sign(s.Secret, e.Body, profileNow)
	c.refused("Verify", "unsigned", c.send(http.MethodPost, e.Header, e.Body, nil), http.StatusUnauthorized)
	for _, k := range slices.Sorted(maps.Keys(sig)) {
		without := sig.Clone()
		without.Del(k)
		c.refused("Verify", "without "+k, c.send(http.MethodPost, e.Header, e.Body, without), http.StatusUnauthorized)
	}
	c.refused("Verify", "another secret", c.send(http.MethodPost, e.Header, e.Body, s.Sign("another-"+s.Secret, e.Body, profileNow)), http.StatusUnauthorized)
	// A trailing space keeps the body the same document, so that only the signature can tell it from the one signed.
	c.refused("Verify", "another body", c.send(http.MethodPost, e.Header, append(slices.Clone(e.Body), ' '), sig), http.StatusUnauthorized)
	if s.MaxSkew <= 0 {
		return
	}
	at := func(t time.Time) *httptest.ResponseRecorder {
		return c.send(http.MethodPost, e.Header, e.Body, s.Sign(s.Secret, e.Body, t))
	}
	c.refused("Verify", "stale", at(profileNow.Add(-s.MaxSkew-time.Second)), http.StatusUnauthorized)
	c.refused("Verify", "from the future", at(profileNow.Add(s.MaxSkew+time.Second)), http.StatusUnauthorized)
	if w := at(profileNow.Add(-s.MaxSkew + time.Second)); w.Code != http.StatusAccepted {
		c.add("Verify", "within the skew", "a request signed a second within the skew answered %d, want 202", w.Code)
	}
	c.sink.take()
}

// malformed checks that a signed body that is not the profile's schema is refused 400.
func (c *profileChecker) malformed() {
	e := c.first
	cases := []ProfileCase{
		{Name: "empty", Header: e.Header},
		{Name: "not JSON", Header: e.Header, Body: []byte("not json")},
		{Name: "cut short", Header: e.Header, Body: e.Body[:len(e.Body)/2]},
	}
	for _, m := range append(cases, c.s.Malformed...) {
		c.refused("Decode", "malformed: "+m.Name, c.signed(m.Header, m.Body), http.StatusBadRequest)
	}
}

// bounds checks the body's bound, and the method.
func (c *profileChecker) bounds() {
	e := c.first
	pad := func(n int) []byte { return append(slices.Clone(e.Body), bytes.Repeat([]byte(" "), n-len(e.Body))...) }
	over := pad(c.s.MaxBody + 1)
	if _, _, err := c.s.Profile.Decode(e.Header, over); !errors.Is(err, web.ErrTooLarge) {
		c.add("Decode", "over the bound", "a body of %d bytes gave %v, want web.ErrTooLarge", len(over), err)
	}
	c.refused("Decode", "over the bound", c.signed(e.Header, over), http.StatusRequestEntityTooLarge)
	w := c.signed(e.Header, pad(c.s.MaxBody))
	if got := c.sink.take(); w.Code != http.StatusAccepted || !slices.Equal(got, []chat.Inbound{c.inbound(*e.Want)}) {
		c.add("Decode", "at the bound", "a body of %d bytes answered %d and gave the Sink %+v, want 202 and the event", c.s.MaxBody, w.Code, got)
	}
	c.refused("ServeHTTP", "GET", c.send(http.MethodGet, e.Header, e.Body, c.s.Sign(c.s.Secret, e.Body, profileNow)), http.StatusMethodNotAllowed)
}

// replies checks that a profile without replies sends nothing, and that one with replies sends what its receiver takes, to the
// connection's URL.
func (c *profileChecker) replies(ctx context.Context) {
	ev := *c.first.Want
	t := chat.Target{Conn: profileConn, Channel: ev.Channel, Thread: ev.ID}
	ref := c.p.RefAt(ev.Channel, ev.ID, "tag-conformance")
	msg := chat.Say(chat.Message{Fallback: "conformance", Parts: []chat.Part{chat.Body{Markdown: "a reply"}}})
	c.sent.take()
	if c.s.Profile.Encoder() == nil {
		_, post := c.p.Post(ctx, t, msg, "tag-conformance")
		for method, err := range map[string]error{
			"Post": post, "Update": c.p.Update(ctx, ref, msg), "Ephemeral": c.p.Ephemeral(ctx, t, ev.User, *msg.Message),
			"Fits": c.p.Fits(chat.Prompt{ID: "ix-conformance", Questions: []chat.Question{{Text: "q"}}}),
		} {
			if !errors.Is(err, web.ErrNoReplies) {
				c.add(method, "no replies", "gave %v, want web.ErrNoReplies", err)
			}
		}
		if n := len(c.sent.take()); n > 0 {
			c.add("Post", "no replies", "a profile without replies made %d requests, want none", n)
		}
		return
	}
	for _, call := range []struct {
		cs string
		do func() error
	}{
		{"reply", func() error { _, err := c.p.Post(ctx, t, msg, "tag-conformance"); return err }},
		{"update", func() error { return c.p.Update(ctx, ref, msg) }},
	} {
		if err := call.do(); err != nil {
			c.add("Post", call.cs, "failed: %v", err)
		}
		reqs := c.sent.take()
		if len(reqs) != 1 {
			c.add("Post", call.cs, "made %d requests, want 1", len(reqs))
			continue
		}
		r := reqs[0]
		if r.method != http.MethodPost || r.url != profileURL {
			c.add("Post", call.cs, "the request is %s %s, want POST to the connection's URL", r.method, r.url)
		}
		if err := c.s.CheckReply(r.header, r.body); err != nil {
			c.add("Post", call.cs, "the receiver would refuse it: %v", err)
		}
	}
}

// recordingSink is a chat.Sink that keeps the messages it is given.
type recordingSink struct {
	mu  sync.Mutex
	got []chat.Inbound
}

func (s *recordingSink) Message(_ context.Context, m chat.Inbound) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, m)
	return nil
}

func (s *recordingSink) Interaction(context.Context, chat.Interaction) error { return nil }

func (s *recordingSink) take() []chat.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.got
	s.got = nil
	return got
}

// sentRequest is a request the platform made.
type sentRequest struct {
	method, url string
	header      http.Header
	body        []byte
}

// recordingTransport keeps the requests it is given, and answers each 200.
type recordingTransport struct {
	mu  sync.Mutex
	got []sentRequest
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	rt.mu.Lock()
	rt.got = append(rt.got, sentRequest{method: req.Method, url: req.URL.String(), header: req.Header.Clone(), body: body})
	rt.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func (rt *recordingTransport) take() []sentRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	got := rt.got
	rt.got = nil
	return got
}
