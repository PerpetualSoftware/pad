package server

import (
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// viewerZone is the zone the caller's days are computed in: their users.timezone,
// else UTC (TASK-4101). A request with no user (an unauthenticated install, a
// share link) gets UTC with Set=false.
func viewerZone(r *http.Request) usertz.Zone {
	if u := currentUser(r); u != nil {
		return usertz.Resolve(u.Timezone)
	}
	return usertz.Resolve(nil)
}

// viewerToday is the caller's calendar day as YYYY-MM-DD.
func viewerToday(r *http.Request) string {
	return time.Now().In(viewerZone(r).Loc).Format("2006-01-02")
}
