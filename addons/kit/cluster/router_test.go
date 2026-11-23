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

// addon is what the package takes from an add-on where the add-ons differ: its name, which prefixes its metrics and names
// its hop header, and what it calls its process.
type addon struct {
	name, noun string
	// hop is the header the add-on's standbys have always set, on the wire.
	hop string
}

// addons are the add-ons on the kit.
var addons = []addon{
	{name: "cma", noun: "adapter", hop: "X-Agent-Kourier-Cma-Forwarded"},
	{name: "ax", noun: "agent", hop: "X-Agent-Kourier-Ax-Forwarded"},
}

func standby(t *testing.T, a addon, leaders Leaders, tel *telemetry.Telemetry) *httptest.Server {
	t.Helper()
	rt := &Router{Local: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a standby served a request itself")
	}), Status: &Status{}, Leaders: leaders, Addon: a.name, Noun: a.noun, RetryAfter: 2 * time.Second, Telemetry: tel, Log: discard()}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return srv
}

// testSpec names the metrics as the add-on named name does, agentkourier_<name>_*, without the backend's packages: the
// router counts only what it forwards.
func testSpec(name string) telemetry.Spec {
	return telemetry.Spec{Name: name, Calls: "backend", TurnsHelp: "Turns.", OperationHelp: "Calls.", AuthRefusedHelp: "Refusals."}
}

func newTelemetry(t *testing.T, name string) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(testSpec(name), nil)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

// forwards reads agentkourier_<name>_forwarded_requests_total{outcome} from tel, the add-on name's.
func forwards(t *testing.T, tel *telemetry.Telemetry, name string, outcome telemetry.ForwardOutcome) int {
	t.Helper()
	n := 0
	_, _ = fmt.Sscanf(lineValue(scrape(t, tel), fmt.Sprintf(`agentkourier_%s_forwarded_requests_total{outcome=%q}`, name, outcome)), "%d", &n)
	return n
}

// The hop header names the add-on, as each add-on's standbys have always sent it.
func TestHopHeaderNamesTheAddon(t *testing.T) {
	for _, a := range addons {
		if got := HopHeader(a.name); got != a.hop {
			t.Errorf("HopHeader(%q) = %q, want %q", a.name, got, a.hop)
		}
	}
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
	for _, a := range addons {
		t.Run(a.name, func(t *testing.T) {
			status := &Status{}
			status.Lead()
			status.Serve()
			served := 0
			rt := &Router{Local: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ }), Status: status,
				Leaders: &fixedLeaders{}, Addon: a.name, Noun: a.noun, Log: discard()}
			for _, hop := range []string{"", "1"} {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/a2a", nil)
				if hop != "" {
					req.Header.Set(a.hop, hop)
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
		})
	}
}

// A standby forwards a streamed answer as it comes: the first event reaches the caller while the leader still holds
// the stream open. The caller's credentials and trace context go with it, X-Forwarded-For gains the caller, and the hop
// marker is set, under the add-on's name.
func TestStandbyForwardsAStreamUnbuffered(t *testing.T) {
	for _, a := range addons {
		t.Run(a.name, func(t *testing.T) {
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
			tel := newTelemetry(t, a.name)
			srv := standby(t, a, &fixedLeaders{leader: target}, tel)

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
			rest, err := io.ReadAll(events)
			if err != nil || !strings.Contains(string(rest), "data: last") {
				t.Fatalf("rest of the stream %q, %v", rest, err)
			}
			if got.Get("Authorization") != "Bearer fake-token" || got.Get("Traceparent") == "" || got.Get(a.hop) != "1" {
				t.Fatalf("the leader got headers %v", got)
			}
			if xff := got.Get("X-Forwarded-For"); !strings.HasPrefix(xff, "203.0.113.7, ") {
				t.Fatalf("X-Forwarded-For %q: the caller is not appended", xff)
			}
			if n := forwards(t, tel, a.name, telemetry.Forwarded); n != 1 {
				t.Fatalf("forwarded = %d", n)
			}
		})
	}
}

// A standby passes the caller's request body on to the leader while the leader's answer streams back: here the leader
// answers before it reads the body, and the caller sends the body only once the answer has begun. Unless the standby
// is full duplex, net/http's server reads and closes what is left of the body as the standby's answer starts, which
// takes the body from under the forward (the transport then closes its connection to the leader and the answer is cut
// short) or, as here, holds the answer until the body has come.
func TestStandbyForwardsTheBodyWhileTheAnswerStreams(t *testing.T) {
	const payload = `{"jsonrpc":"2.0"}`
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		body, err := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "data: got %s %v\n\n", body, err)
	}))
	defer leader.Close()
	target, _ := url.Parse(leader.URL)
	srv := standby(t, addons[0], &fixedLeaders{leader: target}, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sent, send := io.Pipe()
	defer context.AfterFunc(ctx, func() { _ = send.CloseWithError(ctx.Err()) })() // a client that waits for the answer sends no body
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/a2a", sent)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the answer did not start before the body was sent: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	events := bufio.NewReader(resp.Body)
	if line, err := events.ReadString('\n'); line != "data: first\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
	if _, err := io.WriteString(send, payload); err != nil {
		t.Fatal(err)
	}
	_ = send.Close()
	rest, err := io.ReadAll(events)
	if want := "data: got " + payload + " <nil>"; err != nil || !strings.Contains(string(rest), want) {
		t.Fatalf("rest of the stream %q, %v; want %q", rest, err, want)
	}
}

