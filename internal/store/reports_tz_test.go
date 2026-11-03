package store

import (
	"testing"
	"time"
)

// TASK-3524: report buckets in the viewer's zone.

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("zone %s: %v", name, err)
	}
	return loc
}

func setCreatedAt(t *testing.T, s *Store, itemID, ts string) {
	t.Helper()
	if _, err := s.db.Exec(s.dialect.Rebind(`UPDATE items SET created_at = ? WHERE id = ?`), ts, itemID); err != nil {
		t.Fatalf("set created_at: %v", err)
	}
}

func createdIn(rep *ReportData, label string) int {
	b, _ := findBucket(rep.Buckets, label)
	return b.Created
}

func TestGetReport_TZ_LateEveningLandsOnItsLocalDay(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "late", "")
	// 03:30 UTC on 20 Jul is 20:30 on 19 Jul in Los Angeles (PDT).
	setCreatedAt(t, s, item.ID, "2026-07-20T03:30:00Z")
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	utc, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if utc.TZ != "" || createdIn(utc, "2026-07-20") != 1 {
		t.Fatalf("UTC report (no zone) should be unchanged: tz=%q, 07-20=%d", utc.TZ, createdIn(utc, "2026-07-20"))
	}

	la, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: now, Location: mustZone(t, "America/Los_Angeles")})
	if err != nil {
		t.Fatal(err)
	}
	if la.TZ != "America/Los_Angeles" {
		t.Fatalf("tz echo = %q", la.TZ)
	}
	if createdIn(la, "2026-07-19") != 1 || createdIn(la, "2026-07-20") != 0 {
		t.Fatalf("LA report: 07-19=%d 07-20=%d, want the item on 07-19", createdIn(la, "2026-07-19"), createdIn(la, "2026-07-20"))
	}
	if la.Totals.Created != 1 {
		t.Fatalf("LA totals.created = %d, want 1", la.Totals.Created)
	}
}

func TestGetReport_TZ_HalfHourZoneUsesMinutes(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	early := createTestItem(t, s, wsID, colID, "before midnight IST", "")
	late := createTestItem(t, s, wsID, colID, "after midnight IST", "")
	// IST is UTC+5:30, so 18:29 UTC is 23:59 on the 19th and 18:31 UTC is
	// 00:01 on the 20th: the SAME UTC hour, two local days.
	setCreatedAt(t, s, early.ID, "2026-07-19T18:29:00Z")
	setCreatedAt(t, s, late.ID, "2026-07-19T18:31:00Z")
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	ist := mustZone(t, "Asia/Kolkata")

	if g := reportFineGranularity(ist, now.Add(-7*24*time.Hour), now); g != "minute" {
		t.Fatalf("Asia/Kolkata fine granularity = %q, want minute", g)
	}
	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: now, Location: ist})
	if err != nil {
		t.Fatal(err)
	}
	if createdIn(rep, "2026-07-19") != 1 || createdIn(rep, "2026-07-20") != 1 {
		t.Fatalf("IST: 07-19=%d 07-20=%d, want one each", createdIn(rep, "2026-07-19"), createdIn(rep, "2026-07-20"))
	}
}

func TestGetReport_TZ_MonthAcrossDSTChange(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	before := createTestItem(t, s, wsID, colID, "EDT evening", "")
	after := createTestItem(t, s, wsID, colID, "EST evening", "")
	// New York falls back on 1 Nov 2026. 03:30 UTC is 23:30 the previous
	// local day under EDT (UTC-4); 04:30 UTC is 23:30 under EST (UTC-5). A
	// single fixed offset gets one of them wrong.
	setCreatedAt(t, s, before.ID, "2026-10-30T03:30:00Z")
	setCreatedAt(t, s, after.ID, "2026-11-03T04:30:00Z")
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ny := mustZone(t, "America/New_York")

	rep, err := s.GetReport(wsID, ReportOptions{Window: "month", Now: now, Location: ny})
	if err != nil {
		t.Fatal(err)
	}
	if createdIn(rep, "2026-10-29") != 1 || createdIn(rep, "2026-11-02") != 1 {
		t.Fatalf("NY: 10-29=%d 11-02=%d, want one each", createdIn(rep, "2026-10-29"), createdIn(rep, "2026-11-02"))
	}
	// One bucket per local day, in order, with no gaps or repeats across the
	// 25-hour day.
	for i := 1; i < len(rep.Buckets); i++ {
		prev, _ := time.Parse(reportDayLayout, rep.Buckets[i-1].Bucket)
		cur, _ := time.Parse(reportDayLayout, rep.Buckets[i].Bucket)
		if cur.Sub(prev) != 24*time.Hour {
			t.Fatalf("buckets %s → %s are not consecutive days", rep.Buckets[i-1].Bucket, rep.Buckets[i].Bucket)
		}
	}
}

