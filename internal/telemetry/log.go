package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/redact"
)

// MaxLogErrorBytes caps the text of an error attribute in a log line, after redaction. It is the audit log's cap
// for a returned error (internal/audit maxError): room for the context courier wraps an error in ("send message:
// agent: not sent: ...") and the head of what the agent or Slack said, which is the part that diagnoses, but not
// a page of model output or a response body. The text that is cut is not logged anywhere.
const MaxLogErrorBytes = 300

// redactedKeys name attributes that hold error text whatever their type: a string made with err.Error(), a
// recovered panic value, a reason that quotes a cause. An attribute whose value is an error is covered by
// its type, under any key.
var redactedKeys = map[string]bool{"err": true, "error": true, "cause": true, "reason": true, "panic": true}

// NewJSONHandler returns courier's log handler: JSON lines on w at level and above, in which every error attribute
// goes through RedactAttr. It is the only place courier builds one (TestOnlyTheTelemetryPackageBuildsHandlers),
// so no log line carries an error's text unredacted or unbounded.
func NewJSONHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: RedactAttr})
}

// RedactAttr is a slog.HandlerOptions.ReplaceAttr. It replaces the value of an attribute that holds an error, or
// is named err, error, cause, reason or panic, with its text passed through redact.Text and cut to
// MaxLogErrorBytes, so a secret in an error, and the agent's or Slack's own words in one, do not reach the log.
// The attribute keeps its key, and every other attribute, the message, the level and the time are returned as
// they came. The handler calls it for attributes in groups, and for those added with With, as well.
func RedactAttr(_ []string, a slog.Attr) slog.Attr {
	v := a.Value
	switch {
	case v.Kind() == slog.KindAny && isError(v.Any()):
		a.Value = slog.StringValue(LogText(v.Any()))
	case redactedKeys[strings.ToLower(a.Key)] && v.Kind() == slog.KindString:
		a.Value = slog.StringValue(LogText(v.String()))
	case redactedKeys[strings.ToLower(a.Key)] && v.Kind() == slog.KindAny && v.Any() != nil:
		a.Value = slog.StringValue(LogText(v.Any()))
	}
	return a
}

func isError(v any) bool { _, ok := v.(error); return ok }

// LogText returns what a log line may carry of v, an error or any value that stands for an error's text: its
// text, redacted and cut to MaxLogErrorBytes at a rune boundary, the cut marked. A value that cannot print (a
// nil pointer behind an error interface) gives "[unprintable]", not a panic in the logger.
func LogText(v any) (text string) {
	defer func() {
		if recover() != nil {
			text = "[unprintable]"
		}
	}()
	if err, ok := v.(error); ok {
		text = err.Error()
	} else {
		text = fmt.Sprint(v)
	}
	text = redact.Text(text)
	if len(text) <= MaxLogErrorBytes {
		return text
	}
	cut := MaxLogErrorBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + redact.Truncated
}

// LogHandler adds trace and span IDs to contextual logs with valid trace context.
type LogHandler struct{ Handler slog.Handler }

// Enabled delegates to the wrapped handler.
func (h LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, level)
}

// Handle adds IDs without altering the caller's record.
func (h LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs preserves the tracing handler on derived loggers.
func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup preserves the tracing handler on grouped loggers.
func (h LogHandler) WithGroup(name string) slog.Handler {
	return LogHandler{Handler: h.Handler.WithGroup(name)}
}
