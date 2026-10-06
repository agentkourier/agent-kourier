package telemetry

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// An error attribute in a log line, under any key that holds one, is normalized and redacted as free text: a secret
// that an invisible character splits does not reach the log, and no invisible character does.
func TestJSONHandlerTakesTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		var buf bytes.Buffer
		l := newTestLogger(&buf)
		l.Error("m", "error", errors.New(c.Text))
		c.CheckKept(t, fmt.Sprint(logLine(t, &buf)["error"]))

		buf.Reset()
		l.Warn("m", "reason", c.Text)
		c.CheckKept(t, fmt.Sprint(logLine(t, &buf)["reason"]))
	}
	for _, b := range redacttest.LineBreaks {
		c := redacttest.Breaks(b)
		var buf bytes.Buffer
		newTestLogger(&buf).Error("m", "error", errors.New(c.Text))
		c.Check(t, fmt.Sprint(logLine(t, &buf)["error"]))
	}
}

// LogText is the one place the text of an error is made fit for a log line, and it takes the corpus out as well.
func TestLogTextTakesTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		c.CheckKept(t, LogText(errors.New(c.Text)))
	}
}

// OpenTelemetry's own messages, which an exporter builds from its configuration, are redacted as free text too.
func TestDiagnosticsTakeTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		var buf bytes.Buffer
		diagnostic(slog.New(slog.NewJSONHandler(&buf, nil)), newWarnLimiter(time.Now, warnEvery), c.Text, nil)
		c.Check(t, fmt.Sprint(logLine(t, &buf)["msg"]))
	}
}
