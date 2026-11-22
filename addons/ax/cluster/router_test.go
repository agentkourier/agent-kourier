package cluster

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixedLeaders is a Leaders that knows one leader, or none.
type fixedLeaders struct {
	leader  *url.URL
	forgets atomic.Int32
}

func (f *fixedLeaders) Leader(context.Context) (*url.URL, bool) { return f.leader, f.leader != nil }
func (f *fixedLeaders) Forget()                                 { f.forgets.Add(1) }

func standby(t *testing.T, leaders Leaders, tel *telemetry.Telemetry) *httptest.Server {
	t.Helper()
	rt := &Router{Local: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a standby served a request itself")
	}), Status: &Status{}, Leaders: leaders, RetryAfter: 2 * time.Second, Telemetry: tel, Log: discard()}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return srv
}

func newTelemetry(t *testing.T) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(agent.TelemetrySpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

// forwards reads agentkourier_ax_forwarded_requests_total{outcome} from tel.
func forwards(t *testing.T, tel *telemetry.Telemetry, outcome telemetry.ForwardOutcome) int {
	t.Helper()
	n := 0
	_, _ = fmt.Sscanf(lineValue(scrape(t, tel), fmt.Sprintf(`agentkourier_ax_forwarded_requests_total{outcome=%q}`, outcome)), "%d", &n)
	return n
}

// scrape returns what tel serves at /metrics.
func scrape(t *testing.T, tel *telemetry.Telemetry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func lineValue(body, series string) string {
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			return v
		}
	}
	return ""
}

func post(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// want503 posts to url and wants 503 with the Retry-After of the election's retry period.
func want503(t *testing.T, url string, header http.Header) {
	t.Helper()
	resp := post(t, url, header)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("answer %d, Retry-After %q; want 503 and 2", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// A leader that serves runs every request itself, a forwarded one included.
func TestLeaderServesLocally(t *testing.T) {
	status := &Status{}
	status.Lead()
	status.Serve()
	served := 0
	rt := &Router{Local: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ }), Status: status,
		Leaders: &fixedLeaders{}, Log: discard()}
	for _, hop := range []string{"", "1"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/a2a", nil)
		if hop != "" {
			req.Header.Set(HopHeader, hop)
		}
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("answer %d", rec.Code)
		}
	}
	if served != 2 {
		t.Fatalf("served %d of 2", served)
	}
}

// A standby forwards a streamed answer as it comes: the first event reaches the caller while the leader still holds
// the stream open. The caller's credentials and trace context go with it, X-Forwarded-For gains the caller, and the hop
// marker is set.
func TestStandbyForwardsAStreamUnbuffered(t *testing.T) {
	release := make(chan struct{})
	var got http.Header
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.URL.Path != "/a2a" {
			t.Errorf("leader got path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	defer leader.Close()
	target, _ := url.Parse(leader.URL)
	tel := newTelemetry(t)
	srv := standby(t, &fixedLeaders{leader: target}, tel)

	resp := post(t, srv.URL+"/a2a", http.Header{
		"Authorization":   {"Bearer fake-token"},
		"Traceparent":     {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		"X-Forwarded-For": {"203.0.113.7"},
	})
	defer func() { _ = resp.Body.Close() }()
	events := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		line, _ := events.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: first\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive before the stream ended: the standby buffers")
	}
	close(release)
	rest, _ := io.ReadAll(events)
	if !strings.Contains(string(rest), "data: last") {
		t.Fatalf("rest of the stream %q", rest)
	}
	if got.Get("Authorization") != "Bearer fake-token" || got.Get("Traceparent") == "" || got.Get(HopHeader) != "1" {
		t.Fatalf("the leader got headers %v", got)
	}
	if xff := got.Get("X-Forwarded-For"); !strings.HasPrefix(xff, "203.0.113.7, ") {
		t.Fatalf("X-Forwarded-For %q: the caller is not appended", xff)
	}
	if n := forwards(t, tel, telemetry.Forwarded); n != 1 {
		t.Fatalf("forwarded = %d", n)
	}
}

// A custom AUTH_HEADER reaches the leader as it came.
func TestStandbyKeepsACustomAuthHeader(t *testing.T) {
	var got string
	leader := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Get("X-Kourier-Token") }))
	defer leader.Close()
	target, _ := url.Parse(leader.URL)
	resp := post(t, standby(t, &fixedLeaders{leader: target}, nil).URL+"/a2a", http.Header{"X-Kourier-Token": {"fake-token"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || got != "fake-token" {
		t.Fatalf("answer %d, header at the leader %q", resp.StatusCode, got)
	}
}

// A request another standby forwarded is never forwarded again.
func TestStandbyRefusesASecondHop(t *testing.T) {
	leader := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a forwarded request was forwarded again")
	}))
	defer leader.Close()
	target, _ := url.Parse(leader.URL)
	tel := newTelemetry(t)
	want503(t, standby(t, &fixedLeaders{leader: target}, tel).URL+"/a2a", http.Header{HopHeader: {"1"}})
	if n := forwards(t, tel, telemetry.ForwardLoop); n != 1 {
		t.Fatalf("loop = %d", n)
	}
}

func TestStandbyWithNoLeader(t *testing.T) {
	tel := newTelemetry(t)
	want503(t, standby(t, &fixedLeaders{}, tel).URL+"/a2a", nil)
	if n := forwards(t, tel, telemetry.ForwardNoLeader); n != 1 {
		t.Fatalf("no_leader = %d", n)
	}
}

// A leader that cannot be reached is answered 503 and forgotten, so the next request reads the lease again.
func TestStandbyWithAnUnreachableLeader(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(gone.URL)
	gone.Close()
	leaders := &fixedLeaders{leader: target}
	tel := newTelemetry(t)
	want503(t, standby(t, leaders, tel).URL+"/a2a", nil)
	if leaders.forgets.Load() != 1 || forwards(t, tel, telemetry.ForwardFailed) != 1 {
		t.Fatalf("forgets = %d, failed = %d", leaders.forgets.Load(), forwards(t, tel, telemetry.ForwardFailed))
	}
}

// A leader that is still taking over answers 503 and does not forward, to itself or anyone.
func TestLeaderTakingOverAnswers503(t *testing.T) {
	status := &Status{}
	status.Lead()
	rt := &Router{Local: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("served before the takeover was done") }),
		Status: status, Leaders: &fixedLeaders{leader: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}}, RetryAfter: 2 * time.Second, Log: discard()}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/a2a", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("answer %d", rec.Code)
	}
	if status.Ready() {
		t.Fatal("a leader that has not taken over is ready")
	}
	status.Serve()
	if !status.Ready() || !(&Status{}).Ready() {
		t.Fatal("a serving leader and a standby are both ready")
	}
}
