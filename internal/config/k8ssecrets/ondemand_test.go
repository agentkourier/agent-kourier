package k8ssecrets

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
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

// When the caller's context ends during the wait, the cancellation stays in the error beside ErrNotSynced, so that a caller
// that tells a closed pool from an unreadable Secret by the context (Pool.Check) can.
func TestValueKeepsTheCallersCancellation(t *testing.T) {
	client := newAPI(t, secret("sre", "never"))
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler))) // the default 10 s wait, which must not be what ends it
	t.Cleanup(r.Stop)

	canceled, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	begin := time.Now()
	_, err := r.Value(canceled, "sre", "never", "token")
	if !errors.Is(err, ErrNotSynced) || !errors.Is(err, context.Canceled) || time.Since(begin) > 5*time.Second {
		t.Errorf("Value after the caller cancelled = %v after %s, want ErrNotSynced wrapping context.Canceled, promptly", err, time.Since(begin))
	}

	expiring, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	if _, err := r.Value(expiring, "sre", "never", "token"); !errors.Is(err, ErrNotSynced) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Value after the caller's deadline = %v, want ErrNotSynced wrapping context.DeadlineExceeded", err)
	}
}

// entryOf returns the entry of a followed Secret, or nil.
func entryOf(r *Resolver, n config.SecretName) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[n]
}

// Secrets that a failed reload asked for are let go by the next reload: a run of failed reloads leaves the informers of the
// last one, not of each. What a successful reload retained, and what is pinned, stays.
//
// The released informers are held and asked directly whether they ended: told to end, and Shutdown, which waits for the
// informer's goroutines, returns. A snapshot of the goroutines at some point would race with the reflectors of the kept
// Secrets, which list, sync, and only then open their watch; the watch here opens late on purpose.
func TestFailedReloadsDoNotAccumulateInformers(t *testing.T) {
	client := newAPI(t, secret("agent-kourier", "pg"), secret("sre", "kept"))
	client.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		time.Sleep(200 * time.Millisecond) // a watch that opens after the cache has synced, as on a slow runner
		return false, nil, nil
	})
	r := New(client, WithPinned(sn("agent-kourier", "pg")), WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "kept")}); err != nil {
		t.Fatal(err)
	}
	r.Retain([]config.SecretName{sn("sre", "kept")}) // a good snapshot
	kept, pinned := entryOf(r, sn("sre", "kept")), entryOf(r, sn("agent-kourier", "pg"))

	held := map[config.SecretName]*entry{}
	for _, n := range []string{"bad1", "bad2", "bad3", "bad4", "bad5"} { // five failed reloads, each with a new Secret
		if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "kept"), sn("sre", n)}); err != nil {
			t.Fatal(err)
		}
		held[sn("sre", n)] = entryOf(r, sn("sre", n))
	}

	want := map[config.SecretName]bool{sn("agent-kourier", "pg"): true, sn("sre", "kept"): true, sn("sre", "bad5"): true}
	if got := entryNames(r); len(got) != len(want) || !got[sn("sre", "bad5")] || !got[sn("sre", "kept")] || !got[sn("agent-kourier", "pg")] {
		t.Errorf("informers = %v, want %v: the pinned and retained Secrets and the latest reload's", got, want)
	}

	// One more reload that names the retained Secret alone lets go of bad5 too. Every informer that was let go must have
	// been told to end and have no goroutine left; the pinned and the retained one must still run.
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "kept")}); err != nil {
		t.Fatal(err)
	}
	for n, e := range held {
		select {
		case <-e.ended:
		default:
			t.Errorf("the informer of %s was let go but not told to end", n)
		}
		done := make(chan struct{})
		go func() { e.factory.Shutdown(); close(done) }() // waits for the informer's goroutines, and returns at once if they are gone
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("the informer of %s still runs after it was let go", n)
		}
	}
	for n, e := range map[string]*entry{"kept": kept, "pinned": pinned} {
		select {
		case <-e.ended:
			t.Errorf("the %s informer ended although it must keep running", n)
		default:
		}
	}
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

// What the last good snapshot reads stays readable through a reload that fails: the failed config repoints a Secret, so its
// names omit the retained one, and Watch must not let it go. Revision and the turns that hold the snapshot read it.
func TestAFailedReloadKeepsTheLastGoodSnapshotsSecrets(t *testing.T) {
	client := newAPI(t, secret("sre", "old"), secret("sre", "repointed"))
	r := follow(t, client, sn("sre", "old"))
	r.Retain([]config.SecretName{sn("sre", "old")}) // the good snapshot

	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "repointed")}); err != nil { // a failed reload repoints it
		t.Fatal(err)
	}

	if _, err := r.Keys(t.Context(), "sre", "old"); err != nil {
		t.Errorf("Keys of the last good snapshot's Secret after a failed reload = %v", err)
	}
	if _, err := r.SecretVersion(t.Context(), "sre", "old"); err != nil {
		t.Errorf("SecretVersion of the last good snapshot's Secret after a failed reload = %v", err)
	}
}

