package runtime

import (
	"context"
	"sync"
)

// keyLock is a lock for each key of K: the replay handler's claim of a message ID, and the context lock that the
// accept of a message and the upkeep's release of an idle context share. Its zero value is ready to use.
type keyLock[K comparable] struct {
	mu   sync.Mutex
	held map[K]chan struct{} // by key: closed when its holder unlocks it
}

// lock waits until no one holds key, then holds it, or returns ctx's error once ctx ends. The returned func unlocks
// key; it may be called more than once.
func (l *keyLock[K]) lock(ctx context.Context, key K) (func(), error) {
	for {
		l.mu.Lock()
		pending, ok := l.held[key]
		if !ok {
			done := make(chan struct{})
			if l.held == nil {
				l.held = map[K]chan struct{}{}
			}
			l.held[key] = done
			l.mu.Unlock()
			return sync.OnceFunc(func() {
				l.mu.Lock()
				delete(l.held, key)
				l.mu.Unlock()
				close(done)
			}), nil
		}
		l.mu.Unlock()
		select {
		case <-pending:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