// A custom AUTH_HEADER reaches the leader as it came.
func TestStandbyKeepsACustomAuthHeader(t *testing.T) {
	var got string
	leader := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Get("X-Kourier-Token") }))
	defer leader.Close()
	target, _ := url.Parse(leader.URL)
	resp := post(t, standby(t, addons[0], &fixedLeaders{leader: target}, nil).URL+"/a2a", http.Header{"X-Kourier-Token": {"fake-token"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || got != "fake-token" {
		t.Fatalf("answer %d, header at the leader %q", resp.StatusCode, got)
	}
}

// A request another standby of the same add-on forwarded is never forwarded again; another add-on's hop header is not
// this add-on's, and does not stop the forward.
func TestStandbyRefusesASecondHop(t *testing.T) {
	for i, a := range addons {
		t.Run(a.name, func(t *testing.T) {
			leader := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("a forwarded request was forwarded again")
			}))
			defer leader.Close()
			target, _ := url.Parse(leader.URL)
			tel := newTelemetry(t, a.name)
			want503(t, standby(t, a, &fixedLeaders{leader: target}, tel).URL+"/a2a", http.Header{a.hop: {"1"}})
			if n := forwards(t, tel, a.name, telemetry.ForwardLoop); n != 1 {
				t.Fatalf("loop = %d", n)
			}

			other := addons[(i+1)%len(addons)]
			var hops []string
			next := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hops = r.Header.Values(a.hop) }))
			defer next.Close()
			target, _ = url.Parse(next.URL)
			resp := post(t, standby(t, a, &fixedLeaders{leader: target}, tel).URL+"/a2a", http.Header{other.hop: {"1"}})
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || len(hops) != 1 || hops[0] != "1" {
				t.Fatalf("with %s's hop header: answer %d, %s at the leader %q", other.name, resp.StatusCode, a.hop, hops)
			}
		})
	}
}

func TestStandbyWithNoLeader(t *testing.T) {
	for _, a := range addons {
		t.Run(a.name, func(t *testing.T) {
			tel := newTelemetry(t, a.name)
			want503(t, standby(t, a, &fixedLeaders{}, tel).URL+"/a2a", nil)
			if n := forwards(t, tel, a.name, telemetry.ForwardNoLeader); n != 1 {
				t.Fatalf("no_leader = %d", n)
			}
		})
	}
}

// A leader that cannot be reached is answered 503 and forgotten, so the next request reads the lease again.
func TestStandbyWithAnUnreachableLeader(t *testing.T) {
	for _, a := range addons {
		t.Run(a.name, func(t *testing.T) {
			gone := httptest.NewServer(http.NotFoundHandler())
			target, _ := url.Parse(gone.URL)
			gone.Close()
			leaders := &fixedLeaders{leader: target}
			tel := newTelemetry(t, a.name)
			want503(t, standby(t, a, leaders, tel).URL+"/a2a", nil)
			if leaders.forgets.Load() != 1 || forwards(t, tel, a.name, telemetry.ForwardFailed) != 1 {
				t.Fatalf("forgets = %d, failed = %d", leaders.forgets.Load(), forwards(t, tel, a.name, telemetry.ForwardFailed))
			}
		})
	}
}

// A leader that is still taking over answers 503, in the add-on's own words, and does not forward, to itself or anyone.
func TestLeaderTakingOverAnswers503(t *testing.T) {
	for _, tc := range []struct {
		addon
		body string
	}{
		{addons[0], "the adapter is taking over as the leader\n"},
		{addons[1], "the agent is taking over as the leader\n"},
		{addon{name: "echo"}, "the add-on is taking over as the leader\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := &Status{}
			status.Lead()
			rt := &Router{Local: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("served before the takeover was done") }),
				Status: status, Leaders: &fixedLeaders{leader: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}}, Addon: tc.name, Noun: tc.noun,
				RetryAfter: 2 * time.Second, Log: discard()}
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/a2a", nil))
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" || rec.Body.String() != tc.body {
				t.Fatalf("answer %d, Retry-After %q, body %q; want 503, 2 and %q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String(), tc.body)
			}
			if status.Ready() {
				t.Fatal("a leader that has not taken over is ready")
			}
			status.Serve()
			if !status.Ready() || !(&Status{}).Ready() {
				t.Fatal("a serving leader and a standby are both ready")
			}
		})
	}
}
