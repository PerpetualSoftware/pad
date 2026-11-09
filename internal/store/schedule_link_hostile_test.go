package store

import (
	"fmt"
	"testing"
	"time"
)

// syntheticLinkBundle is a bundle of n items, each with a native schedule that
// has target, earliest and occupied_until roles, built from one real export so
// every member is the shape the exporter writes. It returns the bundle and the
// source schedule ids in order. Links are for the caller to add.
func syntheticLinkBundle(t *testing.T, n int) (map[string]any, []string) {
	t.Helper()
	f := newLinkFix(t, 1)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', earliest_days=-1, earliest_secs=0, occupied_days=0, occupied_secs=3600 WHERE id=?`, f.scheds[0].ID)
	m := bundleObject(t, f.exportBundle(t))
	item := m["items"].([]any)[0].(map[string]any)
	sched := m["schedules"].([]any)[0].(map[string]any)
	var items, scheds []any
	var ids []string
	for i := 0; i < n; i++ {
		id, sid := newID(), newID()
		it := map[string]any{}
		for k, v := range item {
			it[k] = v
		}
		it["id"], it["title"], it["slug"], it["item_number"] = id, fmt.Sprintf("S%d", i), fmt.Sprintf("s%d", i), i+1
		sc := map[string]any{}
		for k, v := range sched {
			sc[k] = v
		}
		sc["id"], sc["item_id"] = sid, id
		items, scheds = append(items, it), append(scheds, sc)
		ids = append(ids, sid)
	}
	m["items"], m["schedules"] = items, scheds
	for _, member := range []string{"schedule_links", "schedule_changes", "comments", "item_versions", "reminders"} {
		delete(m, member)
	}
	return m, ids
}

// hubLinks lays hubs*kids+hubs-1 links out as ONE connected component that stays
// inside the 50-dependent and 16-hop limits: each hub's target feeds kids
// children and the next hub's earliest. ids holds the hubs, then their kids.
func hubLinks(ids []string, hubs, kids int, off map[string]any) []any {
	var links []any
	for k := 0; k < hubs; k++ {
		for i := 0; i < kids; i++ {
			links = append(links, linkObject(ids[k], ids[hubs+k*kids+i], "target", "target", off))
		}
		if k+1 < hubs {
			links = append(links, linkObject(ids[k], ids[k+1], "target", "earliest", off))
		}
	}
	return links
}

var civilHop = map[string]any{"mode": "civil", "days": 1, "zone": "America/New_York", "clock_secs": 3600}

func (f *schedFixture) importSynthetic(t *testing.T, m map[string]any) ImportReport {
	t.Helper()
	_, rep := f.importBundle(t, bundleDecode(t, m), "Synthetic "+t.Name())
	return rep
}

