package store

import "strings"

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
	"agenda": true, "needs": true, "tutorials": true,
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

// IsReservedUsername checks if a username is in the reserved list, ignoring case.
func IsReservedUsername(name string) bool {
	return reservedUsernames[strings.ToLower(name)]
}
