package schedule

import (
	"time"

	"github.com/PerpetualSoftware/pad/internal/schedule/internal/rrule"
)

// stallPos is the generator's reported stall position.
func stallPos(rr *rrule.RRule) time.Time { return rr.StallPos() }
