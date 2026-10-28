// Package usertz resolves the IANA time zone a user has chosen for the days
// Pad computes on their behalf (overdue, standup, an auto-stamped start date).
// The setting lives on users.timezone; an unset or unreadable value computes in
// UTC, the one zone every server and every viewer agrees on (TASK-4101).
package usertz

import (
	"errors"
	"strings"
	"time"
)

// MaxLen bounds a zone name; the longest IANA name is well under it.
const MaxLen = 64

// Zone is a resolved viewer zone.
type Zone struct {
	Name string         // the IANA name computed in; "UTC" when unset
	Loc  *time.Location // never nil
	Set  bool           // true only when the user chose a zone that loads
}

// Validate loads name as an IANA zone. It refuses the empty string, "Local"
// (the server's own zone, not the user's), NUL, and names past MaxLen.
func Validate(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, errors.New("timezone must be an IANA name, such as America/Los_Angeles")
	}
	if len(name) > MaxLen || strings.ContainsRune(name, 0) {
		return nil, errors.New("timezone is not a valid IANA name")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, errors.New("timezone is not a known IANA zone")
	}
	return loc, nil
}

// Resolve returns the zone for a stored users.timezone value. Nil, empty or a
// value that no longer loads is UTC with Set=false.
func Resolve(stored *string) Zone {
	if stored != nil {
		if loc, err := Validate(*stored); err == nil {
			return Zone{Name: *stored, Loc: loc, Set: true}
		}
	}
	return Zone{Name: "UTC", Loc: time.UTC}
}

// Note is the sentence that explains an unset zone, or "" when one is set.
func Note(z Zone) string {
	if z.Set {
		return ""
	}
	return "No time zone is set, so days are computed in UTC. Set yours in the web UI settings or with `pad auth timezone <IANA>`."
}
