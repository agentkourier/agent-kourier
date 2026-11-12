package k8ssecrets

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// tenantAPI is an API server on the wire, so that the client's own rate limiter is in the way as it is in the broker. It answers
// 403 for a Secret whose name starts with "denied-" and serves every other Secret named by the field selector, as the API server
// does for a list or a watch of one name. It counts the requests of each kind.
type tenantAPI struct {
	srv    *httptest.Server
	denied atomic.Int64 // requests for a denied Secret, lists and watches

	mu    sync.Mutex
	asked map[string]int // the requests for each denied Secret
}

func isDenied(name string) bool { return strings.HasPrefix(name, "denied-") }

func newTenantAPI(t *testing.T) (*tenantAPI, kubernetes.Interface) {
	t.Helper()
	a := &tenantAPI{asked: map[string]int{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "metadata.name=")
		w.Header().Set("Content-Type", "application/json")
		if isDenied(name) {
			a.denied.Add(1)
			a.mu.Lock()
			a.asked[name]++
			a.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"secrets is forbidden"}`)
			return
		}
		object := `{"kind":"Secret","apiVersion":"v1","metadata":{"name":"` + name + `","namespace":"b","resourceVersion":"1"},"type":"Opaque","data":{"token":"Zm9v"}}`
		if q := r.URL.Query().Get("watch"); q == "true" || q == "1" {
			w.WriteHeader(http.StatusOK)
			if r.URL.Query().Get("sendInitialEvents") == "true" { // a streamed list
				_, _ = fmt.Fprint(w, `{"type":"ADDED","object":`+object+"}\n")
				_, _ = fmt.Fprint(w, `{"type":"BOOKMARK","object":{"kind":"Secret","apiVersion":"v1","metadata":{"resourceVersion":"1","annotations":{"k8s.io/initial-events-end":"true"}}}}`+"\n")
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = fmt.Fprint(w, `{"kind":"SecretList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[`+object+`]}`)
	}))
	t.Cleanup(a.srv.Close)
	// One limiter shared by every informer, as the broker's client has (50 requests a second with a burst of 100, in runconfig.go),
	// at half the rate and a quarter of the burst so that the load of a few hundred Secrets shows within seconds.
	client, err := kubernetes.NewForConfig(&rest.Config{Host: a.srv.URL, QPS: 25, Burst: 25})
	if err != nil {
		t.Fatal(err)
	}
	return a, client
}

// lineCounter counts the lines written to it: the log records of a Resolver.
type lineCounter struct{ n atomic.Int64 }

func (c *lineCounter) Write(p []byte) (int, error) {
	c.n.Add(int64(bytes.Count(p, []byte("\n"))))
	return len(p), nil
}

func deniedReads(n int) []config.SecretRead {
	reads := make([]config.SecretRead, n)
	for i := range reads {
		reads[i] = readOf("b", fmt.Sprintf("denied-%d", i), "token")
	}
	return reads
}

// A tenant that names many Secrets the broker may not read must not cost the others. Each Secret has an informer, and client-go's
// reflector retries a denied list for ever, on the one limiter of the client and with a log line for each failure. Once a denied
// Secret has been answered, nothing may ask for it again until the next recheck of the config, and no other request may wait
// behind those that do: an on-demand read of another tenant's Secret, which makes its own list and watch, stays fast.
func TestDeniedSecretsAreNotRetriedBetweenRechecksAndCostNoOneElse(t *testing.T) {
	const names = 150
	api, client := newTenantAPI(t)
	warnings := &lineCounter{}
	r := New(client, WithLogger(slog.New(slog.NewTextHandler(warnings, &slog.HandlerOptions{Level: slog.LevelWarn}))))
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := r.WatchKeys(ctx, deniedReads(names)); err == nil {
		t.Fatal("WatchKeys of Secrets that are all denied succeeded")
	}
	// Each Secret is reported when it is first denied (a request may fail before that, and the limiter spreads them over seconds).
	testutil.Eventually(t, "every Secret to have been answered once", func() bool { return warnings.n.Load() >= names }, testutil.Within(10*time.Second), testutil.Every(5*time.Millisecond))

	time.Sleep(time.Second) // a retry that is going to come soon has come
	deniedBefore, warnedBefore := api.denied.Load(), warnings.n.Load()
	time.Sleep(3 * time.Second)

	begin := time.Now()
	_, err := r.Value(t.Context(), "b", "good-0", "token") // another tenant's Secret, not followed yet: its own list and its own watch
	took := time.Since(begin)
	t.Logf("Value of another tenant's Secret took %s", took)
	if err != nil {
		t.Errorf("Value of another tenant's Secret = %v", err)
	}
	if took > time.Second {
		t.Errorf("Value of another tenant's Secret took %s while %d denied Secrets are followed; it waited behind their requests", took, names)
	}

	time.Sleep(3 * time.Second)
	if extra := api.denied.Load() - deniedBefore; extra > names/20 {
		t.Errorf("%d requests for the %d denied Secrets in 6 s, after each had been answered; want almost none until the next recheck", extra, names)
	}
	if extra := warnings.n.Load() - warnedBefore; extra > names/20 {
		t.Errorf("%d warnings in 6 s for the %d denied Secrets; want a warning for a change of state and not for each failure", extra, names)
	}
}

