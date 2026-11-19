package fakeslack

import (
	"slices"
	"testing"
	"time"
)

func TestHTTPDeliveryDefaultsAndOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []Option
		retries []time.Duration
		timeout time.Duration
	}{
		{"defaults: three retries and Slack's 3 seconds", nil, []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}, 3 * time.Second},
		{"WithHTTPRetries with no wait is no retry", []Option{WithHTTPRetries()}, []time.Duration{}, 3 * time.Second},
		{"options set both", []Option{WithHTTPRetries(time.Millisecond), WithHTTPTimeout(time.Minute)}, []time.Duration{time.Millisecond}, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewHandler(tc.opts...)
			t.Cleanup(s.Close)
			if !slices.Equal(s.web.retries, tc.retries) || s.web.timeout != tc.timeout {
				t.Errorf("retries %v, timeout %v; want %v, %v", s.web.retries, s.web.timeout, tc.retries, tc.timeout)
			}
		})
	}
}