// zeroFilledDayLabels runs zeroFilledBucketsIn for a day report and fails the
// test when it has not returned by the deadline: the failure mode under test is
// a loop that never ends, which a plain call would turn into a hung package.
func zeroFilledDayLabels(t *testing.T, zone string, start, end time.Time) []string {
	t.Helper()
	loc := mustZone(t, zone)
	done := make(chan []ReportBucket, 1)
	go func() { done <- zeroFilledBucketsIn(start, end, "day", loc, nil, nil) }()
	select {
	case buckets := <-done:
		labels := make([]string, len(buckets))
		for i, b := range buckets {
			labels[i] = b.Bucket
		}
		return labels
	case <-time.After(5 * time.Second):
		t.Fatalf("zeroFilledBucketsIn(%s, %s .. %s) did not return", zone, start.Format(time.RFC3339), end.Format(time.RFC3339))
		return nil
	}
}

func equalLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TASK-4277: a local date that does not exist (a zone moving across the
// International Date Line) must be skipped, not revisited forever.
func TestZeroFilledBucketsIn_SkippedLocalDateTerminates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		zone  string
		start time.Time
		end   time.Time
		want  []string
	}{
		{
			// Samoa skipped 2011-12-30 entirely.
			name:  "Pacific/Apia short window",
			zone:  "Pacific/Apia",
			start: time.Date(2011, 12, 28, 12, 0, 0, 0, time.UTC),
			end:   time.Date(2011, 12, 31, 12, 0, 0, 0, time.UTC),
			want:  []string{"2011-12-28", "2011-12-29", "2011-12-31", "2012-01-01"},
		},
		{
			name:  "Pacific/Apia long window",
			zone:  "Pacific/Apia",
			start: time.Date(2011, 12, 20, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2012, 1, 5, 0, 0, 0, 0, time.UTC),
			want: []string{
				"2011-12-19", "2011-12-20", "2011-12-21", "2011-12-22", "2011-12-23", "2011-12-24", "2011-12-25",
				"2011-12-26", "2011-12-27", "2011-12-28", "2011-12-29", "2011-12-31",
				"2012-01-01", "2012-01-02", "2012-01-03", "2012-01-04", "2012-01-05",
			},
		},
		{
			// Kiribati's Line Islands skipped 1994-12-31.
			name:  "Pacific/Kiritimati",
			zone:  "Pacific/Kiritimati",
			start: time.Date(1994, 12, 29, 12, 0, 0, 0, time.UTC),
			end:   time.Date(1995, 1, 2, 12, 0, 0, 0, time.UTC),
			want:  []string{"1994-12-29", "1994-12-30", "1995-01-01", "1995-01-02", "1995-01-03"},
		},
		{
			// Midnight does not exist on 2018-11-04 in Sao Paulo (the clock
			// jumped 00:00 to 01:00), but the DAY does: it must still get a
			// bucket, exactly once.
			name:  "America/Sao_Paulo midnight gap",
			zone:  "America/Sao_Paulo",
			start: time.Date(2018, 11, 3, 12, 0, 0, 0, time.UTC),
			end:   time.Date(2018, 11, 6, 12, 0, 0, 0, time.UTC),
			want:  []string{"2018-11-03", "2018-11-04", "2018-11-05", "2018-11-06"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := zeroFilledDayLabels(t, tc.zone, tc.start, tc.end)
			if !equalLabels(got, tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestZeroFilledBucketsIn_FallBackHourIsEmittedOnce(t *testing.T) {
	t.Parallel()
	ny := mustZone(t, "America/New_York")
	// 04:00–08:00 UTC on 1 Nov 2026 is local 00:00, 01:00 EDT, 01:00 EST,
	// 02:00, 03:00.
	start := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	fine := map[string]int{"2026-11-01T05": 1, "2026-11-01T06": 2}
	created := foldReportBuckets(fine, "hour", "hour", ny)
	if created["2026-11-01T01"] != 3 {
		t.Fatalf("both 01:00 hours fold into one key: got %v", created)
	}
	buckets := zeroFilledBucketsIn(start, end, "hour", ny, created, nil)
	total := 0
	seen := map[string]bool{}
	for _, b := range buckets {
		if seen[b.Bucket] {
			t.Fatalf("label %s emitted twice", b.Bucket)
		}
		seen[b.Bucket] = true
		total += b.Created
	}
	if total != 3 {
		t.Fatalf("total created %d, want 3 (the repeated hour must not count twice)", total)
	}
}
