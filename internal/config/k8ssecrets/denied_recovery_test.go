package k8ssecrets

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// A Secret that synced is denied once, say by an expired token or an authorizer that failed for a moment, and its informer ends. It
// goes on serving what it cached, and the next recheck after the retry interval starts a new informer that picks up what changed
// meanwhile. This holds for a pinned Secret (the Postgres DSN, the connection's tokens) and for one that the config names: if
// neither were tried again, a blip would freeze the credentials until the broker restarts.
func TestASecretThatSyncedAndWasDeniedOnceRecoversAtARecheck(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		name := "retained"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			creds := sn("sre", "creds")
			client := newAPI(t, secret("sre", "creds"))
			var blip atomic.Bool
			blip.Store(true)
			opened := make(chan struct{}) // the watch is held until the Secret has synced, so that the order is fixed
			var open sync.Once
			client.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
				<-opened
				if blip.Load() {
					return true, nil, apierrors.NewUnauthorized("token expired") // the list was allowed, so the cache syncs
				}
				return false, nil, nil
			})
			opts := []Option{WithLogger(slog.New(slog.DiscardHandler)), WithDeniedRetry(100 * time.Millisecond)}
			if pinned {
				opts = append(opts, WithPinned(creds))
			}
			r := New(client, opts...)
			t.Cleanup(r.Stop)
			t.Cleanup(func() { open.Do(func() { close(opened) }) }) // runs before Stop, which waits for the informer's goroutines
			if pinned {
				if err := r.Start(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := r.Watch(t.Context(), []config.SecretName{creds}); err != nil {
					t.Fatal(err)
				}
				r.Retain([]config.SecretName{creds})
			}
			open.Do(func() { close(opened) })
			waitFor(t, "the informer to end for the denial", func() bool {
				e := entryOf(r, creds)
				r.mu.Lock()
				defer r.mu.Unlock()
				return e.deniedLocked()
			})
			if v, err := r.Value(t.Context(), "sre", "creds", "token"); err != nil || v.Reveal() != canary {
				t.Fatalf("Value while it is denied = %q, %v; want what the cache held", v.Reveal(), err)
			}

			blip.Store(false)
			rotated := secret("sre", "creds")
			rotated.Data["token"] = []byte("rotated")
			rotated.ResourceVersion = "2"
			if _, err := client.CoreV1().Secrets("sre").Update(t.Context(), rotated, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(150 * time.Millisecond) // the retry interval
			waitFor(t, "the rotated value after a recheck", func() bool {
				_ = r.Watch(t.Context(), nil) // a recheck of a config that names nothing: a pinned Secret is tried, and a retained one is too
				v, err := r.Value(t.Context(), "sre", "creds", "token")
				return err == nil && v.Reveal() == "rotated"
			})
		})
	}
}

// A denial that is reported, recovered from and then comes again is reported again: the recovery clears what was said. The second
// denial is of the list of a new informer for other keys.
func TestADeniedAgainAfterRecoveryIsReportedAgain(t *testing.T) {
	client, allow, _, _ := deniedAPI(t)
	logs := &logBuffer{}
	r := New(client, WithLogger(slog.New(slog.NewTextHandler(logs, nil))), WithDeniedRetry(50*time.Millisecond))
	t.Cleanup(r.Stop)
	token := []config.SecretRead{readOf("sre", "creds", "token")}
	_ = r.WatchKeys(t.Context(), token)
	allow.Store(true)
	waitFor(t, "the Secret to be read again", func() bool {
		time.Sleep(80 * time.Millisecond)
		_ = r.WatchKeys(t.Context(), token)
		_, err := r.Keys(t.Context(), "sre", "creds")
		return err == nil
	})
	waitFor(t, "the recovery in the log", func() bool { return strings.Contains(logs.String(), "can list and watch again") })

	allow.Store(false)
	_ = r.WatchKeys(t.Context(), []config.SecretRead{readOf("sre", "creds", "token", "alpha")}) // a new informer, which is denied
	waitFor(t, "the second denial in the log", func() bool {
		return strings.Count(logs.String(), "secret informer cannot list or watch") == 2
	})
}

// A new informer for other keys is denied while the one that serves keeps working: the Secret is not read for the new keys until a
// retry succeeds, and the retry is made, though the informer that serves is not denied.
func TestADeniedInformerForNewKeysIsTriedAgainWhileTheOneThatServesWorks(t *testing.T) {
	client, allow, _, _ := deniedAPI(t)
	allow.Store(true)
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)), WithDeniedRetry(50*time.Millisecond))
	t.Cleanup(r.Stop)
	token := []config.SecretRead{readOf("sre", "creds", "token")}
	both := []config.SecretRead{readOf("sre", "creds", "token", "alpha")}
	if err := r.WatchKeys(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	r.RetainKeys(token)

	allow.Store(false)
	_ = r.WatchKeys(t.Context(), both) // the informer for both keys is denied
	if _, err := r.Value(t.Context(), "sre", "creds", "alpha"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Value of the new key while its informer is denied = %v, want ErrAccessDenied", err)
	}
	if v, err := r.Value(t.Context(), "sre", "creds", "token"); err != nil || v.Reveal() != canary {
		t.Fatalf("Value of a key that is kept = %q, %v; it is served by the informer that works", v.Reveal(), err)
	}

	allow.Store(true)
	time.Sleep(80 * time.Millisecond)
	waitFor(t, "the new key after a recheck", func() bool {
		_ = r.WatchKeys(t.Context(), both)
		v, err := r.Value(t.Context(), "sre", "creds", "alpha")
		return err == nil && v.Reveal() == canary
	})
}
