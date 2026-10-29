package cli

import (
	"testing"
	"time"
)

func TestFormatInstantNeverLocal(t *testing.T) {
	t.Setenv("TZ", "UTC")
	old := time.Local
	time.Local = time.FixedZone("Machine", 9*3600)
	t.Cleanup(func() { time.Local = old; SetDisplayLocationResolver(nil) })
	instant := time.Date(2026, 1, 15, 18, 30, 0, 0, time.UTC)
	for _, resolver := range []func() *time.Location{nil, func() *time.Location { return nil }} {
		SetDisplayLocationResolver(resolver)
		if got := FormatInstant(instant, "15:04"); got != "18:30" {
			t.Fatalf("UTC fallback = %s", got)
		}
	}
	ny, _ := time.LoadLocation("America/New_York")
	SetDisplayLocationResolver(func() *time.Location { return ny })
	if got := FormatInstant(instant, "15:04"); got != "13:30" {
		t.Fatalf("user time = %s", got)
	}
}

func TestRelativeTimeOldDateInZone(t *testing.T) {
	t.Setenv("TZ", "UTC")
	old := time.Local
	time.Local = time.FixedZone("Machine", -9*3600)
	t.Cleanup(func() { time.Local = old; SetDisplayLocationResolver(nil) })
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	SetDisplayLocationResolver(func() *time.Location { return tokyo })
	d := time.Now().UTC().AddDate(0, 0, -9)
	instant := time.Date(d.Year(), d.Month(), d.Day(), 23, 30, 0, 0, time.UTC)
	if got, want := RelativeTime(instant), instant.In(tokyo).Format("Jan 2, 2006"); got != want {
		t.Fatalf("old instant = %s, want %s", got, want)
	}
}
