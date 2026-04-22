package server

import (
	"fmt"

	"github.com/trustelem/zxcvbn"
)

// passwordStrengthMinScore is the minimum acceptable zxcvbn score
// (0-4) for new or rotated passwords. Scores map roughly to:
//
//	0 — too guessable (top-1k breach list)                         reject
//	1 — very guessable (common patterns, short)                    reject
//	2 — somewhat guessable (protection from online attacks)        accept
//	3 — safely unguessable (protection from offline attacks)       accept
//	4 — very unguessable (long unique password or passphrase)      accept
//
// 2 is the OWASP-recommended floor for user-visible forms and what the
// zxcvbn paper itself names as "adequate for online attack scenarios".
const passwordStrengthMinScore = 2

// validatePasswordStrength enforces length + strength. It rejects weak
// passwords at registration / rotation / reset so top-of-breach-list
// entries like "password", "123456", and "qwerty" can't silently land
// in a fresh account.
//
// User-specific context (email, name) is fed into zxcvbn so the library
// can penalize passwords derived from the owner's own identity (common
// attack vector in credential-spraying).
//
// Returns nil if the password is acceptable. Otherwise returns an
// error whose Error() is safe to surface to the user — it contains no
// reflected input.
func validatePasswordStrength(password string, userInputs ...string) error {
	// Keep the length guardrails; zxcvbn doesn't enforce an upper bound
	// and a 1MB POST with a multi-megabyte password should fail fast
	// before the scorer spends CPU on it.
	if len(password) < 8 {
		return fmt.Errorf("Password must be at least 8 characters")
	}
	if len(password) > 128 {
		return fmt.Errorf("Password must be at most 128 characters")
	}

	// Filter out empty context strings — zxcvbn treats "" as a banned
	// substring which incorrectly weakens every password.
	var context []string
	for _, s := range userInputs {
		if s != "" {
			context = append(context, s)
		}
	}

	result := zxcvbn.PasswordStrength(password, context)
	if result.Score < passwordStrengthMinScore {
		return fmt.Errorf("Password is too weak — try a longer passphrase or add unusual characters")
	}
	return nil
}
