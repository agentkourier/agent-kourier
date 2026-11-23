package settings

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Var is one of an add-on's own variables, which Parse reads before the shared ones. Make one with Required, Duration
// or Func, and read its value with its From.
type Var interface {
	// Key is the variable's name.
	Key() string
	// read parses the variable into values, or says why it cannot.
	read(getenv func(string) string, values Values) error
}

// Values are the add-on's own variables as Parse read them. A Var reads its own with From.
type Values struct {
	// m holds each Var's value by its key; declared, the keys of every Var in Spec.Vars, read or not; undeclared, the
	// keys of the Vars From read while they were not declared.
	m          map[string]any
	declared   map[string]bool
	undeclared *sync.Map // safe for From in any goroutine
}

// Undeclared returns, in key order, the keys of the Vars whose From read values while Spec.Vars did not declare them:
// each got its zero value. A Var that a parse reads before Parse has come to it in Spec.Vars counts too. The kit's
// runtime refuses to start an add-on whose backend read one.
func (v Values) Undeclared() []string {
	var keys []string
	if v.undeclared != nil {
		v.undeclared.Range(func(k, _ any) bool { keys = append(keys, k.(string)); return true })
	}
	slices.Sort(keys)
	return keys
}

// Of is a Var whose value is a T.
type Of[T any] struct {
	key, def string
	parse    func(v string, earlier Values) (T, error)
}

// Func is the variable key, which parse reads. parse is given the variable's value, or def when it is unset or empty,
// and the values of the Vars before it in Spec.Vars (a Var that failed reads as its zero value). Its error is the
// whole message, so it names the variable, and it never shows a value that may be a secret.
func Func[T any](key, def string, parse func(v string, earlier Values) (T, error)) Of[T] {
	return Of[T]{key: key, def: def, parse: parse}
}

// Required is a variable that must be set, read as it is.
func Required(key string) Of[string] {
	return Func(key, "", func(v string, _ Values) (string, error) {
		if v == "" {
			return "", fmt.Errorf("%s is required", key)
		}
		return v, nil
	})
}

// Duration is a Go duration of zero or more, def when unset, as TASK_RETENTION is read.
func Duration(key, def string) Of[time.Duration] {
	return Func(key, def, func(v string, _ Values) (time.Duration, error) {
		d, err := duration(v, false)
		if err != nil {
			return 0, fmt.Errorf("%s %q %w", key, v, err)
		}
		return d, nil
	})
}

// Key is the variable's name.
func (o Of[T]) Key() string { return o.key }

// From is the variable's value in values. It is the zero T, with no error, for a Var that was not in the Spec.Vars
// of the Parse that returned values, and a zero value often means something: a TOOL_RESULT_TIMEOUT of 0 waits for
// ever, where its default is 10m. A Var left out of Spec.Vars by mistake is not seen at start, so a test of the
// add-on should check Declared for each Var it reads; values records the read (Values.Undeclared).
func (o Of[T]) From(values Values) T {
	if values.undeclared != nil && !values.declared[o.key] {
		values.undeclared.Store(o.key, true)
	}
	v, _ := values.m[o.key].(T)
	return v
}

// Declared reports whether the Var was in the Spec.Vars of the Parse that returned values, and so whether From reads
// what Parse read.
func (o Of[T]) Declared(values Values) bool { return values.declared[o.key] }

func (o Of[T]) read(getenv func(string) string, values Values) error {
	if o.parse == nil {
		return errors.New("settings: " + o.key + " has no parse function")
	}
	v := getenv(o.key)
	if v == "" {
		v = o.def
	}
	parsed, err := o.parse(v, values)
	if err != nil {
		return err
	}
	values.m[o.key] = parsed
	return nil
}
