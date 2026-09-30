package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agentkourier/agent-kourier/test/sink"
)

// maxLog is the most of the sink's log Requests reads. The log holds every request since the last
// Clear, each up to sink.MaxBody, so it outgrows the client's default limit long before it is large.
const maxLog = 64 << 20

// Sink reads the receiver that Alertmanager routes every test alert to, which is how a scenario
// sees that an alert still arrives while courier is down (D22).
type Sink struct{ c *client }

// NewSink returns a client for the sink at baseURL.
func NewSink(baseURL string) *Sink {
	c := newClient(baseURL)
	c.limit = maxLog
	return &Sink{c}
}

// Requests returns what the sink has recorded since the last Clear, oldest first. A log over 64 MiB
// is an error that says to Clear it.
func (s *Sink) Requests(ctx context.Context) ([]sink.Request, error) {
	var reqs []sink.Request
	if err := s.c.do(ctx, http.MethodGet, "/requests", nil, &reqs); errors.Is(err, errTooLarge) {
		return nil, fmt.Errorf("sink log exceeds %d bytes; Clear it: %w", s.c.limit, err)
	} else if err != nil {
		return nil, err
	}
	return reqs, nil
}

// Clear empties the sink's log. It is for the runner's setup, or for a scenario that runs alone:
// the log is shared, so a scenario that clears it would erase what a concurrent one is waiting
// for. Scenarios match on the names that Alertmanager.Name makes unique instead.
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
