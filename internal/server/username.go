package server

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/store"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)
var usernameShortRe = regexp.MustCompile(`^[a-z0-9]$`)

// ValidateUsername checks a username for format, length, and reserved word violations.
// It does NOT check uniqueness (that requires a store call).
func ValidateUsername(username string) error {
	if username == "" {
		return fmt.Errorf("username is required")
	}

	// Must be lowercase
	if username != strings.ToLower(username) {
		return fmt.Errorf("username must be lowercase")
	}

	// Length check
	if len(username) < 3 {
		return fmt.Errorf("username must be at least 3 characters")
	}
	if len(username) > 39 {
		return fmt.Errorf("username must be at most 39 characters")
	}

	// Format check
	if len(username) == 1 {
		if !usernameShortRe.MatchString(username) {
			return fmt.Errorf("username must contain only lowercase letters, numbers, and hyphens")
		}
	} else if !usernameRe.MatchString(username) {
		return fmt.Errorf("username must contain only lowercase letters, numbers, and hyphens, and cannot start or end with a hyphen")
	}

	// No consecutive hyphens
	if strings.Contains(username, "--") {
		return fmt.Errorf("username cannot contain consecutive hyphens")
	}

	// Reserved words
	if IsReservedUsername(username) {
		return fmt.Errorf("username %q is reserved", username)
	}

	return nil
}

// IsReservedUsername checks if a username is in the reserved list.
func IsReservedUsername(username string) bool {
	return store.IsReservedUsername(username)
}
