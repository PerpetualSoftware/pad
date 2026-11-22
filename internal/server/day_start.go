package server

import "github.com/PerpetualSoftware/pad/internal/usertz"

// dayStartPayload is the "day_start" value the caller's own auth payloads
// carry: "HH:MM" when set, nil (JSON null) when never set (TASK-4699).
func dayStartPayload(stored *int) any {
	if m, set := usertz.DayStart(stored); set {
		return usertz.FormatDayStart(m)
	}
	return nil
}
