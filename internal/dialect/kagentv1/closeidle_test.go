package kagentv1

import (
	"net/http"
	"testing"
)

// idleSpy is a transport that counts the requests to close its idle connections.
type idleSpy struct{ closed int }

func (*idleSpy) RoundTrip(*http.Request) (*http.Response, error) { return nil, http.ErrNotSupported }
func (s *idleSpy) CloseIdleConnections()                         { s.closed++ }

func TestCloseIdleConnectionsReachesTheSharedTransport(t *testing.T) {
	spy := &idleSpy{}
	(&Dialect{base: spy}).CloseIdleConnections()
	if spy.closed != 1 {
		t.Errorf("the shared transport was asked to close idle connections %d times, want 1", spy.closed)
	}
}

func TestCloseIdleConnectionsToleratesAZeroDialectAndAPlainTransport(*testing.T) {
	(&Dialect{}).CloseIdleConnections() // built without New: nothing to close
	plain := roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })
	(&Dialect{base: plain}).CloseIdleConnections() // a transport with no idle connections
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
