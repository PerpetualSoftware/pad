package schedule

const maxPeriodCandidates = 10000

// periodDensity bounds candidates buffered by one generator FREQ period,
// before COUNT, UNTIL or BYSETPOS can reduce them. RFC 5545 section 3.3.10
// (https://www.rfc-editor.org/rfc/rfc5545#section-3.3.10) table rows:
//
//	         SECONDLY MINUTELY HOURLY DAILY  WEEKLY MONTHLY YEARLY
//	BYHOUR   Limit    Limit    Limit  Expand Expand Expand  Expand
//	BYMINUTE Limit    Limit    Expand Expand Expand Expand  Expand
//	BYSECOND Limit    Expand   Expand Expand Expand Expand  Expand
//
// All date-level parts, expanding or limiting, fit within the days of the
// period: internal/rrule/rrule.go:618-643 builds and filters dayset, and
// fillDaySetMonotonic at lines 873-888 includes each day at most once.
// Keep duplicate time items in the product: the generator may emit them.
// ParseRule's 512-byte limit bounds each list length by 256, so even the
// conservative 366 * 256^3 is below 2^33 and uint64 cannot overflow.
func periodDensity(r *Rule) uint64 {
	n := uint64(1)
	switch r.Freq {
	case freqYearly:
		n = 366
	case freqMonthly:
		n = 31
	case freqWeekly:
		n = 7
	}
	if r.Freq != freqHourly && r.Freq != freqMinutely {
		n *= uint64(max(1, len(r.ByHour)))
	}
	if r.Freq != freqMinutely {
		n *= uint64(max(1, len(r.ByMinute)))
	}
	return n * uint64(max(1, len(r.BySecond)))
}
