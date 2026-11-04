package k8ssecrets

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// entryNames returns the Secrets that have an informer.
func entryNames(r *Resolver) map[config.SecretName]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[config.SecretName]bool{}
	for n := range r.entries {
		out[n] = true
	}
	return out
}

// A turn holds the Binding of the snapshot it began under, and a reload may have dropped one of its Secrets since. Value
// follows such a Secret and reads it; Keys and SecretVersion do not, and make no API call.
func TestValueFollowsASecretThatIsNotFollowed(t *testing.T) {
	client := newAPI(t, secret("sre", "creds"), secret("sre", "gone"))
	r := follow(t, client, sn("sre", "creds"), sn("sre", "gone"))
	r.Retain([]config.SecretName{sn("sre", "creds")}) // a reload dropped sre/gone
	if lists, _ := client.count(sn("sre", "gone")); lists != 1 {
		t.Fatalf("setup: sre/gone listed %d times", lists)
	}

	if _, err := r.Keys(t.Context(), "sre", "gone"); !errors.Is(err, ErrNotWatched) {
		t.Errorf("Keys = %v, want ErrNotWatched", err)
	}
	if _, err := r.SecretVersion(t.Context(), "sre", "gone"); !errors.Is(err, ErrNotWatched) {
		t.Errorf("SecretVersion = %v, want ErrNotWatched", err)
	}
	if lists, _ := client.count(sn("sre", "gone")); lists != 1 {
		t.Errorf("Keys and SecretVersion made %d API calls, want none", lists-1)
	}

	v, err := r.Value(t.Context(), "sre", "gone", "token")
	if err != nil || v.Reveal() != canary {
		t.Fatalf("Value of the dropped Secret = %v, %v; want the value", v, err)
	}
	if _, err := r.Keys(t.Context(), "sre", "gone"); err != nil {
		t.Errorf("Keys once Value has followed it = %v", err)
	}
}

// Value of a Secret it has to follow fails as a read of any Secret does: forbidden is ErrAccessDenied at once, and a cache
// that does not sync is ErrNotSynced once ctx ends.
func TestValueOfAnUnfollowedSecretReportsDenialAndTimeout(t *testing.T) {
	client := newAPI(t, secret("sre", "private"), secret("sre", "slow"))
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		switch a.(k8stesting.ListAction).GetListRestrictions().Fields.String() {
		case "metadata.name=private":
			return true, nil, denied()
		case "metadata.name=slow":
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)

	if _, err := r.Value(t.Context(), "sre", "private", "token"); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("Value of a denied Secret = %v, want ErrAccessDenied", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := r.Value(ctx, "sre", "slow", "token"); !errors.Is(err, ErrNotSynced) {
		t.Errorf("Value of a Secret whose cache does not sync = %v, want ErrNotSynced", err)
	}
}

// Secrets that a failed reload asked for are let go by the next reload: a run of failed reloads leaves the informers of the
// last one, not of each. What a successful reload retained, and what is pinned, stays.
func TestFailedReloadsDoNotAccumulateInformers(t *testing.T) {
	before := goleak.IgnoreCurrent()
	client := newAPI(t, secret("agent-kourier", "pg"), secret("sre", "kept"))
	r := New(client, WithPinned(sn("agent-kourier", "pg")), WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "kept")}); err != nil {
		t.Fatal(err)
	}
	r.Retain([]config.SecretName{sn("sre", "kept")}) // a good snapshot

	for _, n := range []string{"bad1", "bad2", "bad3", "bad4", "bad5"} { // five failed reloads, each with a new Secret
		if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "kept"), sn("sre", n)}); err != nil {
			t.Fatal(err)
		}
	}

	want := map[config.SecretName]bool{sn("agent-kourier", "pg"): true, sn("sre", "kept"): true, sn("sre", "bad5"): true}
	if got := entryNames(r); len(got) != len(want) || !got[sn("sre", "bad5")] || !got[sn("sre", "kept")] || !got[sn("agent-kourier", "pg")] {
		t.Errorf("informers = %v, want %v: the pinned and retained Secrets and the latest reload's", got, want)
	}

	r.Retain(nil)
	r.Stop()
	goleak.VerifyNone(t, before)
}

