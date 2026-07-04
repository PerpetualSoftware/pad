package models

import "testing"

// TestIsEmailVerified locks in the mirror-of-IsDisabled contract: a non-empty
// EmailVerifiedAt means verified, empty (the zero value / NULL round-trip)
// means unverified. PLAN-1933 / TASK-1935.
func TestIsEmailVerified(t *testing.T) {
	cases := []struct {
		name            string
		emailVerifiedAt string
		want            bool
	}{
		{"empty is unverified", "", false},
		{"timestamp is verified", "2026-07-04T12:00:00Z", true},
		{"any non-empty string is verified", "x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := &User{EmailVerifiedAt: c.emailVerifiedAt}
			if got := u.IsEmailVerified(); got != c.want {
				t.Errorf("IsEmailVerified() with EmailVerifiedAt=%q = %v, want %v", c.emailVerifiedAt, got, c.want)
			}
		})
	}
}
