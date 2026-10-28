package schedule

import (
	"errors"
	"testing"
	"time"
)

func TestParseWall(t *testing.T) {
	ok := []struct {
		in   string
		want Wall
	}{
		{"2025-03-09", Wall{Year: 2025, Month: 3, Day: 9, AllDay: true}},
		{"2025-03-09T02:30", Wall{Year: 2025, Month: 3, Day: 9, Hour: 2, Minute: 30}},
		{"2025-03-09T02:30:15", Wall{Year: 2025, Month: 3, Day: 9, Hour: 2, Minute: 30, Second: 15}},
		{"2024-02-29T23:59:59", Wall{Year: 2024, Month: 2, Day: 29, Hour: 23, Minute: 59, Second: 59}},
		{"1970-01-01", Wall{Year: 1970, Month: 1, Day: 1, AllDay: true}},
		{"2200-12-31", Wall{Year: 2200, Month: 12, Day: 31, AllDay: true}},
	}
	for _, c := range ok {
		got, err := ParseWall(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseWall(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	bad := []string{
		"", "2025-03-09T02:30Z", "2025-03-09T02:30:00Z", "2025-03-09T02:30:00+05:00",
		"2025-03-09T24:00", "2025-03-09T24:00:00", "2025-02-30", "2025-02-29", "2025-13-01",
		"2025-03-09T02:30:00.5", "2025-03-09 02:30", " 2025-03-09", "2025-3-9", "20250309",
		"2025-03-09T25:00", "2025-03-09T02:60", "2025-03-09T02:30:60", "1969-12-31", "2201-01-01",
		"2025-03-09T02:30:00 America/New_York",
	}
	for _, in := range bad {
		_, err := ParseWall(in)
		var me *MalformedError
		if !errors.Is(err, ErrMalformed) || !errors.As(err, &me) || me.Code != CodeWall {
			t.Errorf("ParseWall(%q) err = %v; want CodeWall", in, err)
		}
	}
}

func TestWallStringRoundTrip(t *testing.T) {
	for _, in := range []string{"2025-03-09", "2025-03-09T02:30:00", "1970-01-01T00:00:00", "2200-12-31T23:59:59"} {
		w, err := ParseWall(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.String(); got != in {
			t.Errorf("String() = %q; want %q", got, in)
		}
	}
	// The short timed form normalises to seconds.
	w, _ := ParseWall("2025-03-09T02:30")
	if got := w.String(); got != "2025-03-09T02:30:00" {
		t.Errorf("String() = %q", got)
	}
	_ = time.UTC
}
