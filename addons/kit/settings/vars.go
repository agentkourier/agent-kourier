package settings

import (
	"errors"
	"fmt"
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
	m map[string]any
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

// From is the variable's value in values, or the zero T when Parse did not read it.
func (o Of[T]) From(values Values) T {
	v, _ := values.m[o.key].(T)
	return v
}

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
