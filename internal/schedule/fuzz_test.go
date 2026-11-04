package schedule

import (
	"errors"
	"testing"
	"time"
)

// fuzzRuleDeadline is how long one ParseRule call may take. The parser is
// linear in its input (512 bytes at most), so a call that takes this long is a
// bug, not a slow machine.
const fuzzRuleDeadline = 250 * time.Millisecond

// FuzzParseRule holds ParseRule to its contract on arbitrary text: it never
// panics, and every call returns either a rule or a *MalformedError, inside the
// deadline (TASK-4236). The seeds are the RFC 5545 section 3.8.5.3 examples
// plus the shapes that have bitten the expander: a rule that matches rarely, a
// narrow BYSETPOS, and the length and syntax edges.
//
//	go test ./internal/schedule/ -run '^$' -fuzz FuzzParseRule -fuzztime 30s
func FuzzParseRule(f *testing.F) {
	for _, s := range []string{
		// RFC 5545 section 3.8.5.3 examples, with the RRULE: prefix removed.
		"FREQ=DAILY;COUNT=10",
		"FREQ=DAILY;UNTIL=20001224T000000Z",
		"FREQ=DAILY;INTERVAL=2",
		"FREQ=DAILY;INTERVAL=10;COUNT=5",
		"FREQ=YEARLY;UNTIL=20000131T140000Z;BYMONTH=1;BYDAY=SU,MO,TU,WE,TH,FR,SA",
		"FREQ=DAILY;UNTIL=20000131T140000Z;BYMONTH=1",
		"FREQ=WEEKLY;COUNT=10",
		"FREQ=WEEKLY;UNTIL=19971224T000000Z",
		"FREQ=WEEKLY;INTERVAL=2;WKST=SU",
		"FREQ=WEEKLY;UNTIL=19971007T000000Z;WKST=SU;BYDAY=TU,TH",
		"FREQ=WEEKLY;COUNT=10;WKST=SU;BYDAY=TU,TH",
		"FREQ=WEEKLY;INTERVAL=2;UNTIL=19971224T000000Z;WKST=SU;BYDAY=MO,WE,FR",
		"FREQ=WEEKLY;INTERVAL=2;COUNT=8;WKST=SU;BYDAY=TU,TH",
		"FREQ=MONTHLY;COUNT=10;BYDAY=1FR",
		"FREQ=MONTHLY;UNTIL=19971224T000000Z;BYDAY=1FR",
		"FREQ=MONTHLY;INTERVAL=2;COUNT=10;BYDAY=1SU,-1SU",
		"FREQ=MONTHLY;COUNT=6;BYDAY=-2MO",
		"FREQ=MONTHLY;BYMONTHDAY=-3",
		"FREQ=MONTHLY;COUNT=10;BYMONTHDAY=2,15",
		"FREQ=MONTHLY;COUNT=10;BYMONTHDAY=1,-1",
		"FREQ=MONTHLY;INTERVAL=18;COUNT=10;BYMONTHDAY=10,11,12,13,14,15",
		"FREQ=MONTHLY;INTERVAL=2;BYDAY=TU",
		"FREQ=YEARLY;INTERVAL=2;COUNT=10;BYMONTH=1,2,3",
		"FREQ=YEARLY;INTERVAL=3;COUNT=10;BYYEARDAY=1,100,200",
		"FREQ=YEARLY;BYDAY=20MO",
		"FREQ=YEARLY;BYWEEKNO=20;BYDAY=MO",
		"FREQ=YEARLY;BYMONTH=3;BYDAY=TH",
		"FREQ=YEARLY;BYDAY=TH;BYMONTH=6,7,8",
		"FREQ=MONTHLY;BYDAY=FR;BYMONTHDAY=13",
		"FREQ=MONTHLY;BYDAY=SA;BYMONTHDAY=7,8,9,10,11,12,13",
		"FREQ=YEARLY;INTERVAL=4;BYMONTH=11;BYDAY=TU;BYMONTHDAY=2,3,4,5,6,7,8",
		"FREQ=MONTHLY;COUNT=3;BYDAY=TU,WE,TH;BYSETPOS=3",
		"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-2",
		"FREQ=HOURLY;INTERVAL=3;UNTIL=19970902T170000Z",
		"FREQ=MINUTELY;INTERVAL=15;COUNT=6",
		"FREQ=MINUTELY;INTERVAL=90;COUNT=4",
		"FREQ=DAILY;BYHOUR=9,10,11,12,13,14,15,16;BYMINUTE=0,20,40",
		"FREQ=MINUTELY;INTERVAL=20;BYHOUR=9,10,11,12,13,14,15,16",
		"FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=MO",
		"FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=SU",
		"FREQ=MONTHLY;BYMONTHDAY=15,30;COUNT=5",
		// Rules that match rarely or never, and narrow BYSETPOS.
		"FREQ=MINUTELY;BYMONTH=2;BYMONTHDAY=29",
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=DAILY;BYMONTH=2;BYMONTHDAY=29;BYDAY=MO",
		"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR,SA,SU;BYSETPOS=31",
		"FREQ=YEARLY;BYDAY=MO;BYSETPOS=-366",
		// Syntax and length edges.
		"",
		";",
		"FREQ",
		"FREQ=",
		"=DAILY",
		"FREQ=DAILY;",
		";FREQ=DAILY",
		"FREQ=DAILY;;COUNT=2",
		"RRULE:FREQ=DAILY",
		"freq=daily;count=2",
		"FREQ=DAILY;COUNT=99999999999999999999",
		"FREQ=DAILY;INTERVAL=-1",
		"FREQ=DAILY;BYHOUR=24",
		"FREQ=DAILY;UNTIL=2000",
		"FREQ=DAILY;COUNT=2;UNTIL=20000101T000000Z",
		"FREQ=DAILY;FREQ=DAILY",
		"FREQ=SECONDLY",
		"FREQ=DAILY;BYDAY=\xff\xfe",
		"FREQ=DAILY;X-FOO=bar",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		start := time.Now()
		r, err := ParseRule(s)
		if d := time.Since(start); d > fuzzRuleDeadline {
			t.Fatalf("ParseRule(%q) took %v, over the %v deadline", s, d, fuzzRuleDeadline)
		}
		if err != nil {
			var m *MalformedError
			if !errors.As(err, &m) || !errors.Is(err, ErrMalformed) {
				t.Fatalf("ParseRule(%q) = %T %v, want a *MalformedError", s, err, err)
			}
			return
		}
		// A rule that parsed must print and re-parse to itself.
		out := r.String()
		again, err := ParseRule(out)
		if err != nil {
			t.Fatalf("ParseRule(%q) accepted, but its String %q does not re-parse: %v", s, out, err)
		}
		if again.String() != out {
			t.Fatalf("ParseRule(%q).String() = %q, then %q on a second pass", s, out, again.String())
		}
	})
}
