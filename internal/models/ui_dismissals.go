package models

import (
	"strings"

	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// UIDismissalKeys are the one-time UI suggestions a user can dismiss
// (TASK-3452). The set is closed: the store and the handler refuse any other
// key, so users.ui_dismissals stays a short list of known words.
var UIDismissalKeys = []string{
	// The first-time tutorial card on /console's empty state (Pad Cloud).
	"tutorials.console",
	// The first-time tutorial card on a workspace's setup launchpad.
	"tutorials.launchpad",
}

// TZMismatchPrefix starts the one open key family (TASK-4101): the notice that
// the browser's zone differs from the saved one is dismissed per PAIR, so a new
// pair (a trip, a new device) asks again. The key is
// "tz-mismatch:<settingZone>|<browserZone>", both halves loadable IANA names.
const TZMismatchPrefix = "tz-mismatch:"

// MaxTZMismatchDismissals caps the pair keys one user can hold; the write that
// would exceed it is refused, nothing is evicted.
const MaxTZMismatchDismissals = 100

// IsTZMismatchKey reports whether key is a well-formed tz-mismatch pair key.
func IsTZMismatchKey(key string) bool {
	rest, ok := strings.CutPrefix(key, TZMismatchPrefix)
	if !ok {
		return false
	}
	setting, browser, ok := strings.Cut(rest, "|")
	if !ok {
		return false
	}
	if _, err := usertz.Validate(setting); err != nil {
		return false
	}
	_, err := usertz.Validate(browser)
	return err == nil
}

// IsUIDismissalKey reports whether key is in UIDismissalKeys or is a
// well-formed tz-mismatch pair key.
func IsUIDismissalKey(key string) bool {
	for _, k := range UIDismissalKeys {
		if k == key {
			return true
		}
	}
	return IsTZMismatchKey(key)
}
