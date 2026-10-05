package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Signaler drives the container signaler of the fast tier (test/signaler), which freezes, resumes or kills the processes of
// one container by its container ID: what a paused node, a resumed one and a crash do to a replica. Its surface has no
// authentication and can end any process of the node, so this client is only for tests inside the cluster, and the
// signaler's Service is never published.
type Signaler struct{ c *client }

// NewSignaler returns a client for the signaler at baseURL.
func NewSignaler(baseURL string) *Signaler { return &Signaler{newClient(baseURL)} }

// Ready says whether a signaler answers at baseURL: any HTTP answer will do, since it signals nothing without a container ID, and a
// refused connection or a name that does not resolve (a tier brought up without it) is the error.
func (s *Signaler) Ready(ctx context.Context) error {
	var se *StatusError
	if err := s.c.do(ctx, http.MethodGet, "/", nil, nil); err != nil && !errors.As(err, &se) {
		return fmt.Errorf("e2e: the signaler does not answer: %w", err)
	}
	return nil
}

// Stop sends SIGSTOP to every process of the container run, which is then frozen: it neither runs nor answers, and its
// connections stay open, as a long pause leaves them. Resume it with Cont.
func (s *Signaler) Stop(ctx context.Context, container string) error {
	return s.signal(ctx, "stop", container)
}

// Cont sends SIGCONT, which resumes a container that Stop froze.
func (s *Signaler) Cont(ctx context.Context, container string) error {
	return s.signal(ctx, "cont", container)
}

// Kill sends SIGKILL: the container's process ends at once, with no shutdown, and the kubelet starts the container again
// in the same pod.
func (s *Signaler) Kill(ctx context.Context, container string) error {
	return s.signal(ctx, "kill", container)
}

func (s *Signaler) signal(ctx context.Context, verb, container string) error {
	if err := s.c.do(ctx, http.MethodPost, "/"+verb+"?"+url.Values{"container": {container}}.Encode(), nil, nil); err != nil {
		return fmt.Errorf("e2e: signaler %s %s: %w", verb, container, err)
	}
	return nil
}
