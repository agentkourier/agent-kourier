package chat

import (
	"errors"
	"fmt"
)

// ErrConnectionDown is wrapped by the error of a Resolver that has no platform for a connection: nothing runs it, because it
// is not served, it has not started yet, or it stopped. It is not a fault of the platform and no retry of the call mends
// it; the work that needs the connection waits for it to run (an outbox row stays pending) or fails fast (a stream, an edit),
// and a caller finds it with errors.Is.
var ErrConnectionDown = errors.New("chat: no platform is running the connection")

// Resolver gives the platform of a ChatConnection, "namespace/name", as the part that asks needs it: P is the part's own
// narrow view of a Platform, or Platform itself. The parts of the broker that every connection shares (the outbox, the
// renderer, the interactions) hold a Resolver and look the platform up at each row, turn or press, from the connection that
// the row, turn or press carries, so that a call reaches the app of its own connection and no other. A Platform stays the
// platform of one connection.
type Resolver[P any] interface {
	// Platform returns the platform that runs conn, or an error wrapping ErrConnectionDown if none does. A Resolver is safe for
	// concurrent use.
	Platform(conn string) (P, error)
}

// PlatformSet is a Resolver of a fixed set of platforms by connection: the broker's, while it runs one connection, and a
// test's. A connection not in it is down.
type PlatformSet[P any] map[string]P

// Platform implements Resolver.
func (s PlatformSet[P]) Platform(conn string) (P, error) {
	p, ok := s[conn]
	if !ok {
		var zero P
		return zero, fmt.Errorf("%w: %q", ErrConnectionDown, conn)
	}
	return p, nil
}

// Narrow views the platforms of r as P, one of the narrower interfaces a part asks of a Platform (the outbox's, the
// interactions'). A platform that is not a P is an error, which the part treats as the platform being unavailable; every
// platform a part is wired to is one, so it does not happen in a built broker.
func Narrow[P any](r Resolver[Platform]) Resolver[P] { return narrowed[P]{r} }

type narrowed[P any] struct{ r Resolver[Platform] }

func (n narrowed[P]) Platform(conn string) (P, error) {
	var zero P
	p, err := n.r.Platform(conn)
	if err != nil {
		return zero, err
	}
	v, ok := any(p).(P)
	if !ok {
		return zero, fmt.Errorf("chat: the platform of %q is a %T, which is not a %T", conn, p, &zero)
	}
	return v, nil
}
