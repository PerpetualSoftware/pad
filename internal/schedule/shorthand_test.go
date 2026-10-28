package schedule

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestShorthandTable(t *testing.T) {
	want := map[string]string{
		"daily": "FREQ=DAILY", "weekdays": "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", "weekly": "FREQ=WEEKLY",
		"biweekly": "FREQ=WEEKLY;INTERVAL=2", "monthly": "FREQ=MONTHLY", "monthly-last": "FREQ=MONTHLY;BYMONTHDAY=-1",
		"quarterly": "FREQ=MONTHLY;INTERVAL=3", "yearly": "FREQ=YEARLY",
	}
	for name, rule := range want {
		for _, in := range []string{name, " " + name + " ", strings.ToUpper(name)} {
			if got, ok := Shorthand(in); !ok || got != rule {
				t.Errorf("Shorthand(%q) = %q, %v; want %q", in, got, ok, rule)
			}
		}
	}
	if len(ShorthandNames()) != len(want) {
		t.Errorf("%d shorthands; want %d", len(ShorthandNames()), len(want))
	}
}

func TestShorthandUnknown(t *testing.T) {
	for _, in := range []string{"", "hourly", "FREQ=DAILY", "dailyy"} {
		if r, ok := Shorthand(in); ok || r != "" {
			t.Errorf("Shorthand(%q) = %q, %v; want none", in, r, ok)
		}
	}
}

func TestNormalizeRuleStripsPrefix(t *testing.T) {
	for _, in := range []string{"RRULE:FREQ=DAILY", "  rrule:freq=daily ", "FREQ=DAILY", "daily"} {
		if got, err := NormalizeRule(in); err != nil || got != "FREQ=DAILY" {
			t.Errorf("NormalizeRule(%q) = %q, %v; want FREQ=DAILY", in, got, err)
		}
	}
}

func TestNormalizeRuleCanonicalOrder(t *testing.T) {
	got, err := NormalizeRule("byday=mo,-1fr;bymonth=1;count=3;wkst=su;interval=2;freq=monthly")
	if want := "FREQ=MONTHLY;INTERVAL=2;COUNT=3;WKST=SU;BYMONTH=1;BYDAY=MO,-1FR"; err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
}

func TestNormalizeRuleMalformedTyped(t *testing.T) {
	for in, want := range map[string]Code{"": CodeSyntax, "RRULE:": CodeSyntax, "FREQ=DAILY;FOO=1": CodeUnknownPart, "COUNT=3": CodeMissingPart, "FREQ=SECONDLY": CodeUnsupported} {
		_, err := NormalizeRule(in)
		if code, _ := codeOf(t, err); code != want {
			t.Errorf("NormalizeRule(%q): code %s; want %s", in, code, want)
		}
	}
}

func TestMonthlyShorthandOn31stSkipsShortMonths(t *testing.T) {
	rule, _ := Shorthand("monthly")
	c := compile(t, pinned(t, "UTC", "2025-01-31T09:00", rule+";COUNT=4"))
	occ, _ := c.Between(inst(t, "2025-01-01T00:00:00Z"), inst(t, "2025-12-31T00:00:00Z"), nil, Limits{MaxWindow: 400 * 24 * time.Hour})
	wantAts(t, occ, "2025-01-31T09:00:00Z", "2025-03-31T09:00:00Z", "2025-05-31T09:00:00Z", "2025-07-31T09:00:00Z")
}

func TestMonthlyLastShorthand(t *testing.T) {
	rule, _ := Shorthand("monthly-last")
	c := compile(t, pinned(t, "UTC", "2024-01-31T09:00", rule+";COUNT=3"))
	occ, _ := c.Between(inst(t, "2024-01-01T00:00:00Z"), inst(t, "2024-04-01T00:00:00Z"), nil, Limits{})
	wantAts(t, occ, "2024-01-31T09:00:00Z", "2024-02-29T09:00:00Z", "2024-03-31T09:00:00Z")
}

func TestQuarterlyShorthand(t *testing.T) {
	rule, _ := Shorthand("quarterly")
	c := compile(t, pinned(t, "UTC", "2025-01-15T09:00", rule+";COUNT=3"))
	occ, _ := c.Between(inst(t, "2025-01-01T00:00:00Z"), inst(t, "2025-12-31T00:00:00Z"), nil, Limits{MaxWindow: 400 * 24 * time.Hour})
	wantAts(t, occ, "2025-01-15T09:00:00Z", "2025-04-15T09:00:00Z", "2025-07-15T09:00:00Z")
}

func TestShorthandNamesSorted(t *testing.T) {
	n := ShorthandNames()
	if !sort.StringsAreSorted(n) || !reflect.DeepEqual(n[:2], []string{"biweekly", "daily"}) {
		t.Errorf("names = %v; want sorted", n)
	}
}