// listDelay makes the list of one Secret answer after d.
func listDelay(client *api, secretName string, d time.Duration) {
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.ListAction).GetListRestrictions().Fields.String() == "metadata.name="+secretName {
			time.Sleep(d)
		}
		return false, nil, nil
	})
}

// When an agent restarts, every turn of a Binding reconnects together and reads the same Secret: the first read follows it,
// and the others find a cache that has not synced. They wait for it as the first does and read the value.
func TestValueReadsWhileAnotherReadWaitsForTheSameCache(t *testing.T) {
	client := newAPI(t, secret("sre", "late"))
	listDelay(client, "late", 400*time.Millisecond)
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)

	type result struct {
		v     string
		err   error
		after time.Duration
	}
	results := make(chan result, 2)
	begin := time.Now()
	read := func() {
		v, err := r.Value(t.Context(), "sre", "late", "token")
		results <- result{v.Reveal(), err, time.Since(begin)}
	}
	go read()
	time.Sleep(100 * time.Millisecond) // the first read has followed the Secret, and its cache has not synced
	go read()

	for range 2 {
		got := <-results
		if got.err != nil || got.v != canary || got.after > 5*time.Second {
			t.Errorf("a read while the cache syncs = %q, %v after %s; want the value, in well under the 10 s wait", got.v, got.err, got.after)
		}
	}
}

// A reload that releases an informer that a Value is still waiting for must not fail the Value: it follows the Secret again.
func TestValueFollowsAgainWhenTheInformerItWaitsForIsReleased(t *testing.T) {
	client := newAPI(t, secret("sre", "held"), secret("sre", "other"))
	gate, listed := make(chan struct{}), make(chan struct{}, 1)
	var first atomic.Bool
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.ListAction).GetListRestrictions().Fields.String() == "metadata.name=held" && first.CompareAndSwap(false, true) {
			listed <- struct{}{}
			<-gate // the first list hangs until the test lets it go
		}
		return false, nil, nil
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)

	type result struct {
		v   string
		err error
	}
	got := make(chan result, 1)
	begin := time.Now()
	go func() {
		v, err := r.Value(t.Context(), "sre", "held", "token")
		got <- result{v.Reveal(), err}
	}()
	<-listed
	time.AfterFunc(300*time.Millisecond, func() { close(gate) }) // the hung list ends after the release has begun
	// A forced recheck: a Watch that does not name the Secret stops the informer the Value is waiting for.
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "other")}); err != nil {
		t.Fatal(err)
	}

	select {
	case res := <-got:
		if res.err != nil || res.v != canary {
			t.Errorf("Value = %q, %v; want it to follow the Secret again and read it", res.v, res.err)
		}
		if took := time.Since(begin); took > 5*time.Second {
			t.Errorf("Value took %s, want well under the 10 s wait", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Value did not return")
	}
}

// The wait is bounded by the resolver, not only by the caller's context: Rejected calls Value with a context that has no
// deadline, and a cache that never syncs must not hold it for ever.
func TestValueWaitIsBoundedWhateverTheContext(t *testing.T) {
	client := newAPI(t, secret("sre", "never"))
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	r.wait = 100 * time.Millisecond
	t.Cleanup(r.Stop)
	done := make(chan error, 1)
	go func() { _, err := r.Value(context.Background(), "sre", "never", "token"); done <- err }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrNotSynced) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Errorf("Value = %v, want ErrNotSynced alone once the wait's own bound ends", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Value with a context without a deadline did not return after the on-demand wait")
	}
}

// Stop ends a Value that waits, at once, and so does a denial; neither waits out the bound.
func TestValueEndsAtOnceOnStopAndOnDenial(t *testing.T) {
	client := newAPI(t, secret("sre", "never"), secret("sre", "private"))
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		switch a.(k8stesting.ListAction).GetListRestrictions().Fields.String() {
		case "metadata.name=never":
			return true, nil, errors.New("connection refused")
		case "metadata.name=private":
			return true, nil, denied()
		}
		return false, nil, nil
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler))) // the default 10 s wait
	t.Cleanup(r.Stop)

	begin := time.Now()
	if _, err := r.Value(context.Background(), "sre", "private", "token"); !errors.Is(err, ErrAccessDenied) || time.Since(begin) > 3*time.Second {
		t.Errorf("Value of a denied Secret = %v after %s, want ErrAccessDenied at once", err, time.Since(begin))
	}

	done := make(chan error, 1)
	go func() { _, err := r.Value(context.Background(), "sre", "never", "token"); done <- err }()
	time.Sleep(100 * time.Millisecond)
	stopped := time.Now()
	r.Stop()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStopped) || time.Since(stopped) > 3*time.Second {
			t.Errorf("Value across Stop = %v after %s, want ErrStopped at once", err, time.Since(stopped))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Value did not return after Stop")
	}
}
