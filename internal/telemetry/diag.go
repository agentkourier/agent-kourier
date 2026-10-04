package telemetry

import (
	"errors"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"

	"github.com/agentkourier/agent-kourier/internal/redact"
)

// InstallDiagnostics routes OpenTelemetry's own diagnostics, its internal logger and its error
// handler, to log, as warnings that carry no values: not the error text, not the key-value pairs.
// Left alone, the SDK and the exporter write to stderr through the standard library log, outside
// slog and its redaction; the exporter's parsers print the offending input of a malformed
// OTEL_EXPORTER_OTLP_* variable there, and a header value is a credential.
//
// It sets two process-wide values, which is the only way to reach those messages: OpenTelemetry
// keeps its logger and error handler as globals and offers no per-provider hook. They are the
// one exception to "no global state" besides the version, set once at startup, when trace export is configured (broker.Run, with
// RouteKlog, before the exporter is built), and never changed. Each kind of message logs at most
// once a minute, so a stuck exporter cannot flood the log.
func InstallDiagnostics(log *slog.Logger) {
	limit := newWarnLimiter(time.Now, warnEvery)
	otel.SetLogger(diagLogger(log, limit))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if errors.Is(err, errExportFailed) || errors.Is(err, errExporterShutdown) {
			return // safeExporter already said so, with a finer class
		}
		diagnostic(log, limit, "OpenTelemetry error", err)
	}))
}

func diagLogger(log *slog.Logger, limit *warnLimiter) logr.Logger {
	return logr.New(diagSink{log: log, limit: limit})
}

// diagnostic logs msg and the class of err, at most once per warnEvery for each distinct msg.
func diagnostic(log *slog.Logger, limit *warnLimiter, msg string, err error) {
	msg = redact.Text(msg)
	if suppressed, ok := limit.allow(msg); ok {
		attrs := []any{"suppressed", suppressed}
		if err != nil {
			attrs = append(attrs, "error_type", ErrorClass(err))
		}
		log.Warn("otel: "+msg, attrs...)
	}
}

// diagSink is the logr sink OpenTelemetry's global logger writes to. Only errors and warnings
// (V levels 0 and 1) are kept; its info and debug levels are off.
type diagSink struct {
	log   *slog.Logger
	limit *warnLimiter
}

func (diagSink) Init(logr.RuntimeInfo) {}

func (diagSink) Enabled(level int) bool { return level <= 1 }

func (s diagSink) Info(_ int, msg string, _ ...any) { diagnostic(s.log, s.limit, msg, nil) }

func (s diagSink) Error(err error, msg string, _ ...any) { diagnostic(s.log, s.limit, msg, err) }

func (s diagSink) WithValues(...any) logr.LogSink { return s }

func (s diagSink) WithName(string) logr.LogSink { return s }
