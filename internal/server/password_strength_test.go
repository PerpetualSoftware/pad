package server

import (
	"testing"
)

func TestValidatePasswordStrength(t *testing.T) {
	cases := []struct {
		name       string
		password   string
		userInputs []string
		wantErr    bool
	}{
		// Length guardrails
		{"too short", "abc", nil, true},
		{"exactly 8 but weak", "12345678", nil, true},
		{"too long", makeStr(129, 'a'), nil, true},
		{"128 chars ok if strong", makeStr(128, 'a'), nil, true}, // all-'a' is weak but long enough — zxcvbn gives enough score

		// Breached / top-of-list rockyou entries
		{"literally 'password'", "password", nil, true},
		{"'password123'", "password123", nil, true},
		{"'qwerty'", "qwerty", nil, true},
		{"'qwerty1234'", "qwerty1234", nil, true},
		{"'letmein1'", "letmein1", nil, true},
		{"'123456789'", "123456789", nil, true},
		{"'iloveyou'", "iloveyou", nil, true},

		// Context-aware: passwords derived from the user's identity get
		// penalized. An adversary who knows the target's email or name
		// will try these first.
		{"derived from email", "alice@example.com", []string{"alice@example.com", "Alice"}, true},
		{"user name + digits", "Alice2026", []string{"alice@example.com", "Alice"}, true},

		// Acceptable passphrases / random strings
		{"four-word passphrase", "correct-horse-battery-staple", nil, false},
		{"random mixed", "Tr0ub4dor&3-elephant", nil, false},
		{"long mixed case+digits+symbol", "uFLo$82nx-Pqm%1Zz", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePasswordStrength(tc.password, tc.userInputs...)
			if tc.wantErr && err == nil {
				t.Fatalf("validatePasswordStrength(%q) should have rejected; got nil", tc.password)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validatePasswordStrength(%q) should have accepted; got error %q", tc.password, err)
			}
		})
	}
}

// TestValidatePasswordStrength_EmptyUserInputsAreIgnored pins behavior
// around passing empty strings as context: zxcvbn treats each user-input
// string as a banned substring, and "" is a substring of everything, so
// passing "" would make every password fail. We filter empties.
func TestValidatePasswordStrength_EmptyUserInputsAreIgnored(t *testing.T) {
	if err := validatePasswordStrength("correct-horse-battery-staple", "", ""); err != nil {
		t.Fatalf("empty user inputs should be ignored, got error: %v", err)
	}
}

func makeStr(n int, c byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
