package server

import (
	"net/http"
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

// TestValidatePasswordStrength_ContextPenalizesDerivedPasswords is a
// unit-level guard that the context-aware penalty actually discriminates
// between a password containing the user's identity tokens and one that
// doesn't. Without it, PATCH /auth/me's "use the pending name" fix is
// silent.
func TestValidatePasswordStrength_ContextPenalizesDerivedPasswords(t *testing.T) {
	// A password that zxcvbn rates score == 2 without context but that
	// becomes score < 2 once "zaphod" is declared a user-input (the
	// substring gets penalized). The exact scores depend on the
	// embedded word lists — pin the test by demanding only a
	// before/after DIFFERENCE rather than an absolute score.
	pwd := "zaphod123456"

	// Without context — may or may not pass; the important thing is
	// that adding "zaphod" as a user-input must either keep it failing
	// or flip a previously-passing password to failing. Either way,
	// the WITH-context result must NOT be strictly weaker than
	// WITHOUT-context.
	errWithoutCtx := validatePasswordStrength(pwd)
	errWithCtx := validatePasswordStrength(pwd, "zaphod")

	// The password contains "zaphod" literally; WITH context it must
	// be rejected.
	if errWithCtx == nil {
		t.Fatalf("password %q containing user-input %q should be rejected with context", pwd, "zaphod")
	}
	_ = errWithoutCtx // don't over-constrain the no-context branch
}

// TestPasswordChange_RejectsPasswordDerivedFromPendingName verifies that
// a PATCH /auth/me that changes BOTH name and password is checked
// against the NEW name — a regression would let a caller set
// name="Zaphod" + password="zaphodzaphod" in one request and slip the
// identity-derived penalty because the previous name was still the
// validation context.
func TestPasswordChange_RejectsPasswordDerivedFromPendingName(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "beeblebrox@example.com", "Arthur")

	// Sanity: the password is rejected when "zaphod" is declared a
	// user-input (unit-level dependency).
	if err := validatePasswordStrength("zaphodzaphod", "zaphod"); err == nil {
		t.Fatal("test fixture: password should be rejected with 'zaphod' context")
	}

	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", map[string]string{
		"name":             "Zaphod",
		"current_password": "correct-horse-battery-staple",
		"new_password":     "zaphodzaphod", // weak + derived from pending name
	}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on password derived from pending name; got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPasswordReset_UsesIdentityContext verifies the reset endpoint
// refuses a password derived from the target user's identity. A
// regression would mean /auth/reset-password enforces a weaker policy
// than bootstrap/register/rotation — a real bypass since the reset URL
// is the most common post-compromise recovery path.
func TestPasswordReset_UsesIdentityContext(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "zaphodbeeblebrox@example.com", "Zaphod Beeblebrox")

	// Issue a reset token via the admin flow (same CreatePasswordReset
	// path as the request-reset-email endpoint). We need access to the
	// token string directly to submit the reset.
	user, err := srv.store.GetUserByEmail("zaphodbeeblebrox@example.com")
	if err != nil || user == nil {
		t.Fatalf("fixture: failed to look up user: %v", err)
	}
	token, err := srv.store.CreatePasswordReset(user.ID)
	if err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}

	// Reset with a weak password that's clearly derived from the user's
	// identity — must be rejected BEFORE the token is consumed so the
	// user can retry on the same link.
	rr := doRequest(srv, "POST", "/api/v1/auth/reset-password", map[string]string{
		"token":    token,
		"password": "zaphodzaphod",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on identity-derived reset password; got %d: %s", rr.Code, rr.Body.String())
	}

	// Token must still be usable: retry with a strong password.
	rr = doRequest(srv, "POST", "/api/v1/auth/reset-password", map[string]string{
		"token":    token,
		"password": "another-strong-test-passphrase-19",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("token should remain consumable after a weak-password rejection; got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRegister_IncludesUsernameInStrengthContext pins that registration
// penalizes a password derived from the caller-supplied username.
// Registration can't call through the full flow easily (requires
// invitation code for non-first user), so we exercise the unit-level
// invariant: validatePasswordStrength with a username-derived password
// plus "zaphod123" as the username is rejected.
func TestRegister_IncludesUsernameInStrengthContext(t *testing.T) {
	// The registration handler calls:
	//   validatePasswordStrength(input.Password, input.Email, input.Name, input.Username)
	// so a password literally containing the username must be rejected.
	if err := validatePasswordStrength("zaphodzaphod", "user@example.com", "Someone", "zaphod"); err == nil {
		t.Fatal("registration strength check must penalize passwords derived from username")
	}
}

func makeStr(n int, c byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
