package store

import "time"

// Report buckets in the viewer's time zone (TASK-3524).
//
// A report without a Location buckets by UTC, exactly as before: SQL groups by
// the leading characters of the UTC RFC3339 timestamp. With a Location, a day
// bucket is the viewer's local calendar day and an hour bucket their local
// hour. SQLite has no time-zone database, so the grouping cannot happen in
// SQL; instead SQL groups by a FINER UTC unit and Go folds each group into
// the local bucket it falls in:
//
//   - by UTC hour when the zone's offset is a whole number of hours throughout
//     the window (almost every zone): at most 24 groups per day;
//   - by UTC MINUTE otherwise (+5:30, +5:45, Lord Howe's half-hour DST). A
//     GROUP BY yields a row only for a minute that has an event in it, so the
//     row count is bounded by the number of events in the window, not by the
//     minutes in it. Do not "optimise" this into a fixed offset: an offset is
//     wrong for every day on the other side of a DST change inside the window.
//
// Both are exact: every UTC hour (or minute) lies wholly inside one local hour
// and one local day in such a zone.

// reportFineGranularity picks the SQL grouping for a zoned report.
func reportFineGranularity(loc *time.Location, start, end time.Time) string {
	for t := start; ; t = t.Add(6 * time.Hour) {
		if t.After(end) {
			t = end
		}
		if _, off := t.In(loc).Zone(); off%3600 != 0 {
			return "minute"
		}
		if !t.Before(end) {
			return "hour"
		}
	}
}

const (
	reportHourLayout   = "2006-01-02T15"
	reportMinuteLayout = "2006-01-02T15:04"
	reportDayLayout    = "2006-01-02"
)

// foldReportBuckets re-keys UTC fine-grained counts into local buckets.
func foldReportBuckets(fine map[string]int, fineGran, gran string, loc *time.Location) map[string]int {
	in := reportHourLayout
	if fineGran == "minute" {
		in = reportMinuteLayout
	}
	out := reportDayLayout
	if gran == "hour" {
		out = reportHourLayout
	}
	folded := make(map[string]int, len(fine))
	for key, n := range fine {
		t, err := time.ParseInLocation(in, key, time.UTC)
		if err != nil {
			continue // not a timestamp prefix; cannot be placed
		}
		folded[t.In(loc).Format(out)] += n
	}
	return folded
}

// zeroFilledBucketsIn is zeroFilledBuckets in a local calendar: days step by
// calendar day (23 or 25 hours across a DST change), hours by an absolute hour
// labelled in local time. A DST fall-back repeats a local hour label; its
// counts were already folded into that one key, so the label is emitted once,
// never twice (which would count it twice in the totals).
func zeroFilledBucketsIn(start, end time.Time, gran string, loc *time.Location, created, completed map[string]int) []ReportBucket {
	ls := start.In(loc)
	out := []ReportBucket{}
	seen := map[string]bool{}
	emit := func(label string) {
		if seen[label] {
			return
		}
		seen[label] = true
		out = append(out, ReportBucket{Bucket: label, Created: created[label], Completed: completed[label]})
	}
	if gran == "hour" {
		for t := time.Date(ls.Year(), ls.Month(), ls.Day(), ls.Hour(), 0, 0, 0, loc); !t.After(end); t = t.Add(time.Hour) {
			emit(t.In(loc).Format(reportHourLayout))
		}
		return out
	}
	// Step a calendar-date counter, never the previous midnight: adding a day
	// to a midnight that sits in a gap lands on a date already visited (the
	// loop then never ends, TASK-4277) or drifts an hour off the day boundary.
	// The counter is pure calendar arithmetic in UTC, so it advances by exactly
	// one date per pass.
	y, m, day := ls.Date()
	for n := 0; ; n++ {
		civil := time.Date(y, m, day+n, 0, 0, 0, 0, time.UTC)
		d := time.Date(civil.Year(), civil.Month(), civil.Day(), 0, 0, 0, 0, loc)
		if d.After(end) {
			break
		}
		// A date the zone skipped (a jump across the International Date Line)
		// has no events and no bucket. Test noon, not midnight: a DST gap at
		// midnight (Sao Paulo) resolves midnight to the previous evening, but
		// the day itself exists and keeps its bucket.
		noon := time.Date(civil.Year(), civil.Month(), civil.Day(), 12, 0, 0, 0, loc)
		if ly, lm, ld := noon.Date(); ly != civil.Year() || lm != civil.Month() || ld != civil.Day() {
			continue
		}
		emit(civil.Format(reportDayLayout))
	}
	return out
}
