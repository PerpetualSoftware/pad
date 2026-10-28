package schedule

const nyh = "DTSTART;TZID=America/New_York:"
const lonh = "DTSTART;TZID=Europe/London:"

// ExtraRows: our rows. Expected instants are exact UTC ("Z:") so the DST
// policy (gap shifts forward by the pre-gap offset; overlap takes the
// earlier instant) is asserted on the instant, not on local wall time.
var ExtraRows = []Row{
	// DAILY x {gap, overlap} x {NY, London}
	{"dst-daily-ny-gap", nyh + "20250308T023000\nRRULE:FREQ=DAILY;COUNT=3", "Z:20250308T0730Z Z:20250309T0730Z Z:20250310T0630Z", false},
	{"dst-daily-ny-overlap", nyh + "20251101T013000\nRRULE:FREQ=DAILY;COUNT=3", "Z:20251101T0530Z Z:20251102T0530Z Z:20251103T0630Z", false},
	{"dst-daily-london-gap", lonh + "20250329T013000\nRRULE:FREQ=DAILY;COUNT=3", "Z:20250329T0130Z Z:20250330T0130Z Z:20250331T0030Z", false},
	{"dst-daily-london-overlap", lonh + "20251025T013000\nRRULE:FREQ=DAILY;COUNT=3", "Z:20251025T0030Z Z:20251026T0030Z Z:20251027T0130Z", false},
	// WEEKLY / MONTHLY / YEARLY x {gap, overlap} (NY)
	{"dst-weekly-ny-gap", nyh + "20250302T023000\nRRULE:FREQ=WEEKLY;COUNT=3", "Z:20250302T0730Z Z:20250309T0730Z Z:20250316T0630Z", false},
	{"dst-weekly-ny-overlap", nyh + "20251026T013000\nRRULE:FREQ=WEEKLY;COUNT=3", "Z:20251026T0530Z Z:20251102T0530Z Z:20251109T0630Z", false},
	{"dst-monthly-ny-gap", nyh + "20250209T023000\nRRULE:FREQ=MONTHLY;COUNT=3", "Z:20250209T0730Z Z:20250309T0730Z Z:20250409T0630Z", false},
	{"dst-monthly-ny-overlap", nyh + "20251002T013000\nRRULE:FREQ=MONTHLY;COUNT=3", "Z:20251002T0530Z Z:20251102T0530Z Z:20251202T0630Z", false},
	{"dst-yearly-ny-gap", nyh + "20240310T023000\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU;COUNT=3", "Z:20240310T0730Z Z:20250309T0730Z Z:20260308T0730Z", false},
	{"dst-yearly-ny-overlap", nyh + "20241103T013000\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU;COUNT=3", "Z:20241103T0530Z Z:20251102T0530Z Z:20261101T0530Z", false},
	// HOURLY: elapsed-time policy (policy B). Wall-clock stepping would collapse
	// 02:00 into 03:00 across the gap and skip the repeated 01:00 hour.
	{"dst-hourly-ny-gap", nyh + "20250309T000000\nRRULE:FREQ=HOURLY;COUNT=5", "Z:20250309T0500Z Z:20250309T0600Z Z:20250309T0700Z Z:20250309T0800Z Z:20250309T0900Z", false},
	{"dst-hourly-ny-overlap", nyh + "20251102T000000\nRRULE:FREQ=HOURLY;COUNT=5", "Z:20251102T0400Z Z:20251102T0500Z Z:20251102T0600Z Z:20251102T0700Z Z:20251102T0800Z", false},
	{"dst-minutely60-ny-overlap", nyh + "20251102T003000\nRRULE:FREQ=MINUTELY;INTERVAL=60;COUNT=4", "Z:20251102T0430Z Z:20251102T0530Z Z:20251102T0630Z Z:20251102T0730Z", false},
	// BYHOUR expansion landing in the gap: two wall times collapse to one instant.
	{"dst-daily-byhour-gap-collision-dedupe", nyh + "20250309T000000\nRRULE:FREQ=DAILY;BYHOUR=2,3;COUNT=3", "Z:20250309T0700Z Z:20250310T0600Z", false},
	{"dst-rdate-in-gap", nyh + "20250309T090000\nRRULE:FREQ=DAILY;COUNT=1\nRDATE;TZID=America/New_York:20250309T023000", "Z:20250309T0730Z Z:20250309T1300Z", false},
	{"dst-exdate-in-gap", nyh + "20250308T023000\nRRULE:FREQ=DAILY;COUNT=3\nEXDATE;TZID=America/New_York:20250309T023000", "Z:20250308T0730Z Z:20250310T0630Z", false},
	{"dst-until-at-gap", nyh + "20250308T023000\nRRULE:FREQ=DAILY;UNTIL=20250309T073000Z", "Z:20250308T0730Z Z:20250309T0730Z", false},
	{"dst-hourly-byhour-gap-collapse", nyh + "20250309T000000\nRRULE:FREQ=HOURLY;BYHOUR=1,2,3;COUNT=3", "Z:20250309T0600Z Z:20250309T0700Z", false},
	{"dst-minutely-byhour-overlap-once", nyh + "20251102T003000\nRRULE:FREQ=MINUTELY;INTERVAL=30;BYHOUR=1;COUNT=4", "Z:20251102T0500Z Z:20251102T0530Z Z:20251103T0600Z Z:20251103T0630Z", false},
	// BYMONTHDAY 31 and -1
	{"bymonthday-31-skips-short-months", nyh + "20250131T090000\nRRULE:FREQ=MONTHLY;BYMONTHDAY=31;COUNT=5", "20250131 20250331 20250531 20250731 20250831", false},
	{"bymonthday-minus1-leap", nyh + "20240131T090000\nRRULE:FREQ=MONTHLY;BYMONTHDAY=-1;COUNT=5", "20240131 20240229 20240331 20240430 20240531", false},
	{"bymonthday-minus1-nonleap", nyh + "20250131T090000\nRRULE:FREQ=MONTHLY;BYMONTHDAY=-1;COUNT=3", "20250131 20250228 20250331", false},
	// RDATE / EXDATE interplay with COUNT: COUNT bounds the RULE; EXDATE removes
	// from the set without extending it; RDATE adds beyond it.
	{"rdate-exdate-count", nyh + "19970902T090000\nRRULE:FREQ=DAILY;COUNT=3\nEXDATE;TZID=America/New_York:19970903T090000\nRDATE;TZID=America/New_York:19970920T090000", "19970902 19970904 19970920", false},
	{"exdate-of-last-count-instance", nyh + "19970902T090000\nRRULE:FREQ=DAILY;COUNT=3\nEXDATE;TZID=America/New_York:19970904T090000", "19970902 19970903", false},
	{"rdate-duplicate-of-rule-instance-deduped", nyh + "19970902T090000\nRRULE:FREQ=DAILY;COUNT=2\nRDATE;TZID=America/New_York:19970903T090000", "19970902 19970903", false},
	{"rdate-before-dtstart-sorted", nyh + "19970902T090000\nRRULE:FREQ=DAILY;COUNT=2\nRDATE;TZID=America/New_York:19970801T090000", "19970801 19970902 19970903", false},
	{"rdate-only-no-rrule", nyh + "19970902T090000\nRDATE;TZID=America/New_York:19970905T090000", "19970902 19970905", false},
	{"oneshot-no-rrule", nyh + "19970902T090000", "19970902", false},
	// DTSTART not matching the rule (policy probe: dateutil semantic = not included)
	{"dtstart-not-matching-rule-not-included", nyh + "19970902T090000\nRRULE:FREQ=WEEKLY;BYDAY=FR;COUNT=2", "19970905 19970912", false},
	// UNTIL boundary is inclusive
	{"until-inclusive", nyh + "19970902T090000\nRRULE:FREQ=DAILY;UNTIL=19970904T130000Z", "19970902 19970903 19970904", false},
}
