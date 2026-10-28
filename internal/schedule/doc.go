// Package schedule expands RFC 5545 recurrence rules into instants, with no
// store or HTTP (TASK-4100). It reads the clock only for the MaxDuration
// deadline in engine.go, so whether a call returns an answer or CapTime can
// depend on machine load. A caller supplies a Wall reading and a rule and
// gets UTC instants back.
//
// WALL TIMES. A schedule's Start, RDATEs, EXDATEs and UNTIL are Walls, civil
// readings with no zone ("2025-03-09T02:30:00"). The zone is separate: pinned
// to an IANA zone on the schedule, or floating, in which case the viewer's
// zone is supplied per call. Resolve turns a Wall into an instant, and is the
// only place a zone is applied.
//
// DST (RFC 5545 3.3.5). A wall time in a spring-forward gap does not exist: it
// takes the offset in force before the gap, so it lands after the gap (02:30
// becomes 03:30). A wall time in a fall-back overlap exists twice: it takes
// the first, DST-side instant. Two wall times that resolve to one instant are
// emitted once; COUNT counts wall-clock occurrences before that de-duplication.
//
// SUB-DAILY RULES (ruling Q2). HOURLY and MINUTELY with no BY part step in
// elapsed time, so INTERVAL means that many real hours or minutes across a
// transition. With any BY part they step on the wall clock and collapse: a
// repeated hour is emitted once, and gap readings merge into the post-gap
// instant. Everything else (DAILY, WEEKLY, MONTHLY, YEARLY) is wall clock, so
// a daily 02:30 never drifts. Populations, in America/New_York 2025 (gap
// 03-09 02:00 to 03:00, overlap 11-02 02:00 to 01:00):
//
//	FREQ                        gap                          overlap
//	DAILY/WEEKLY/MONTHLY/YEARLY 02:30 -> 03:30 EDT, no drift 01:30 once, as EDT
//	HOURLY/MINUTELY, no BY      fixed elapsed steps          every step fires, 01:00 EDT and 01:00 EST
//	HOURLY/MINUTELY, with BY    gap readings collapse        repeated hour once, first pass
//	BYHOUR=2,3 on DAILY         both -> 03:00 EDT, once      n/a
//	all-day                     date unaffected; At is local midnight, shifted forward if a gap
//	floating                    resolved in the view zone of each call
//	RDATE                       resolved forward             earlier
//	EXDATE                      matched on the wall reading
//	UNTIL                       compared on the wall; a Z UNTIL and elapsed rules: on the instant
//
// RULES. ParseRule is strict. It refuses what RFC 5545 forbids (COUNT with
// UNTIL, BYWEEKNO off YEARLY, an ordinal BYDAY off MONTHLY and YEARLY,
// BYSETPOS alone, BYMONTHDAY on WEEKLY, BYYEARDAY on DAILY, WEEKLY or MONTHLY),
// rules that can never fire (Feb 30), FREQ=SECONDLY (the tick is 30 s), and
// rules over 512 bytes, INTERVAL over 1,000,000 or years outside 1970..2200.
// A DTSTART the rule does not produce is not emitted (dateutil semantics, ruling
// Q1). Every refusal is a *MalformedError carrying a Code and the offending Part;
// no third-party error text ever reaches a caller.
//
// VENDORED EXPANDER. The library that expands a rule is vendored at
// internal/schedule/internal/rrule (teambition/rrule-go v1.8.2, MIT; see its
// PROVENANCE.md) because its Next() is unbounded on a no-match stretch: UNTIL
// is checked only against produced candidates, so a rule that never matches
// spins inside one call until year 9999 (TASK-4100 H1). The vendored copy
// carries a step budget (Limits.MaxSteps) that restarts at every candidate it
// produces, so it ends only a no-match stretch. The engine merges the rule, the
// RDATEs and the EXDATEs itself (the library's Set is a test oracle only): a
// rule that stalls is a stream state with a lower-bound position, the RDATEs
// below it still flow, and a CapStall is returned only when an answer could lie
// in the part of the rule not examined (TASK-4100 r3). CapScan is a
// different bound: the total work from Start to the query exceeds Limits.MaxScan
// periods. Do not "upgrade" the copy back to the module: that removes the bound.
package schedule
