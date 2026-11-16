package web_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
)

// fakeKey is the test profile's secret: a fake constant.
const fakeKey = "fake-web-secret"

// headerKey carries the test profile's "signature": the secret itself, which is no signature at all, and is enough to test the
// platform, which only asks the profile whether a request passes.
const headerKey = "X-Test-Key"

// testProfile is a profile of the platform's tests: a request passes Verify when it carries the secret in headerKey, and its body is
// a testEvent in JSON. It sends replies when it has an encoder.
type testProfile struct {
	caps    chat.Caps
	replies bool
	maxBody int

	mu      sync.Mutex
	encoded []web.Reply // what Encode was given
	encErr  error       // what Encode says, if set
}

// testEvent is the test profile's body.
type testEvent struct {
	Channel, Thread, ID, User, Text string
	Mentioned, Ignore               bool
}

func newTestProfile(replies bool) *testProfile {
	return &testProfile{caps: chat.Caps{MaxText: 1000}, replies: replies, maxBody: 4096}
}

func (p *testProfile) Name() v1alpha1.WebProfile { return v1alpha1.WebProfileKourierV1 }

func (p *testProfile) Verify(h http.Header, _ []byte, _ time.Time) error {
	if h.Get(headerKey) != fakeKey {
		return errors.New("test: no key")
	}
	return nil
}

func (p *testProfile) Decode(_ http.Header, body []byte) (web.Event, bool, error) {
	if len(body) > p.maxBody {
		return web.Event{}, false, web.ErrTooLarge
	}
	var e testEvent
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return web.Event{}, false, errors.New("test: not a test event")
	}
	if e.Ignore {
		return web.Event{}, false, nil
	}
	return web.Event{Channel: e.Channel, Thread: e.Thread, ID: e.ID, User: e.User, Text: e.Text, Mentioned: e.Mentioned}, true, nil
}

func (p *testProfile) Caps() chat.Caps { return p.caps }

func (p *testProfile) Encoder() web.Encoder {
	if !p.replies {
		return nil
	}
	return p
}

// Encode sends the reply as JSON, with its key and kind as headers.
func (p *testProfile) Encode(r web.Reply, _ time.Time) (web.Request, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.encoded = append(p.encoded, r)
	if p.encErr != nil {
		return web.Request{}, p.encErr
	}
	body, err := json.Marshal(r.Message)
	if err != nil {
		return web.Request{}, err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Test-Kind", string(r.Kind))
	h.Set("Idempotency-Key", r.Key)
	return web.Request{Header: h, Body: body}, nil
}

// taken returns, and forgets, what Encode was given.
func (p *testProfile) taken() []web.Reply {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.encoded
	p.encoded = nil
	return out
}
