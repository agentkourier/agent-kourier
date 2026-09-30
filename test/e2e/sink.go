package e2e

import (
	"context"
	"net/http"
	"time"

	"github.com/agentkourier/agent-kourier/test/sink"
)

// Sink reads the receiver that Alertmanager routes every test alert to, which is how a scenario
// sees that an alert still arrives while courier is down (D22).
type Sink struct{ c *client }

// NewSink returns a client for the sink at baseURL.
func NewSink(baseURL string) *Sink { return &Sink{newClient(baseURL)} }

// Requests returns what the sink has recorded, oldest first.
func (s *Sink) Requests(ctx context.Context) ([]sink.Request, error) {
	var reqs []sink.Request
	if err := s.c.do(ctx, http.MethodGet, "/requests", nil, &reqs); err != nil {
		return nil, err
	}
	return reqs, nil
}

// Clear empties the sink's log.
func (s *Sink) Clear(ctx context.Context) error {
	return s.c.do(ctx, http.MethodDelete, "/requests", nil, nil)
}

// WaitFor returns the first recorded request that match accepts, polling for up to timeout.
func (s *Sink) WaitFor(ctx context.Context, timeout time.Duration, match func(sink.Request) bool) (sink.Request, error) {
	var found sink.Request
	err := Wait(ctx, timeout, "a matching request at the sink", func(ctx context.Context) (bool, error) {
		reqs, err := s.Requests(ctx)
		if err != nil {
			return false, err
		}
		for _, r := range reqs {
			if match(r) {
				found = r
				return true, nil
			}
		}
		return false, nil
	})
	return found, err
}
