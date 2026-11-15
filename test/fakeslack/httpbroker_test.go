package fakeslack_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// recordingSink is the broker's side of the Slack adapter: it keeps every message and press the adapter hands it.
type recordingSink struct {
	mu       sync.Mutex
	messages []slack.Message
	presses  []slack.Interaction
}

func (s *recordingSink) Message(_ context.Context, m slack.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
	return nil
}

func (s *recordingSink) Interaction(_ context.Context, i slack.Interaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presses = append(s.presses, i)
	return nil
}

func (s *recordingSink) seen() ([]slack.Message, []slack.Interaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slack.Message(nil), s.messages...), append([]slack.Interaction(nil), s.presses...)
}

// TestTheSlackAdapterInHTTPModeTakesWhatTheFakeDelivers runs the broker's own Slack adapter in HTTP mode behind a listener, and the
// fake delivering to it: what the fake signs and sends is what the adapter verifies and decodes, and the adapter's 200 is the fake's ack.
func TestTheSlackAdapterInHTTPModeTakesWhatTheFakeDelivers(t *testing.T) {
	leakCheck(t)
	var handler atomic.Pointer[http.Handler]
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := handler.Load()
		if h == nil {
			http.Error(w, "not yet", http.StatusServiceUnavailable)
			return
		}
		(*h).ServeHTTP(w, r)
	}))
	t.Cleanup(listener.Close)
	fake := fakeslack.New(fakeslack.WithRequestURL(listener.URL, testSigningSecret), fakeslack.WithHTTPRetries(20*time.Millisecond, 50*time.Millisecond, 100*time.Millisecond))
	t.Cleanup(fake.Close)

	up := make(chan bool, 4)
	client := slack.New(slack.Config{
		BotToken: secret.New(testToken), SigningSecret: secret.New(testSigningSecret), APIURL: fake.URL() + "/",
		Logger: testutil.DiscardLogger(), OnConnection: func(c bool) { up <- c },
	})
	h, ok := client.HTTPHandler()
	if !ok {
		t.Fatal("the adapter has no HTTP handler with a signing secret")
	}
	handler.Store(&h)
	if err := fake.VerifyURL(waitCtx(t), "AFAKEAPP"); err != nil {
		t.Fatalf("VerifyURL against the adapter = %v; want its challenge echoed", err)
	}

	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, sink) }()
	t.Cleanup(func() { cancel(); _ = <-done }) // Run ends with the context's error
	select {
	case <-up:
	case <-time.After(10 * time.Second):
		t.Fatal("the adapter's Run did not start taking requests")
	}

	m, err := fake.Mention("C1", "U1", "<@UFAKEBOT> hello")
	if err != nil {
		t.Fatal(err)
	}
	waitAcked(t, fake, m.Envelopes...)
	_, ask := postAsk(t, fake) // the bot's own posts come back to it as events, which it drops
	press, err := fake.Click("U1", ask, "approve", "yes")
	if err != nil {
		t.Fatal(err)
	}
	waitAcked(t, fake, press)

	messages, presses := sink.seen()
	if len(messages) != 2 || messages[0].TS != m.TS || !messages[0].Mentioned || messages[0].User != "U1" || messages[1].TS != m.TS {
		t.Errorf("the sink got %+v; want the app_mention and the message event of %s, and none of the bot's own posts", messages, m.TS)
	}
	if len(presses) != 1 || presses[0].MessageTS != ask || presses[0].ActionID != "approve" || presses[0].Value != "yes" || presses[0].User != "U1" {
		t.Errorf("the sink got the presses %+v; want the press of approve on %s", presses, ask)
	}
}
