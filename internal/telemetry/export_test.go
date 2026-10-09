package telemetry

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type rejectedExporter struct{}

func (rejectedExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector echoed secret-credential-canary")
}
func (rejectedExporter) Shutdown(context.Context) error {
	return errors.New("collector echoed secret-credential-canary")
}

// fakeClock is a settable time source for the rate limiter.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// logRecords decodes the JSON log lines in buf.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestCollectorErrorDetailsDoNotEnterSDKDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	e := newSafeExporter(rejectedExporter{}, slog.New(slog.NewJSONHandler(&logs, nil)), time.Now)
	for _, err := range []error{e.ExportSpans(t.Context(), nil), e.Shutdown(t.Context())} {
		if err == nil || strings.Contains(err.Error(), "canary") {
			t.Fatal("collector error was not scrubbed")
		}
	}
	if !errors.Is(e.ExportSpans(t.Context(), nil), errExportFailed) || errExportFailed.Error() != "OTLP trace export failed" {
		t.Fatal("the SDK is not told one constant error")
	}
	if strings.Contains(logs.String(), "canary") || logs.Len() == 0 {
		t.Fatalf("log = %q: the failure was not logged, or its text was", logs.String())
	}
}

// realExporter is the OTLP exporter Agent Kourier uses, aimed at url, with retries short enough for a test.
func realExporter(t *testing.T, url string, timeout time.Duration) sdktrace.SpanExporter {
	t.Helper()
	exporter, err := otlptracehttp.New(t.Context(), otlptracehttp.WithEndpointURL(url), otlptracehttp.WithTimeout(timeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: true, InitialInterval: time.Millisecond, MaxInterval: 5 * time.Millisecond, MaxElapsedTime: 50 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	return exporter
}

// exportOne sends one span through e and returns what the SDK is told.
func exportOne(t *testing.T, e sdktrace.SpanExporter) error {
	t.Helper()
	stub := tracetest.SpanStub{Name: "x"}
	return e.ExportSpans(t.Context(), []sdktrace.ReadOnlySpan{stub.Snapshot()})
}

// refusedURL is an address nothing listens on.
func refusedURL(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr + "/v1/traces"
}

func TestExportClassOfErrorsTheExporterWraps(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"dns", fmt.Errorf("traces export: Post: %w", &net.DNSError{Err: "no such host", Name: "collector"}), "dns"},
		{"unknown authority", fmt.Errorf("traces export: %w", x509.UnknownAuthorityError{}), "tls"},
		{"hostname", fmt.Errorf("traces export: %w", x509.HostnameError{}), "tls"},
		{"cancelled", fmt.Errorf("traces export: %w", context.Canceled), "cancelled"},
		{"body that imitates a status", errors.New("traces export: max retry time elapsed: retry-able request failure: body: failed to send to x: 401 "), "http_retryable"},
		{"unknown", errors.New("something else"), "error"},
	} {
		if got := exportClass(tc.err); got != tc.want {
			t.Errorf("%s: class = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestExportFailureIsLoggedByClass(t *testing.T) {
	const body = "collector-body-canary"
	status := func(code int) func(t *testing.T) string {
		return func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)
			return srv.URL + "/v1/traces"
		}
	}
	for _, tc := range []struct {
		name    string
		url     func(t *testing.T) string
		timeout time.Duration
		want    string
	}{
		{"refused", refusedURL, time.Second, "refused"},
		{"unauthorized", status(http.StatusUnauthorized), time.Second, "http_401"},
		{"forbidden", status(http.StatusForbidden), time.Second, "http_403"},
		{"not found", status(http.StatusNotFound), time.Second, "http_404"},
		{"retryable", status(http.StatusServiceUnavailable), time.Second, "http_retryable"},
		{"timeout", func(t *testing.T) string {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(release) }) // runs first, so Close is not held by the stalled handler
			return srv.URL + "/v1/traces"
		}, 30 * time.Millisecond, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			e := newSafeExporter(realExporter(t, tc.url(t), tc.timeout), slog.New(slog.NewJSONHandler(&logs, nil)), (&fakeClock{t: time.Unix(1000, 0)}).now)
			err := exportOne(t, e)
			if !errors.Is(err, errExportFailed) {
				t.Fatalf("SDK was told %v, want the constant error", err)
			}
			records := logRecords(t, &logs)
			if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["error_type"] != tc.want {
				t.Fatalf("log = %s, want one WARN of class %s", logs.String(), tc.want)
			}
			if strings.Contains(logs.String(), body) || strings.Contains(logs.String(), "127.0.0.1") {
				t.Fatalf("log carries the collector's body or address: %s", logs.String())
			}
		})
	}
}

// switchable lets one safeExporter, with its one rate limiter, fail in different ways.
type switchable struct{ sdktrace.SpanExporter }

func TestExportFailureWarningsAreRateLimitedPerClass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	var logs bytes.Buffer
	clock := &fakeClock{t: time.Unix(1000, 0)}
	next := &switchable{realExporter(t, srv.URL+"/v1/traces", time.Second)}
	e := newSafeExporter(next, slog.New(slog.NewJSONHandler(&logs, nil)), clock.now)
	for range 3 {
		_ = exportOne(t, e)
		clock.t = clock.t.Add(time.Second)
	}
	if got := logRecords(t, &logs); len(got) != 1 || got[0]["suppressed"] != float64(0) {
		t.Fatalf("three failures in one window logged %v, want one line", got)
	}
	// A different class is its own bucket, in the same window.
	unauthorized := next.SpanExporter
	next.SpanExporter = realExporter(t, refusedURL(t), time.Second)
	_ = exportOne(t, e)
	if got := logRecords(t, &logs); len(got) != 2 || got[1]["error_type"] != "refused" {
		t.Fatalf("a new class was withheld: %v", got)
	}
	next.SpanExporter = unauthorized
	clock.t = clock.t.Add(warnEvery)
	_ = exportOne(t, e)
	got := logRecords(t, &logs)
	if len(got) != 3 || got[2]["error_type"] != "http_401" || got[2]["suppressed"] != float64(2) {
		t.Fatalf("after the interval: %v, want a third line counting the 2 withheld", got)
	}
}

func TestWarnLimiterBoundsItsKeys(t *testing.T) {
	l := newWarnLimiter((&fakeClock{t: time.Unix(1000, 0)}).now, time.Minute)
	for i := range maxWarnKeys {
		if _, ok := l.allow(strconv.Itoa(i)); !ok {
			t.Fatalf("key %d refused before the bound", i)
		}
	}
	if _, ok := l.allow("one too many"); ok {
		t.Fatal("a key past the bound was tracked")
	}
	if _, ok := l.allow("0"); ok {
		t.Fatal("a tracked key was let through twice in a window")
	}
}
