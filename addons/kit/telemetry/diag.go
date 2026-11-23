package telemetry

import (
	"errors"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
)

// InstallDiagnostics routes OpenTelemetry's own diagnostics, its internal logger and its error handler, to log, as
// warnings that carry no values: not the error's text, not the key-value pairs. Left alone, the SDK and the exporter
// write to stderr through the standard library's log, and the exporter's parsers print the input of a malformed
// OTEL_EXPORTER_OTLP_* variable there, where a header's value is a credential.
//
// It sets two process-wide values, the only way to reach those messages (OpenTelemetry keeps its logger and error
// handler as globals). Call it once, at start, before the exporter is built, when trace export is configured. Each
// kind of message logs at most once a minute.
func InstallDiagnostics(log *slog.Logger) {
	limit := newWarnLimiter(time.Now, warnEvery)
	otel.SetLogger(logr.New(diagSink{log: log, limit: limit}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if errors.Is(err, errExportFailed) || errors.Is(err, errExporterShutdown) {
			return // safeExporter already said so, with a finer class
		}
		diagnostic(log, limit, "OpenTelemetry error", err)
	}))
}

// diagnostic logs msg, a logr message (a constant by logr's convention: the values go in the pairs, which are dropped),
// and the class of err, at most once per warnEvery for each msg.
func diagnostic(log *slog.Logger, limit *warnLimiter, msg string, err error) {
	if suppressed, ok := limit.allow(msg); ok {
		attrs := []any{"suppressed", suppressed}
		if err != nil {
			attrs = append(attrs, "error_type", exportClass(err))
		}
		log.Warn("otel: "+msg, attrs...)
	}
}

// diagSink is the logr sink OpenTelemetry's global logger writes to. Only errors and warnings (V levels 0 and 1) are
// kept.
type diagSink struct {
	log   *slog.Logger
	limit *warnLimiter
}

func (diagSink) Init(logr.RuntimeInfo)                   {}
func (diagSink) Enabled(level int) bool                  { return level <= 1 }
func (s diagSink) Info(_ int, msg string, _ ...any)      { diagnostic(s.log, s.limit, msg, nil) }
func (s diagSink) Error(err error, msg string, _ ...any) { diagnostic(s.log, s.limit, msg, err) }
func (s diagSink) WithValues(...any) logr.LogSink        { return s }
func (s diagSink) WithName(string) logr.LogSink          { return s }
