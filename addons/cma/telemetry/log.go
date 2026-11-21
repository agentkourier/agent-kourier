package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// MaxLogErrorBytes caps the text of an error attribute in a log line, as Agent Kourier caps its own: an error from the
// Managed Agents API or the database can carry a response body, and a session error carries the agent's own words.
const MaxLogErrorBytes = 300

// cutMark ends a text that MaxLogErrorBytes cut.
const cutMark = "…"

// errorKeys are the attribute keys whose string value is an error's text, or a message the adapter did not write.
var errorKeys = map[string]bool{"err": true, "error": true, "cause": true, "reason": true, "message": true}

// NewLogHandler returns the adapter's log handler: JSON lines on w at level and above, with every duration written as
// text ("10m0s", not nanoseconds) and every error attribute (an error value, or a string under an error key) cut to
// MaxLogErrorBytes at a rune boundary. It is the only handler the adapter builds.
func NewLogHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: replaceAttr})
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	v := a.Value
	switch {
	case v.Kind() == slog.KindDuration:
		a.Value = slog.StringValue(v.Duration().String())
	case v.Kind() == slog.KindAny:
		if err, ok := v.Any().(error); ok {
			a.Value = slog.StringValue(LogText(err))
		}
	case v.Kind() == slog.KindString && errorKeys[strings.ToLower(a.Key)]:
		a.Value = slog.StringValue(LogText(v.String()))
	}
	return a
}

// LogText is what a log line may carry of v, an error or a text: its text cut to MaxLogErrorBytes at a rune boundary,
// the cut marked. A value that cannot print (a nil pointer behind an error interface) gives "[unprintable]".
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
	if len(text) <= MaxLogErrorBytes {
		return text
	}
	cut := MaxLogErrorBytes - len(cutMark)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + cutMark
}
