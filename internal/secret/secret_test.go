package secret

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// canary is the value under test; it must never come back out of a formatting path.
const canary = "s3cr3t-value-canary"

// exposed has an exported and an unexported Value field. fmt cannot call methods on an
// unexported field, so it prints the field's representation instead; that must not hold the
// value either.
type exposed struct {
	Name   string
	Public Value
	hidden Value
}

func TestReveal(t *testing.T) {
	if got := New(canary).Reveal(); got != canary {
		t.Errorf("Reveal = %q, want %q", got, canary)
	}
	if got := (Value{}).Reveal(); got != "" {
		t.Errorf("zero Value Reveal = %q, want empty", got)
	}
}

func TestFormattingRedacts(t *testing.T) {
	v := New(canary)
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%10s", "%-10v", "%.3s", "%08x"}
	for _, verb := range verbs {
		for name, arg := range map[string]any{"value": v, "pointer": &v} {
			got := fmt.Sprintf(verb, arg)
			if strings.Contains(got, canary) || strings.Contains(got, fmt.Sprintf("%x", canary)) {
				t.Errorf("Sprintf(%q, %s) = %q, contains the value", verb, name, got)
			}
			if !strings.Contains(got, "redacted") {
				t.Errorf("Sprintf(%q, %s) = %q, want it redacted", verb, name, got)
			}
		}
	}
	if got := v.String(); got != redacted {
		t.Errorf("String = %q, want %q", got, redacted)
	}
	if got := v.GoString(); got != redacted {
		t.Errorf("GoString = %q, want %q", got, redacted)
	}
}

func TestStructContainingValue(t *testing.T) {
	x := exposed{Name: "cfg", Public: New(canary), hidden: New(canary)}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for name, arg := range map[string]any{"struct": x, "pointer": &x, "slice": []exposed{x}, "map": map[string]exposed{"k": x}, "any": any(x)} {
			if got := fmt.Sprintf(verb, arg); strings.Contains(got, canary) {
				t.Errorf("Sprintf(%q, %s) = %q, contains the value", verb, name, got)
			}
		}
	}
	if got := fmt.Sprintf("%+v", x); !strings.Contains(got, "Public:[redacted]") {
		t.Errorf("%%+v = %q, want the exported field redacted", got)
	}
}

func TestErrorsDoNotLeak(t *testing.T) {
	v := New(canary)
	inner := fmt.Errorf("auth failed for %v and %+v and %#v and %s", v, v, v, v)
	errs := map[string]error{
		"errorf":  inner,
		"wrapped": fmt.Errorf("call agent: %w", inner),
		"joined":  errors.Join(inner, fmt.Errorf("again %q", v)),
		"struct":  fmt.Errorf("config %+v", exposed{Public: v, hidden: v}),
	}
	for name, err := range errs {
		if strings.Contains(err.Error(), canary) {
			t.Errorf("%s: error %q contains the value", name, err)
		}
	}
}

func TestJSONAndText(t *testing.T) {
	v := New(canary)
	for name, in := range map[string]any{"value": v, "pointer": &v, "struct": exposed{Public: v, hidden: v}, "map": map[string]Value{"k": v}} {
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(b), canary) {
			t.Errorf("%s: JSON %s contains the value", name, b)
		}
	}
	if b, _ := json.Marshal(v); string(b) != `"[redacted]"` {
		t.Errorf("JSON = %s, want the redaction string", b)
	}
	b, err := v.MarshalText()
	if err != nil || string(b) != redacted {
		t.Errorf("MarshalText = %q, %v", b, err)
	}
}

func TestSlogRedacts(t *testing.T) {
	v := New(canary)
	x := exposed{Name: "cfg", Public: v, hidden: v}
	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	}
	for name, newHandler := range handlers {
		var buf bytes.Buffer
		log := slog.New(newHandler(&buf)).With("bound", v)
		log.Info("read", "token", v, "ptr", &v, "cfg", x, "group", slog.GroupValue(slog.Any("inner", v)), "err", fmt.Errorf("bad %v", v))
		out := buf.String()
		if strings.Contains(out, canary) {
			t.Errorf("%s handler output contains the value: %s", name, out)
		}
		if !strings.Contains(out, "redacted") {
			t.Errorf("%s handler output = %s, want it redacted", name, out)
		}
	}
}