// asking returns how many requests each denied Secret has had.
func (a *tenantAPI) asking() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.asked)
}

// triedSince returns how many denied Secrets have had at least n requests more than in base. A try of a Secret is two requests
// (measured against this server with client-go's reflector: both are answered forbidden).
func (a *tenantAPI) triedSince(base map[string]int, n int) (tried int) {
	for name, got := range a.asking() {
		if got >= base[name]+n {
			tried++
		}
	}
	return tried
}

// The tries of the denied Secrets that a recheck makes are paced: the first deniedRetryBurst at once and then one every
// deniedRetryPace, each Secret once, so that they leave the shared limiter to everyone else; and a reload that comes before the retry
// interval has run out makes none. A Stop with tries still queued ends the goroutine that starts them (the goleak of TestMain).
func TestTheTriesOfDeniedSecretsAtARecheckAreOnceEachAndPaced(t *testing.T) {
	const names = 30
	const retry = 400 * time.Millisecond
	api, client := newTenantAPI(t)
	warnings := &lineCounter{}
	r := New(client, WithDeniedRetry(retry), WithLogger(slog.New(slog.NewTextHandler(warnings, &slog.HandlerOptions{Level: slog.LevelWarn}))))
	t.Cleanup(r.Stop)
	reads := deniedReads(names)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_ = r.WatchKeys(ctx, reads)
	testutil.Eventually(t, "every Secret to have been answered once", func() bool { return warnings.n.Load() >= names }, testutil.Within(10*time.Second), testutil.Every(5*time.Millisecond))
	base := api.asking()

	_ = r.WatchKeys(ctx, reads) // a reload, sooner than the retry interval after the Secrets were started: a changed config
	time.Sleep(100 * time.Millisecond)
	if tried := api.triedSince(base, 1); tried != 0 {
		t.Fatalf("a reload before the retry interval tried %d denied Secrets", tried)
	}

	time.Sleep(retry) // the recheck
	begin := time.Now()
	_ = r.WatchKeys(ctx, reads)
	time.Sleep(4 * deniedRetryPace)
	if tried := api.triedSince(base, 1); tried < deniedRetryBurst || tried > deniedRetryBurst+5 {
		t.Errorf("%s after the recheck %d Secrets were tried; want the burst of %d and one more every %s", time.Since(begin), tried, deniedRetryBurst, deniedRetryPace)
	}
	testutil.Eventually(t, "the rest to be tried", func() bool { return api.triedSince(base, 2) >= names }, testutil.Within(10*time.Second), testutil.Every(5*time.Millisecond))
	if took := time.Since(begin); took < time.Duration(names-deniedRetryBurst-2)*deniedRetryPace {
		t.Errorf("all %d tries were made within %s; want them paced, one every %s after the first %d", names, took, deniedRetryPace, deniedRetryBurst)
	}
	// The interval runs from when a Secret is tried and not from when the recheck queued it, so the last of a long queue is not asked
	// again sooner than the retry interval after it was asked.
	var first, last time.Time
	r.mu.Lock()
	for _, e := range r.entries {
		if first.IsZero() || e.attemptedAt.Before(first) {
			first = e.attemptedAt
		}
		if e.attemptedAt.After(last) {
			last = e.attemptedAt
		}
	}
	r.mu.Unlock()
	if spread := last.Sub(first); spread < time.Duration(names-deniedRetryBurst-2)*deniedRetryPace {
		t.Errorf("the Secrets were last tried over %s; want the times they were tried, %s apart, and not the time of the recheck", spread, deniedRetryPace)
	}
	time.Sleep(retry / 2)
	if more := api.triedSince(base, 3); more != 0 {
		t.Errorf("%d Secrets were tried a second time before the next recheck", more)
	}

	time.Sleep(retry) // and the next recheck, which Stop interrupts with tries still queued
	_ = r.WatchKeys(ctx, reads)
	r.Stop()
}
