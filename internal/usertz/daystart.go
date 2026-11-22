package usertz

import (
	"errors"
	"fmt"
)

// DefaultDayStartMinute is the day start read for a user who never set one:
// 09:00 (TASK-4699).
const DefaultDayStartMinute = 540

// ParseDayStart reads a 24-hour "HH:MM" day start as a minute of the day. It
// accepts exactly two hour digits 00-23, a colon and two minute digits 00-59:
// no seconds, no spaces, no single-digit hour.
func ParseDayStart(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' || !isDigit(s[0]) || !isDigit(s[1]) || !isDigit(s[3]) || !isDigit(s[4]) {
		return 0, errors.New("day start must be a 24-hour time HH:MM, such as 09:00")
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, errors.New("day start must be a 24-hour time HH:MM between 00:00 and 23:59")
	}
	return h*60 + m, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// FormatDayStart writes a minute of the day as "HH:MM".
func FormatDayStart(minute int) string {
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// DayStart returns the day start for a stored users.day_start_minute: the
// stored minute with set=true, or DefaultDayStartMinute with set=false when it
// is nil or outside 0..1439.
func DayStart(stored *int) (minute int, set bool) {
	if stored != nil && *stored >= 0 && *stored < 1440 {
		return *stored, true
	}
	return DefaultDayStartMinute, false
}
