package schedule

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// check parses and validates a rule the way Compile will: ParseRule, then
// Validate against the Start.
func check(t *testing.T, rule string, allDay, floating bool) (Rule, error) {
	t.Helper()
	start := mustWall(t, "1997-09-02T09:00")
	if allDay {
		start = mustWall(t, "1997-09-02")
	}
	r, err := ParseRule(rule)
	if err != nil {
		return r, err
	}
	var loc *time.Location
	if !floating {
		loc = mustLoc(t, "America/New_York")
	}
	return r, r.Validate(start, loc)
}

func TestMalformedRulesTable(t *testing.T) {
	const ok = Code("")
	rows := []struct {
		rule             string
		allDay, floating bool
		code             Code
		part             string
	}{
		{rule: "COUNT=3", code: CodeMissingPart, part: "FREQ"},
		{rule: "FREQ=FORTNIGHTLY", code: CodeRange, part: "FREQ"},
		{rule: "FREQ=daily", code: ok},
		{rule: "FREQ=DAILY;FOO=1", code: CodeUnknownPart, part: "FOO"},
		{rule: "FREQ=DAILY;COUNT=2;COUNT=3", code: CodeDuplicatePart, part: "COUNT"},
		{rule: "FREQ=DAILY;COUNT=3;UNTIL=19971224T000000Z", code: CodeCombination, part: "UNTIL"},
		{rule: "FREQ=DAILY;COUNT=0", code: CodeRange, part: "COUNT"},
		{rule: "FREQ=DAILY;COUNT=-1", code: CodeRange, part: "COUNT"},
		{rule: "FREQ=DAILY;INTERVAL=0", code: CodeRange, part: "INTERVAL"},
		{rule: "FREQ=DAILY;INTERVAL=-2", code: CodeRange, part: "INTERVAL"},
		{rule: "FREQ=DAILY;INTERVAL=x", code: CodeSyntax, part: "INTERVAL"},
		{rule: "FREQ=YEARLY;BYMONTH=13", code: CodeRange, part: "BYMONTH"},
		{rule: "FREQ=YEARLY;BYMONTH=0", code: CodeRange, part: "BYMONTH"},
		{rule: "FREQ=MONTHLY;BYMONTHDAY=0", code: CodeRange, part: "BYMONTHDAY"},
		{rule: "FREQ=MONTHLY;BYMONTHDAY=32", code: CodeRange, part: "BYMONTHDAY"},
		{rule: "FREQ=MONTHLY;BYMONTHDAY=-32", code: CodeRange, part: "BYMONTHDAY"},
		{rule: "FREQ=WEEKLY;BYMONTHDAY=1", code: CodeCombination, part: "BYMONTHDAY"},
		{rule: "FREQ=YEARLY;BYYEARDAY=0", code: CodeRange, part: "BYYEARDAY"},
		{rule: "FREQ=YEARLY;BYYEARDAY=367", code: CodeRange, part: "BYYEARDAY"},
		{rule: "FREQ=MONTHLY;BYYEARDAY=1", code: CodeCombination, part: "BYYEARDAY"},
		{rule: "FREQ=YEARLY;BYWEEKNO=0", code: CodeRange, part: "BYWEEKNO"},
		{rule: "FREQ=YEARLY;BYWEEKNO=54", code: CodeRange, part: "BYWEEKNO"},
		{rule: "FREQ=MONTHLY;BYWEEKNO=20", code: CodeCombination, part: "BYWEEKNO"},
		{rule: "FREQ=DAILY;BYHOUR=24", code: CodeRange, part: "BYHOUR"},
		{rule: "FREQ=DAILY;BYMINUTE=60", code: CodeRange, part: "BYMINUTE"},
		{rule: "FREQ=DAILY;BYSECOND=61", code: CodeRange, part: "BYSECOND"},
		{rule: "FREQ=DAILY;BYSECOND=60", code: CodeRange, part: "BYSECOND"},
		{rule: "FREQ=WEEKLY;BYDAY=XX", code: CodeSyntax, part: "BYDAY"},
		{rule: "FREQ=MONTHLY;BYDAY=0MO", code: CodeRange, part: "BYDAY"},
		{rule: "FREQ=YEARLY;BYDAY=54MO", code: CodeRange, part: "BYDAY"},
		{rule: "FREQ=MONTHLY;BYDAY=6MO", code: CodeRange, part: "BYDAY"},
		{rule: "FREQ=WEEKLY;BYDAY=1MO", code: CodeCombination, part: "BYDAY"},
		{rule: "FREQ=YEARLY;BYWEEKNO=20;BYDAY=1MO", code: CodeCombination, part: "BYDAY"},
		{rule: "FREQ=MONTHLY;BYDAY=MO;BYSETPOS=0", code: CodeRange, part: "BYSETPOS"},
		{rule: "FREQ=YEARLY;BYDAY=MO;BYSETPOS=367", code: CodeRange, part: "BYSETPOS"},
		{rule: "FREQ=MONTHLY;BYSETPOS=1", code: CodeCombination, part: "BYSETPOS"},
		{rule: "FREQ=WEEKLY;WKST=XX", code: CodeSyntax, part: "WKST"},
		{rule: "FREQ=DAILY;UNTIL=tomorrow", code: CodeSyntax, part: "UNTIL"},
		{rule: "FREQ=DAILY;UNTIL=19900101T000000Z", code: CodeRange, part: "UNTIL"},
		{rule: "FREQ=HOURLY;BYHOUR=9", code: ok},
		{rule: "FREQ=DAILY;BYHOUR=9,,10", code: CodeSyntax, part: "BYHOUR"},
		{rule: "FREQ=DAILY;", code: CodeSyntax},
		{rule: "RRULE:FREQ=DAILY", code: CodeSyntax},
		{rule: "FREQ=YEARLY;BYEASTER=0", code: CodeUnknownPart, part: "BYEASTER"},
		{rule: "FREQ=SECONDLY", code: CodeUnsupported, part: "FREQ"},
		{rule: "FREQ=YEARLY;RSCALE=HEBREW", code: CodeUnsupported, part: "RSCALE"},
		{rule: "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30", code: CodeUnsatisfiable, part: "BYMONTHDAY"},
		{rule: "FREQ=HOURLY", allDay: true, code: CodeCombination, part: "FREQ"},
		{rule: "FREQ=DAILY;BYHOUR=9", allDay: true, code: CodeCombination, part: "BYHOUR"},
		{rule: "FREQ=DAILY;UNTIL=19971224T000000Z", floating: true, code: CodeCombination, part: "UNTIL"},
	}
	for _, r := range rows {
		t.Run(r.rule, func(t *testing.T) {
			_, err := check(t, r.rule, r.allDay, r.floating)
			if r.code == ok {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			var me *MalformedError
			if !errors.Is(err, ErrMalformed) || !errors.As(err, &me) {
				t.Fatalf("err = %v (%T); want a *MalformedError", err, err)
			}
			if me.Code != r.code || me.Part != r.part {
				t.Errorf("got code=%s part=%q (%v); want code=%s part=%q", me.Code, me.Part, err, r.code, r.part)
			}
			if strings.Contains(err.Error(), "rrule-go") || strings.Contains(err.Error(), "<nil>") {
				t.Errorf("error text leaks a library: %v", err)
			}
		})
	}
}

func TestRuleCanonicalString(t *testing.T) {
	rows := []struct{ in, want string }{
		{"freq=daily", "FREQ=DAILY"},
		{"FREQ=WEEKLY;BYDAY=mo,tu;INTERVAL=2;WKST=su", "FREQ=WEEKLY;INTERVAL=2;WKST=SU;BYDAY=MO,TU"},
		{"BYSETPOS=-1;BYDAY=MO,TU,WE,TH,FR;FREQ=MONTHLY", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1"},
		{"FREQ=YEARLY;BYDAY=+1MO,-1FR;BYMONTH=1,6", "FREQ=YEARLY;BYMONTH=1,6;BYDAY=1MO,-1FR"},
		{"FREQ=DAILY;UNTIL=19971224", "FREQ=DAILY;UNTIL=19971224"},
		{"FREQ=DAILY;UNTIL=19971224t000000z", "FREQ=DAILY;UNTIL=19971224T000000Z"},
		{"FREQ=DAILY;COUNT=3;BYSECOND=0;BYMINUTE=30;BYHOUR=9", "FREQ=DAILY;COUNT=3;BYHOUR=9;BYMINUTE=30;BYSECOND=0"},
		{"FREQ=YEARLY;BYWEEKNO=20;BYYEARDAY=-1;BYMONTHDAY=-1", "FREQ=YEARLY;BYWEEKNO=20;BYYEARDAY=-1;BYMONTHDAY=-1"},
	}
	for _, r := range rows {
		got, err := ParseRule(r.in)
		if err != nil || got.String() != r.want {
			t.Errorf("ParseRule(%q).String() = %q, %v; want %q", r.in, got.String(), err, r.want)
		}
		again, err := ParseRule(got.String())
		if err != nil || again.String() != r.want {
			t.Errorf("canonical form %q does not round-trip: %q, %v", r.want, again.String(), err)
		}
	}
}

func TestRuleLengthLimit(t *testing.T) {
	long := "FREQ=DAILY;BYHOUR=" + strings.Repeat("9,", 250) + "9"
	if len(long) <= maxRuleBytes {
		t.Fatalf("fixture is %d bytes", len(long))
	}
	_, err := ParseRule(long)
	var me *MalformedError
	if !errors.As(err, &me) || me.Code != CodeRange || me.Part != "" {
		t.Fatalf("err = %v; want CodeRange on the whole rule", err)
	}
	if len(me.Value) > maxValueBytes {
		t.Errorf("Value is %d bytes; want at most %d", len(me.Value), maxValueBytes)
	}
	exact := "FREQ=DAILY;BYHOUR=9" + strings.Repeat(",9", (maxRuleBytes-len("FREQ=DAILY;BYHOUR=9"))/2)
	if len(exact) > maxRuleBytes || len(exact) < maxRuleBytes-1 {
		t.Fatalf("fixture is %d bytes", len(exact))
	}
	if _, err := ParseRule(exact); err != nil {
		t.Errorf("a %d-byte rule: %v", len(exact), err)
	}
}

func TestUnsatisfiableRules(t *testing.T) {
	bad := []string{
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=31",
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=-30",
		"FREQ=YEARLY;BYMONTH=4;BYMONTHDAY=31",
		"FREQ=YEARLY;BYMONTH=6;BYMONTHDAY=31",
		"FREQ=YEARLY;BYMONTH=9;BYMONTHDAY=31",
		"FREQ=YEARLY;BYMONTH=11;BYMONTHDAY=31",
		"FREQ=MONTHLY;BYMONTH=4,6,9,11;BYMONTHDAY=31",
		"FREQ=MINUTELY;BYMONTH=2;BYMONTHDAY=30",
	}
	for _, rule := range bad {
		begin := time.Now()
		_, err := ParseRule(rule)
		var me *MalformedError
		if !errors.As(err, &me) || me.Code != CodeUnsatisfiable {
			t.Errorf("%s: err = %v; want CodeUnsatisfiable", rule, err)
		}
		if d := time.Since(begin); d > 50*time.Millisecond {
			t.Errorf("%s: took %s", rule, d)
		}
	}
	good := []string{
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29", // a leap year has it
		"FREQ=YEARLY;BYMONTH=2,3;BYMONTHDAY=31",
		"FREQ=MONTHLY;BYMONTHDAY=31",
		"FREQ=MONTHLY;BYMONTHDAY=-1",
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30,28",
	}
	for _, rule := range good {
		if _, err := ParseRule(rule); err != nil {
			t.Errorf("%s: %v", rule, err)
		}
	}
}

// TestMalformedSpecFields covers the rule-level half: the UNTIL forms, their
// year bounds and their relation to Start. Zone, RDATE and the rest are
// Compile's and are tested with it.
func TestMalformedSpecFields(t *testing.T) {
	for _, in := range []string{"FREQ=DAILY;UNTIL=19691231", "FREQ=DAILY;UNTIL=22010101", "FREQ=DAILY;UNTIL=19971330"} {
		_, err := ParseRule(in)
		var me *MalformedError
		if !errors.As(err, &me) || me.Part != "UNTIL" {
			t.Errorf("%s: err = %v; want a refusal on UNTIL", in, err)
		}
	}
	for _, in := range []string{"FREQ=DAILY;UNTIL=19970902", "FREQ=DAILY;UNTIL=19970902T090000", "FREQ=DAILY;UNTIL=19970902T130000Z"} {
		if _, err := check(t, in, false, false); err != nil {
			t.Errorf("%s: UNTIL equal to Start must be accepted: %v", in, err)
		}
	}
	for _, in := range []string{"FREQ=DAILY;UNTIL=19970901", "FREQ=DAILY;UNTIL=19970902T085959", "FREQ=DAILY;UNTIL=19970902T125959Z"} {
		_, err := check(t, in, false, false)
		var me *MalformedError
		if !errors.As(err, &me) || me.Code != CodeRange || me.Part != "UNTIL" {
			t.Errorf("%s: err = %v; want CodeRange on UNTIL", in, err)
		}
	}
	if _, err := check(t, "FREQ=DAILY;UNTIL=19970902", true, false); err != nil {
		t.Errorf("all-day UNTIL on the Start date: %v", err)
	}
}
