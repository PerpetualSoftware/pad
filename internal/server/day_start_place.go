package server

import (
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// resolvedFromDayStart is the resolved_from value of an answer whose bare date
// was placed at the caller's day start (TASK-4699, TASK-4723).
const resolvedFromDayStart = "day_start"

// isBareDate reports whether raw is a bare YYYY-MM-DD and returns its wall.
func isBareDate(raw string) (schedule.Wall, bool) {
	day, err := schedule.ParseWall(raw)
	if err != nil || !day.AllDay {
		return schedule.Wall{}, false
	}
	return day, true
}

// callerDayStartInstant places a bare date at the CALLER's day start (unset
// reads 09:00) in the CALLER's account zone, never the recipient's: reading
// the recipient's would leak their setting and zone through the instant. A
// spring-forward gap shifts the time forward and a fall-back overlap takes the
// earlier instant (schedule.Resolve). The caller checks viewerZone(r).Set
// first; an unset zone is refused, never read as UTC.
func callerDayStartInstant(r *http.Request, day schedule.Wall) time.Time {
	var stored *int
	if u := currentUser(r); u != nil {
		stored = u.DayStartMinute
	}
	minute, _ := usertz.DayStart(stored)
	day.AllDay, day.Hour, day.Minute = false, minute/60, minute%60
	return schedule.Resolve(viewerZone(r).Loc, day)
}