// An informer that Value started for a holder lasts until the next Watch that does not name the Secret. A holder that still
// reads it follows it again, which costs a list, not an error.
func TestValueFollowsADroppedSecretAgainAfterAReload(t *testing.T) {
	client := newAPI(t, secret("sre", "creds"), secret("sre", "held"))
	r := follow(t, client, sn("sre", "creds"))
	r.Retain([]config.SecretName{sn("sre", "creds")})

	if _, err := r.Value(t.Context(), "sre", "held", "token"); err != nil {
		t.Fatal(err)
	}
	if !entryNames(r)[sn("sre", "held")] {
		t.Fatal("Value did not follow the Secret")
	}
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "creds")}); err != nil { // the next reload
		t.Fatal(err)
	}
	if entryNames(r)[sn("sre", "held")] {
		t.Error("the next reload left the holder's informer running")
	}
	if _, err := r.Keys(t.Context(), "sre", "held"); !errors.Is(err, ErrNotWatched) {
		t.Errorf("Keys of the released Secret = %v, want ErrNotWatched", err)
	}
	v, err := r.Value(t.Context(), "sre", "held", "token")
	if err != nil || v.Reveal() != canary {
		t.Errorf("Value after the release = %v, %v; want it followed again", v, err)
	}
	if lists, _ := client.count(sn("sre", "held")); lists != 2 {
		t.Errorf("sre/held was listed %d times, want 2 (once per time it was followed)", lists)
	}
}

// What is denied for one Secret is not said of another: a wait for a Secret whose list is slow ends at its context, not with
// the denial of a Secret that is followed beside it, and Keys of it says that its cache has yet to sync.
func TestAnotherSecretsDenialIsNotThisOnes(t *testing.T) {
	client := newAPI(t, secret("sre", "private"), secret("sre", "slow"))
	release := make(chan struct{})
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		switch a.(k8stesting.ListAction).GetListRestrictions().Fields.String() {
		case "metadata.name=private":
			return true, nil, denied()
		case "metadata.name=slow":
			<-release // the list hangs
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	t.Cleanup(func() { close(release) }) // runs before Stop, which waits for the informers

	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "private")}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Watch of the denied Secret = %v, want ErrAccessDenied", err)
	}
	r.Retain([]config.SecretName{sn("sre", "private")}) // keeps its informer, still denied, while the next is waited for

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := r.Watch(ctx, []config.SecretName{sn("sre", "slow")})
	if errors.Is(err, ErrAccessDenied) || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "[sre/slow]") {
		t.Errorf("Watch of the slow Secret = %v, want its context's deadline, naming sre/slow, and no denial", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "slow"); !errors.Is(err, ErrNotSynced) || errors.Is(err, ErrAccessDenied) {
		t.Errorf("Keys of the slow Secret = %v, want ErrNotSynced and no denial", err)
	}
}

// Ready counts the pinned Secrets: one that has not synced makes it fail, whatever the config names.
func TestReadyCountsAPinnedSecretThatHasNotSynced(t *testing.T) {
	client := newAPI(t, secret("agent-kourier", "pg"))
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	r := New(client, WithPinned(sn("agent-kourier", "pg")), WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := r.Start(ctx); err == nil {
		t.Fatal("Start of a Secret whose list fails returned nil")
	}

	if err := r.Ready(); !errors.Is(err, ErrNotSynced) || !strings.Contains(err.Error(), "agent-kourier/pg") {
		t.Errorf("Ready with a pinned Secret that is not synced = %v, want ErrNotSynced naming it", err)
	}
}

// A Secret that is pinned and also named by the config stays pinned when the config drops it: asking for it by name must not
// undo the pin.
func TestAPinnedSecretTheConfigAlsoNamesStaysPinned(t *testing.T) {
	client := newAPI(t, secret("agent-kourier", "pg"))
	r := New(client, WithPinned(sn("agent-kourier", "pg")), WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.Watch(t.Context(), []config.SecretName{sn("agent-kourier", "pg")}); err != nil { // the config names it too
		t.Fatal(err)
	}
	r.Retain([]config.SecretName{sn("agent-kourier", "pg")})

	r.Retain(nil) // and then drops it
	if err := r.Watch(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Keys(t.Context(), "agent-kourier", "pg"); err != nil {
		t.Errorf("the pinned Secret after the config dropped it: Keys = %v", err)
	}
}
