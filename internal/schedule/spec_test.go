package schedule

import (
	"strings"
	"testing"
)

// TestCompileNeverReturnsLibraryError feeds Compile every malformed shape the
// package knows, plus hostile text, and checks each refusal is ours: a
// *MalformedError carrying none of the expansion library's wording.
func TestCompileNeverReturnsLibraryError(t *testing.T) {
	rules := []string{
		"COUNT=3", "FREQ=FORTNIGHTLY", "FREQ=DAILY;FOO=1", "FREQ=DAILY;COUNT=2;COUNT=3", "FREQ=DAILY;COUNT=3;UNTIL=19971224T000000Z",
		"COUNT=0", "INTERVAL=0", "FREQ=YEARLY;BYMONTH=13", "FREQ=WEEKLY;BYMONTHDAY=1", "FREQ=MONTHLY;BYYEARDAY=1", "FREQ=MONTHLY;BYWEEKNO=20",
		"FREQ=DAILY;BYSECOND=61", "FREQ=WEEKLY;BYDAY=XX", "FREQ=MONTHLY;BYDAY=0MO", "FREQ=WEEKLY;BYDAY=1MO", "FREQ=MONTHLY;BYSETPOS=1",
		"FREQ=DAILY;UNTIL=tomorrow", "FREQ=DAILY;UNTIL=19900101T000000Z", "FREQ=DAILY;", "RRULE:FREQ=DAILY", "FREQ=SECONDLY",
		"FREQ=YEARLY;RSCALE=HEBREW", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30", "", "=", ";;;", "FREQ=DAILY;BYHOUR=9,,10",
		"FREQ=DAILY;INTERVAL=99999999999999999999", "\x00\xff", "FREQ=YEARLY;BYEASTER=0", strings.Repeat("FREQ=DAILY;", 80),
	}
	start := mustWall(t, "1997-09-02T09:00")
	for _, rule := range rules {
		_, err := Compile(Spec{Start: start, Zone: nyZone, Rule: rule})
		if rule == "" {
			if err != nil {
				t.Errorf("an empty rule means no rule: %v", err)
			}
			continue
		}
		code, _ := codeOf(t, err)
		if code == "" {
			t.Errorf("%q: empty code", rule)
		}
		for _, banned := range []string{"rrule-go", "<nil>", "NewRRule", "teambition"} {
			if strings.Contains(err.Error(), banned) {
				t.Errorf("%q: error text %q leaks %q", rule, err, banned)
			}
		}
	}
	for _, s := range []Spec{{Start: Wall{}, Zone: "UTC"}, {Start: start, Zone: "../etc/passwd"}, {Start: start, Floating: true, Zone: "UTC"}} {
		_, err := Compile(s)
		codeOf(t, err)
	}
}

func TestCompileCanonicalisesAndCopies(t *testing.T) {
	rd := []Wall{mustWall(t, "2025-04-01T09:00")}
	s := Spec{Start: mustWall(t, "2025-03-02T09:00"), Zone: nyZone, Rule: "freq=weekly;byday=mo", RDates: rd}
	c := compile(t, s)
	rd[0].Day = 9 // the caller's slice must not reach the compiled schedule
	got := c.Spec()
	if got.Rule != "FREQ=WEEKLY;BYDAY=MO" || got.RDates[0].Day != 1 {
		t.Errorf("Spec() = %+v; want a canonical rule and a private copy of RDATE", got)
	}
	got.RDates[0].Day = 7
	if c.Spec().RDates[0].Day != 1 {
		t.Error("Spec() shares its slices with the compiled schedule")
	}
}
