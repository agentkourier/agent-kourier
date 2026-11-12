package config

import "testing"

func TestReferenceAllowed(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		target  string
		allowed []string
		want    bool
	}{
		{"same namespace needs no list", "sre", "sre", nil, true},
		{"same namespace ignores the list", "sre", "sre", []string{"payments"}, true},
		{"another namespace that is listed", "payments", "sre", []string{"payments"}, true},
		{"another namespace among several", "payments", "sre", []string{"ops", "payments"}, true},
		{"another namespace that is not listed", "payments", "sre", []string{"ops"}, false},
		{"another namespace with no list", "payments", "sre", nil, false},
		{"a list is not a prefix match", "pay", "sre", []string{"payments"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReferenceAllowed(tt.from, tt.target, tt.allowed); got != tt.want {
				t.Errorf("ReferenceAllowed(%q, %q, %v) = %v, want %v", tt.from, tt.target, tt.allowed, got, tt.want)
			}
		})
	}
}
