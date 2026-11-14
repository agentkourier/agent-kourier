package k8ssecrets

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// deniedAPI is a fake API whose list of sre/creds answers forbidden until allow is set, and, while hold holds a channel, waits for it to
// be closed before it answers.
func deniedAPI(t *testing.T) (client *api, allow *atomic.Bool, hold *atomic.Pointer[chan struct{}], lists *atomic.Int64) {
	t.Helper()
	client = newAPI(t, secret("sre", "creds"))
	allow, hold, lists = &atomic.Bool{}, &atomic.Pointer[chan struct{}]{}, &atomic.Int64{}
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.ListAction).GetListRestrictions().Fields.String() != "metadata.name=creds" {
			return false, nil, nil
		}
		lists.Add(1)
		if h := hold.Load(); h != nil {
			<-*h
		}
		if allow.Load() {
			return false, nil, nil
		}
		return true, nil, denied()
	})
	return client, allow, hold, lists
}

// A denial is reported once, whatever the number of tries that are denied again, and the recovery is reported once: a log that said it
// for every try would be the cost this change removes, and one that never said it recovered would leave the denial standing.
func TestADeniedSecretIsReportedOncePerChangeOfState(t *testing.T) {
	client, allow, _, lists := deniedAPI(t)
	logs := &logBuffer{}
	r := New(client, WithLogger(slog.New(slog.NewTextHandler(logs, nil))), WithDeniedRetry(50*time.Millisecond))
	t.Cleanup(r.Stop)
	names := []config.SecretName{sn("sre", "creds")}
	_ = r.Watch(t.Context(), names)
	for range 4 { // four rechecks, each of which tries it and is denied again
		time.Sleep(80 * time.Millisecond)
		_ = r.Watch(t.Context(), names)
	}
	if n := strings.Count(logs.String(), "secret informer cannot list or watch"); n != 1 {
		t.Errorf("the denial was reported %d times over five tries; want once\n%s", n, logs.String())
	}
	if n := lists.Load(); n < 5 { // the first try and the four rechecks
		t.Fatalf("%d lists: the rechecks did not try it, so this test proves nothing", n)
	}

	allow.Store(true)
	testutil.Eventually(t, "the Secret to be read again", func() bool {
		time.Sleep(80 * time.Millisecond)
		_ = r.Watch(t.Context(), names)
		_, err := r.Keys(t.Context(), "sre", "creds")
		return err == nil
	}, testutil.Within(10*time.Second), testutil.Every(5*time.Millisecond))
	testutil.Eventually(t, "the recovery in the log", func() bool { return strings.Contains(logs.String(), "can list and watch again") }, testutil.Within(10*time.Second), testutil.Every(5*time.Millisecond))
	if n := strings.Count(logs.String(), "can list and watch again"); n != 1 {
		t.Errorf("the recovery was reported %d times; want once\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), canary) {
		t.Errorf("log holds a Secret value: %s", logs.String())
	}
}

// While a try of a denied Secret is in flight, a read says what it said before, denied, and not that the cache has not synced: the
// status of a Binding must not flicker between the two at every recheck. A Watch waits for the try, as for any list in flight.
func TestADeniedSecretStaysDeniedToAReadWhileItIsTried(t *testing.T) {
	client, allow, hold, _ := deniedAPI(t)
	r := New(client, WithLogger(testutil.DiscardLogger()), WithDeniedRetry(50*time.Millisecond))
	t.Cleanup(r.Stop)
	names := []config.SecretName{sn("sre", "creds")}
	if err := r.Watch(t.Context(), names); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Watch = %v, want ErrAccessDenied", err)
	}
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	hold.Store(&release)
	allow.Store(true)
	time.Sleep(80 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := r.Watch(ctx, names) // starts the try, which the API holds
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrAccessDenied) {
		t.Errorf("Watch while the try is in flight = %v, want it to wait for the try and end with its own deadline", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "creds"); !errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotSynced) {
		t.Errorf("Keys while the try is in flight = %v, want ErrAccessDenied, as before the try", err)
	}
	once.Do(func() { close(release) })
	testutil.Eventually(t, "the try to be read", func() bool { _, err := r.Keys(t.Context(), "sre", "creds"); return err == nil }, testutil.Within(10*time.Second), testutil.Every(5*time.Millisecond))
}

// A Secret that does not exist is not denied: the list answers with no items, the informer syncs and holds one watch, and no recheck
// asks again. Only forbidden and unauthorized end an informer.
func TestAMissingSecretIsNotDeniedAndNotTriedAgain(t *testing.T) {
	client := newAPI(t)
	r := New(client, WithLogger(testutil.DiscardLogger()), WithDeniedRetry(10*time.Millisecond))
	t.Cleanup(r.Stop)
	names := []config.SecretName{sn("sre", "missing")}
	if err := r.Watch(t.Context(), names); err != nil {
		t.Fatalf("Watch of a Secret that does not exist = %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	_ = r.Watch(t.Context(), names)
	if lists, _ := client.count(names[0]); lists != 1 {
		t.Errorf("%d lists of a Secret that does not exist, want the one", lists)
	}
	if _, err := r.Keys(t.Context(), "sre", "missing"); !errors.Is(err, config.ErrSecretNotFound) {
		t.Errorf("Keys = %v, want ErrSecretNotFound", err)
	}
}
