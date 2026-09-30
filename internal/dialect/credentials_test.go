package dialect_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// Fake secrets. Every formatting path below must print neither of them.
const (
	tokenCanary  = "CANARY-fake-token-0000"
	headerCanary = "CANARY-fake-header-1111"
)

func newCreds() dialect.Credentials {
	return dialect.Credentials{
		Token:   secret.New(tokenCanary),
		Headers: dialect.Headers{"X-Api-Key": {secret.New(headerCanary)}},
	}
}

// leaks reports whether s holds a canary as text, hex (%x), or a byte list.
func leaks(s string) bool {
	for _, c := range []string{tokenCanary, headerCanary} {
		for _, needle := range []string{c, hex.EncodeToString([]byte(c)), strings.ToUpper(hex.EncodeToString([]byte(c))), fmt.Sprintf("%v", []byte(c))} {
			if strings.Contains(s, needle) {
				return true
			}
		}
	}
	return false
}

// Shapes that hold Credentials the ways a struct in the broker might. fmt cannot call
// methods on an unexported field, so those shapes are the ones that leaked.
type (
	exportedField   struct{ C dialect.Credentials }
	unexportedField struct{ c dialect.Credentials }
	unexportedPtr   struct{ c *dialect.Credentials }
	unexportedSlice struct{ c []dialect.Credentials }
	unexportedMap   struct {
		c map[string]dialect.Credentials
	}
	unexportedParts struct { // the token and the headers on their own
		token   any
		headers any
	}
	// connError is an error that keeps Credentials in a field and formats them in its text.
	connError struct{ creds dialect.Credentials }
)

func (e connError) Error() string {
	return fmt.Sprintf("connect %+v", struct{ c dialect.Credentials }{e.creds})
}

func subjects() map[string]any {
	c := newCreds()
	subs := map[string]any{
		"value":                    c,
		"pointer":                  &c,
		"exported field":           exportedField{c},
		"pointer to exported":      &exportedField{c},
		"unexported field":         unexportedField{c},
		"pointer to unexported":    &unexportedField{c},
		"unexported pointer field": unexportedPtr{&c},
		"slice":                    []dialect.Credentials{c},
		"slice of pointers":        []*dialect.Credentials{&c},
		"unexported slice":         unexportedSlice{[]dialect.Credentials{c}},
		"map":                      map[string]dialect.Credentials{"a": c},
		"map of pointers":          map[string]*dialect.Credentials{"a": &c},
		"unexported map":           unexportedMap{map[string]dialect.Credentials{"a": c}},
		"token":                    c.Token,
		"headers":                  c.Headers,
		"unexported token+headers": unexportedParts{c.Token, c.Headers},
		"unexported header values": unexportedParts{headers: c.Headers["X-Api-Key"]},
		"anonymous struct":         struct{ c dialect.Credentials }{c},
	}
	// Error chains: text built by formatting the shapes, and the shapes as error fields.
	subs["error text"] = errors.Join(errors.New("first"), fmt.Errorf("connect %v: %w", unexportedField{c}, errors.New("boom")))
	subs["error holding credentials"] = errors.Join(errors.New("first"), connError{c})
	return subs
}

// TestCredentialsCanary formats every shape with every verb, in error chains and in slog,
// and fails when a canary shows.
func TestCredentialsCanary(t *testing.T) {
	handlers := map[string]func(io.Writer) slog.Handler{
		"text": func(w io.Writer) slog.Handler { return slog.NewTextHandler(w, nil) },
		"json": func(w io.Writer) slog.Handler { return slog.NewJSONHandler(w, nil) },
	}
	for name, v := range subjects() {
		t.Run(name, func(t *testing.T) {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
				if got := fmt.Sprintf(verb, v); leaks(got) {
					t.Errorf("Sprintf(%q) shows a secret: %s", verb, got)
				}
				err := errors.Join(errors.New("x"), fmt.Errorf("wrap "+verb+": %w", v, errors.New("boom")))
				if got := fmt.Sprintf(verb, err); leaks(got) {
					t.Errorf("Sprintf(%q) of an errors.Join shows a secret: %s", verb, got)
				}
				if leaks(err.Error()) {
					t.Errorf("errors.Join error text (%s) shows a secret: %s", verb, err)
				}
			}
			for hname, mk := range handlers {
				for lname, log := range map[string]func(*slog.Logger){
					"attr":      func(l *slog.Logger) { l.Info("m", "c", v) },
					"With":      func(l *slog.Logger) { l.With("c", v).Info("m") },
					"WithGroup": func(l *slog.Logger) { l.WithGroup("g").Info("m", "c", v) },
					"Group":     func(l *slog.Logger) { l.Info("m", slog.Group("g", slog.Any("c", v))) },
					"nested": func(l *slog.Logger) {
						l.WithGroup("a").With(slog.Group("b", "c", v)).WithGroup("d").Info("m", slog.Group("e", slog.Any("c", v)))
					},
					"slog.Any/Go": func(l *slog.Logger) { l.Info("m", slog.Any("c", []any{v, map[string]any{"k": v}})) },
				} {
					var out bytes.Buffer
					log(slog.New(mk(&out)))
					if leaks(out.String()) {
						t.Errorf("slog %s handler, %s shows a secret: %s", hname, lname, out.String())
					}
				}
			}
		})
	}
}

// The canary check must see a secret that is there, or the test above proves nothing.
func TestCanaryDetectorSeesAPlainString(t *testing.T) {
	plain := struct{ token string }{tokenCanary}
	for _, verb := range []string{"%v", "%+v", "%s", "%q", "%x", "%X", "%d"} {
		if got := fmt.Sprintf(verb, plain); !leaks(got) {
			t.Errorf("Sprintf(%q) of a plain string = %s; the detector missed it", verb, got)
		}
	}
}