// The component cap a live write enforces (maxLinkComponent) holds on import:
// the layout keeps every footprint under 50 and 16, so the cap is the only rule
// that can refuse the links past the 500th.
func TestBundleLinks_ImportEnforcesTheComponentCap(t *testing.T) {
	const hubs, kids = 11, 48
	m, ids := syntheticLinkBundle(t, hubs*(kids+1))
	links := hubLinks(ids, hubs, kids, elapsedOffset(60))
	if len(links) != 538 {
		t.Fatalf("layout has %d links", len(links))
	}
	m["schedule_links"] = links
	f := newSchedFixture(t)
	rep := f.importSynthetic(t, m)
	if len(rep.DroppedSchedules) != 38 {
		t.Fatalf("%d drops, want the 38 links past the 500th: %+v", len(rep.DroppedSchedules), rep.DroppedSchedules[:1])
	}
	for _, d := range rep.DroppedSchedules {
		if d.Reason != "links:limit" {
			t.Fatalf("drop %+v, want links:limit", d)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != maxLinkComponent {
		t.Fatalf("%d links stored, want exactly the cap of %d", n, maxLinkComponent)
	}
}

// A bundle that makes the import check the same big component over and over
// must cost a bounded amount of work: links past maxImportedLinks are not
// examined, and say so once.
func TestBundleLinks_ImportBoundsTheWorkOfAHostileBundle(t *testing.T) {
	old := maxImportedLinks
	maxImportedLinks = 600
	t.Cleanup(func() { maxImportedLinks = old })
	const hubs, kids = 10, 48
	m, ids := syntheticLinkBundle(t, hubs*(kids+1))
	links := hubLinks(ids, hubs, kids, civilHop)
	accepted := len(links)
	for i := 0; i < 400; i++ {
		links = append(links, linkObject(ids[0], ids[hubs], "target", "target", civilHop))        // a second edge into one child
		links = append(links, linkObject(ids[hubs+i%kids], ids[0], "target", "target", civilHop)) // a cycle
	}
	m["schedule_links"] = links
	f := newSchedFixture(t)
	start := time.Now()
	rep := f.importSynthetic(t, m)
	elapsed := time.Since(start)
	want := map[string]int{"links:duplicate_incoming": 0, "links:cycle": 0, "links:bundle_limit": 1}
	got := map[string]int{}
	for _, d := range rep.DroppedSchedules {
		got[d.Reason]++
	}
	// 600 links are examined: the 489 that fit, then 111 rejects of the two kinds.
	if got["links:bundle_limit"] != 1 || got["links:duplicate_incoming"]+got["links:cycle"] != maxImportedLinks-accepted || len(got) != 3 {
		t.Fatalf("drops by reason %v (want one links:bundle_limit and %d rejects of the two kinds, %v)", got, maxImportedLinks-accepted, want)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != accepted {
		t.Fatalf("%d links stored, want the %d that fit", n, accepted)
	}
	t.Logf("examined %d of %d links in %s", maxImportedLinks, len(links), elapsed)
}

// The other ceiling is on work, not on count: links that each re-check a big
// component run out of budget long before maxImportedLinks, and a bundle of the
// same size made of small components does not.
func TestBundleLinks_ImportBoundsTheComponentWorkOfAHostileBundle(t *testing.T) {
	old := maxImportedLinkWork
	maxImportedLinkWork = 200_000
	t.Cleanup(func() { maxImportedLinkWork = old })
	const hubs, kids = 10, 48
	m, ids := syntheticLinkBundle(t, hubs*(kids+1))
	links := hubLinks(ids, hubs, kids, civilHop)
	accepted := len(links)
	for i := 0; i < 800; i++ {
		links = append(links, linkObject(ids[0], ids[hubs], "target", "target", civilHop))
	}
	m["schedule_links"] = links
	f := newSchedFixture(t)
	start := time.Now()
	rep := f.importSynthetic(t, m)
	got := map[string]int{}
	for _, d := range rep.DroppedSchedules {
		got[d.Reason]++
	}
	examined := got["links:duplicate_incoming"]
	if got["links:bundle_limit"] != 1 || examined == 0 || examined >= 800 || len(got) != 2 {
		t.Fatalf("drops by reason %v: want one links:bundle_limit after some, not all, of the 800 rejects", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != accepted {
		t.Fatalf("%d links stored, want the %d that fit", n, accepted)
	}
	t.Logf("%d of 800 rejects examined before the budget ran out, import took %s", examined, time.Since(start))

	// A bundle of many small components costs a few units a link and is not cut.
	maxImportedLinkWork = old
	n := 600
	small, sids := syntheticLinkBundle(t, 2*n)
	var pairs []any
	for i := 0; i < n; i++ {
		pairs = append(pairs, linkObject(sids[i], sids[n+i], "target", "target", civilHop))
	}
	small["schedule_links"] = pairs
	g := newSchedFixture(t)
	if rep := g.importSynthetic(t, small); len(rep.DroppedSchedules) != 0 || g.count(t, `SELECT COUNT(*) FROM schedule_links`) != n {
		t.Fatalf("%d small links: dropped %+v", n, rep.DroppedSchedules)
	}
}
