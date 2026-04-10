package server

import (
	"fmt"
	"regexp"
	"strings"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)
var usernameShortRe = regexp.MustCompile(`^[a-z0-9]$`)

// reservedUsernames contains words that cannot be used as usernames because they
// conflict with existing or planned URL routes, system paths, or common terms.
// Decision D11: "be generous — better to reserve too many than too few."
var reservedUsernames = map[string]bool{
	// Auth routes
	"login": true, "register": true, "join": true, "signup": true, "signin": true,
	"forgot-password": true, "reset-password": true,
	"logout": true, "auth": true, "oauth": true, "sso": true,

	// App routes
	"settings": true, "admin": true, "console": true,
	"dashboard": true, "api": true, "health": true,
	"new": true, "create": true, "edit": true, "delete": true,

	// Share/special routes
	"s": true, "share": true, "shared": true,
	"invite": true, "invitations": true,

	// System
	"app": true, "www": true, "mail": true, "ftp": true, "smtp": true,
	"static": true, "assets": true, "public": true, "cdn": true,
	"pad": true, "system": true, "root": true, "server": true,
	"null": true, "undefined": true, "nan": true, "true": true, "false": true,

	// Common reserved
	"about": true, "help": true, "support": true, "contact": true,
	"docs": true, "blog": true, "pricing": true, "plans": true,
	"terms": true, "privacy": true, "security": true, "legal": true,
	"status": true, "changelog": true, "updates": true,
	"home": true, "index": true, "search": true, "explore": true,
	"notifications": true, "messages": true, "feed": true,
	"account": true, "profile": true, "user": true, "users": true,
	"org": true, "orgs": true, "team": true, "teams": true,
	"workspace": true, "workspaces": true,
	"project": true, "projects": true,
}

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
	if reservedUsernames[username] {
		return fmt.Errorf("username %q is reserved", username)
	}

	return nil
}

// IsReservedUsername checks if a username is in the reserved list.
func IsReservedUsername(username string) bool {
	return reservedUsernames[strings.ToLower(username)]
}
