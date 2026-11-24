package runtime

import (
	"context"
	"errors"
	"testing"
)

// A key is held by one holder at a time; a waiter that gives up, and an unlock, leave no entry behind, and an unlock
// may run twice.
func TestKeyLock(t *testing.T) {
	var l keyLock[string]
	unlock, err := l.lock(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	other, err := l.lock(t.Context(), "other")
	if err != nil {
		t.Fatalf("another key: %v, want it held at once", err)
	}
	other()

	ctx, cancel := context.WithCancel(t.Context())
	waited := make(chan error, 1)
	go func() {
		_, err := l.lock(ctx, "k")
		waited <- err
	}()
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("a waiter whose context ended = %v, want canceled", err)
	}
	if n := held(&l); n != 1 {
		t.Fatalf("held %d keys after the waiter gave up, want the holder's alone", n)
	}

	unlock()
	unlock()
	if n := held(&l); n != 0 {
		t.Fatalf("held %d keys after the unlock, want none", n)
	}

	// A waiter takes the key once its holder unlocks it.
	unlock, err = l.lock(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		next, err := l.lock(t.Context(), "k")
		if err != nil {
			t.Error(err)
		}
		got <- next
	}()
	unlock()
	(<-got)()
	if n := held(&l); n != 0 {
		t.Fatalf("held %d keys after every unlock, want none", n)
	}
}

func held(l *keyLock[string]) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}
