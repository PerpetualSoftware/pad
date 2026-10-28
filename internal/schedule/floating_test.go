package schedule

import (
	"errors"
	"testing"
	"time"
)

func TestPinnedIgnoresViewZone(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-01-06T09:00", "FREQ=DAILY"))
	at := inst(t, "2025-01-06T00:00:00Z")
	var first string
	for _, v := range []*time.Location{nil, mustLoc(t, nyZone), mustLoc(t, "Asia/Tokyo"), mustLoc(t, "America/Los_Angeles")} {
		o, _, err := c.NextAfter(at, v, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = o.At.Format(time.RFC3339)
		}
		if got := o.At.Format(time.RFC3339); got != first || got != "2025-01-06T14:00:00Z" {
			t.Errorf("view %v: %s; want 2025-01-06T14:00:00Z", v, got)
		}
	}
}

func TestFloatingFollowsViewZone(t *testing.T) {
	c := compile(t, Spec{Start: mustWall(t, "2025-01-06T09:00"), Floating: true, Rule: "FREQ=DAILY"})
	at := inst(t, "2025-01-01T00:00:00Z")
	for zone, want := range map[string]string{
		"America/New_York": "2025-01-06T14:00:00Z", "Asia/Tokyo": "2025-01-06T00:00:00Z", "America/Los_Angeles": "2025-01-06T17:00:00Z",
	} {
		o, _, err := c.NextAfter(at, mustLoc(t, zone), Limits{})
		if err != nil || o.At.Format(time.RFC3339) != want {
			t.Errorf("%s: %v %v; want %s", zone, o.At, err, want)
		}
	}
}

func TestFloatingAcrossZoneChange(t *testing.T) {
	c := compile(t, Spec{Start: mustWall(t, "2025-01-06T09:00"), Floating: true, Rule: "FREQ=DAILY"})
	ny, tokyo := mustLoc(t, nyZone), mustLoc(t, "Asia/Tokyo")
	o1, _, _ := c.NextAfter(inst(t, "2025-01-01T00:00:00Z"), ny, Limits{})
	o2, _, _ := c.NextAfter(o1.At, tokyo, Limits{})
	if h := o1.At.In(ny).Hour(); h != 9 {
		t.Errorf("first is %d:00 in New York; want 9", h)
	}
	if h := o2.At.In(tokyo).Hour(); h != 9 || o2.Wall.Day != 7 {
		t.Errorf("second is %v in Tokyo; want 09:00 on the 7th", o2.At.In(tokyo))
	}
}

func TestFloatingDSTInViewZone(t *testing.T) { TestDSTFloatingViewZone(t) }

func TestFloatingRequiresView(t *testing.T) {
	c := compile(t, Spec{Start: mustWall(t, "2025-01-06T09:00"), Floating: true, Rule: "FREQ=DAILY"})
	if _, _, err := c.NextAfter(inst(t, "2025-01-01T00:00:00Z"), nil, Limits{}); !errors.Is(err, ErrViewRequired) {
		t.Errorf("NextAfter err = %v; want ErrViewRequired", err)
	}
	if _, err := c.Between(inst(t, "2025-01-01T00:00:00Z"), inst(t, "2025-01-05T00:00:00Z"), nil, Limits{}); !errors.Is(err, ErrViewRequired) {
		t.Errorf("Between err = %v; want ErrViewRequired", err)
	}
}

func codeOf(t *testing.T, err error) (Code, string) {
	t.Helper()
	var me *MalformedError
	if !errors.As(err, &me) || !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v; want a *MalformedError", err)
	}
	return me.Code, me.Part
}

func TestPinnedRequiresZone(t *testing.T) {
	_, err := Compile(Spec{Start: mustWall(t, "2025-01-06T09:00")})
	if code, part := codeOf(t, err); code != CodeZone || part != "Zone" {
		t.Errorf("got %s/%s; want zone/Zone", code, part)
	}
}

func TestFloatingForbidsZone(t *testing.T) {
	_, err := Compile(Spec{Start: mustWall(t, "2025-01-06T09:00"), Floating: true, Zone: nyZone})
	if code, _ := codeOf(t, err); code != CodeZone {
		t.Errorf("got %s; want zone", code)
	}
}

func TestFloatingZUntilRefused(t *testing.T) {
	_, err := Compile(Spec{Start: mustWall(t, "2025-01-06T09:00"), Floating: true, Rule: "FREQ=DAILY;UNTIL=20250301T000000Z"})
	if code, part := codeOf(t, err); code != CodeCombination || part != "UNTIL" {
		t.Errorf("got %s/%s; want combination/UNTIL", code, part)
	}
}
