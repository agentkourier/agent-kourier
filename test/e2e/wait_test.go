package e2e

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

type result struct {
	ok  bool
	err error
}

// steps returns a condition that gives each result in turn, then the last one for ever.
func steps(rs ...result) func(context.Context) (bool, error) {
	i := 0
	return func(context.Context) (bool, error) {
		r := rs[min(i, len(rs)-1)]
		i++
		return r.ok, r.err
	}
}

func TestWait(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		timeout time.Duration
		cond    func(context.Context) (bool, error)
		is, not error // the error must match is (nil: no error at all) and must not match not
	}{
		{"holds on the third check", context.Background(), 5 * time.Second, steps(result{}, result{}, result{ok: true}), nil, nil},
		{"rides out an error", context.Background(), 5 * time.Second, steps(result{err: errBoom}, result{ok: true}), nil, nil},
		{"times out", context.Background(), 40 * time.Millisecond, steps(result{}), ErrTimeout, nil},
		{"times out with the last error", context.Background(), 40 * time.Millisecond, steps(result{err: errBoom}), errBoom, nil},
		{"true with an error is not success", context.Background(), 40 * time.Millisecond, steps(result{true, errBoom}), ErrTimeout, nil},
		{"a cancelled caller is not a timeout", cancelled, 5 * time.Second, steps(result{}), context.Canceled, ErrTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Wait(tc.ctx, tc.timeout, "the thing", tc.cond)
			if tc.is == nil && err != nil || tc.is != nil && !errors.Is(err, tc.is) || tc.not != nil && errors.Is(err, tc.not) {
				t.Fatalf("Wait = %v, want is=%v not=%v", err, tc.is, tc.not)
			}
		})
	}
}
