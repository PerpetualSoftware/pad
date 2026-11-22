package usertz

import "testing"

// TASK-4699: the per-user day start is strict 24-hour HH:MM.
func TestParseDayStart(t *testing.T) {
	for in, want := range map[string]int{"00:00": 0, "09:00": 540, "07:30": 450, "23:59": 1439} {
		got, err := ParseDayStart(in)
		if err != nil || got != want {
			t.Errorf("ParseDayStart(%q) = %d, %v; want %d", in, got, err, want)
		}
		if FormatDayStart(got) != in {
			t.Errorf("FormatDayStart(%d) = %q, want %q", got, FormatDayStart(got), in)
		}
	}
	for _, bad := range []string{"9:00", "24:00", "09:60", "09:00:00", "", "09:00\x00", "\x0009:00", " 09:00", "09:00 ", "0900", "+9:00", "09:5a", "１9:00"} {
		if got, err := ParseDayStart(bad); err == nil {
			t.Errorf("ParseDayStart(%q) accepted as %d", bad, got)
		}
	}
}

func TestDayStart(t *testing.T) {
	if m, set := DayStart(nil); set || m != DefaultDayStartMinute || DefaultDayStartMinute != 540 {
		t.Errorf("DayStart(nil) = %d, %v; want 540, false", m, set)
	}
	v := 450
	if m, set := DayStart(&v); !set || m != 450 {
		t.Errorf("DayStart(450) = %d, %v", m, set)
	}
	for _, bad := range []int{-1, 1440} {
		if m, set := DayStart(&bad); set || m != DefaultDayStartMinute {
			t.Errorf("DayStart(%d) = %d, %v; want the default, unset", bad, m, set)
		}
	}
}
