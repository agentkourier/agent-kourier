package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestLogHandler(t *testing.T) {
	long := strings.Repeat("é", MaxLogErrorBytes) // two bytes a rune, so a byte cut would split one
	tests := []struct {
		name string
		attr slog.Attr
		want string
	}{
		{"duration as text", slog.Duration("timeout", 10*time.Minute), "10m0s"},
		{"short error kept", slog.Any("error", errors.New("boom")), "boom"},
		{"long error cut", slog.Any("error", errors.New(long)), ""},
		{"long message cut", slog.String("message", long), ""},
		{"other strings kept", slog.String("session", long), long},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(NewLogHandler(&buf, slog.LevelInfo)).Info("x", tt.attr)
			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatal(err)
			}
			got, _ := line[tt.attr.Key].(string)
			if tt.want != "" {
				if got != tt.want {
					t.Fatalf("%s = %q, want %q", tt.attr.Key, got, tt.want)
				}
				return
			}
			if len(got) > MaxLogErrorBytes || !strings.HasSuffix(got, cutMark) || !utf8.ValidString(got) {
				t.Fatalf("%s is %d bytes, valid=%v, ends %q; want at most %d bytes, valid UTF-8, ending %q",
					tt.attr.Key, len(got), utf8.ValidString(got), got[max(0, len(got)-6):], MaxLogErrorBytes, cutMark)
			}
		})
	}
}

func TestLogTextUnprintable(t *testing.T) {
	var p *pathError
	if got := LogText(error(p)); got != "[unprintable]" {
		t.Fatalf("LogText(nil *pathError) = %q", got)
	}
}

type pathError struct{ path string }

func (e *pathError) Error() string { return e.path }
