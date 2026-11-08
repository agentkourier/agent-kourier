package fakeslack

import (
	"fmt"
	"net/http"
	"strings"
)

// postHold is the hold on one app's chat.postMessage calls.
type postHold struct {
	release chan struct{} // closed by ReleasePosts
	held    int           // calls waiting now, guarded by Server.mu
}

// HoldPosts holds the chat.postMessage calls of the app appID: from here each call waits, before the message is stored or any event is queued, until
// ReleasePosts (or until its client goes away, when the call is abandoned and the message is never stored, as for a request that never arrived). The
// app's other methods, and every other app, are not held. Holding an app that is held already changes nothing. It is for a test that needs a post to be
// certainly in flight: a leader killed while its post waits here leaves the row pending. It fails with ErrNotFound for an app the Server does not host.
//
// The holds are safe only because the e2e tests run one after another (none calls t.Parallel): a hold is the fake's, not a test's, so two tests that held the
// same app at once would release each other's.
//
// UNCONFIRMED(MC-11): that a call whose client has gone is never applied; Slack may apply a request that reached it.
func (s *Server) HoldPosts(appID string) error {
	if _, ok := s.appByID(appID); !ok {
		return fmt.Errorf("fakeslack: no app %q: %w", appID, ErrNotFound)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == nil {
		s.holds = map[string]*postHold{}
	}
	if s.holds[appID] == nil {
		s.holds[appID] = &postHold{release: make(chan struct{})}
	}
	return nil
}

// ReleasePosts ends the hold on the app's chat.postMessage calls: the calls that wait go on, in no order, and later calls are not held. An app that is not
// held is released already. It fails with ErrNotFound for an app the Server does not host.
func (s *Server) ReleasePosts(appID string) error {
	if _, ok := s.appByID(appID); !ok {
		return fmt.Errorf("fakeslack: no app %q: %w", appID, ErrNotFound)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.holds[appID]; h != nil {
		close(h.release)
		delete(s.holds, appID)
	}
	return nil
}

// HeldPosts returns how many chat.postMessage calls of the app wait on its hold now: a call whose client has gone is no longer counted.
func (s *Server) HeldPosts(appID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.holds[appID]; h != nil {
		return h.held
	}
	return 0
}

// releaseAllPosts ends every hold, for Close.
func (s *Server) releaseAllPosts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, h := range s.holds {
		close(h.release)
		delete(s.holds, id)
	}
}

// waitPostHold holds r, a request for a Web API method, if it is a chat.postMessage call of a held app that authenticates. It returns false when the
// client went away while the call waited, and then nothing is to be written or stored.
func (s *Server) waitPostHold(r *http.Request) bool {
	if strings.TrimPrefix(r.URL.Path, "/") != "chat.postMessage" {
		return true
	}
	s.mu.Lock()
	held := len(s.holds) > 0
	s.mu.Unlock()
	if !held || r.ParseForm() != nil {
		return true
	}
	app, code := s.authenticate(r, tokenPrefix("chat.postMessage"))
	if code != "" {
		return true // refused as it would be; nothing waits
	}
	s.mu.Lock()
	h := s.holds[app.AppID]
	if h == nil {
		s.mu.Unlock()
		return true
	}
	h.held++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		h.held--
		s.mu.Unlock()
	}()
	select {
	case <-h.release:
		return true
	case <-r.Context().Done():
		return false
	}
}
