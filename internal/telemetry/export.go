package telemetry

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"regexp"
	"sync"
	"syscall"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// warnEvery is the interval at which one kind of exporter or SDK diagnostic may log a warning.
const warnEvery = time.Minute

// maxWarnKeys bounds the kinds a warnLimiter tracks; one more kind is dropped, not remembered.
const maxWarnKeys = 64

// errExportFailed and errExporterShutdown are all the SDK is ever told: what the exporter
// reports can hold collector response bodies, URLs and authentication details. The class goes
// to the broker's log instead, where safeExporter says it once a minute.
var (
	errExportFailed     = errors.New("OTLP trace export failed")
	errExporterShutdown = errors.New("OTLP exporter shutdown failed")
)

// safeExporter keeps collector response bodies and authentication details out of SDK diagnostics.
type safeExporter struct {
	next  sdktrace.SpanExporter
	log   *slog.Logger
	limit *warnLimiter
}

func newSafeExporter(next sdktrace.SpanExporter, log *slog.Logger, now func() time.Time) safeExporter {
	return safeExporter{next: next, log: log, limit: newWarnLimiter(now, warnEvery)}
}

func (e safeExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.next.ExportSpans(ctx, spans); err != nil {
		e.warn("trace export failed", err)
		return errExportFailed
	}
	return nil
}

func (e safeExporter) Shutdown(ctx context.Context) error {
	if err := e.next.Shutdown(ctx); err != nil {
		e.warn("trace exporter shutdown failed", err)
		return errExporterShutdown
	}
	return nil
}

// warn logs the class of err, at most once per warnEvery for each message and class, with the
// number of warnings it withheld since the last one. The error text is never logged.
func (e safeExporter) warn(msg string, err error) {
	class := exportClass(err)
	if suppressed, ok := e.limit.allow(msg + "/" + class); ok {
		e.log.Warn(msg, "error_type", class, "suppressed", suppressed)
	}
}

// The exporter exposes an HTTP status only in its message. These anchored patterns read the
// three digits and nothing else: a collector's response body follows them and could say anything.
var (
	statusPattern    = regexp.MustCompile(`^(?:traces export: )?failed to send to \S+: ([1-5][0-9][0-9]) `)
	retryablePattern = regexp.MustCompile(`^(?:traces export: )?max retry time elapsed: retry-able request failure: body: `)
)

// exportClass maps an exporter error to a bounded class: refused, timeout, dns, tls, an HTTP
// status such as http_401, http_retryable (the exporter retried 429, 502, 503 or 504 until it
// gave up, and does not say which), cancelled, or error.
func exportClass(err error) string {
	var dns *net.DNSError
	var authority x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case ErrorClass(err) == "timeout":
		return "timeout"
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &authority), errors.As(err, &host), errors.As(err, &invalid):
		return "tls"
	}
	if m := statusPattern.FindStringSubmatch(err.Error()); m != nil {
		return "http_" + m[1]
	}
	if retryablePattern.MatchString(err.Error()) {
		return "http_retryable"
	}
	return "error"
}

// warnLimiter lets each key through once per interval and counts what it withheld.
type warnLimiter struct {
	now   func() time.Time
	every time.Duration
	mu    sync.Mutex
	last  map[string]time.Time
	held  map[string]int
}

func newWarnLimiter(now func() time.Time, every time.Duration) *warnLimiter {
	return &warnLimiter{now: now, every: every, last: map[string]time.Time{}, held: map[string]int{}}
}

// allow reports whether key may log now, and how many it withheld since it last did.
func (l *warnLimiter) allow(key string) (suppressed int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	last, seen := l.last[key]
	if !seen && len(l.last) >= maxWarnKeys {
		return 0, false
	}
	if seen && now.Sub(last) < l.every {
		l.held[key]++
		return 0, false
	}
	suppressed = l.held[key]
	l.last[key], l.held[key] = now, 0
	return suppressed, true
}
