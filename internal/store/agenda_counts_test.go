package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const countsSchema = `{"fields":[{"key":"status","type":"select","options":["open","done","cancelled"],"default":"open","terminal_options":["done","cancelled"],"abandoned_options":["cancelled"]}]}`

// countsItem makes an item created at createdAt whose history is the given
// (instant, status) steps. Its own create row is replaced, so the history is
// exactly what the test wrote.
func countsItem(t *testing.T, f *schedFixture, collID, title, createdAt string, steps ...[2]string) *models.Item {
	t.Helper()
	it := createTestItem(t, f.s, f.ws.ID, collID, title, "")
	f.exec(t, `UPDATE items SET created_at = ? WHERE id = ?`, createdAt, it.ID)
	f.exec(t, `DELETE FROM status_transitions WHERE item_id = ?`, it.ID)
	prev := ""
	all := append([][2]string{{createdAt, "open"}}, steps...)
	for n, st := range all {
		f.exec(t, `INSERT INTO status_transitions (id,item_id,workspace_id,collection_id,field_key,from_status,to_status,created_at,seq) VALUES (?,?,?,?,'status',?,?,?,?)`,
			fmt.Sprintf("%s-%d", it.ID, n), it.ID, f.ws.ID, collID, prev, st[1], st[0], n)
		prev = st[1]
	}
	return it
}

func countsFixture(t *testing.T) *schedFixture {
	f := newSchedFixture(t)
	f.exec(t, `UPDATE collections SET schema = ? WHERE id = ?`, countsSchema, f.item.CollectionID)
	// The fixture's own item would be one more open item in every series.
	f.exec(t, `UPDATE items SET deleted_at = ? WHERE id = ?`, "2030-01-01T00:00:00Z", f.item.ID)
	return f
}

func countsGot(days []AgendaDayCount) string {
	out := ""
	for _, d := range days {
		out += fmt.Sprintf("%s:%d/%d/%d ", d.Day, d.Completed, d.Open, d.Burndown)
	}
	return out
}

// completed / open at the end of the day / burn-down of the items open at the
// window's start. New items never add to the burn-down line.
func TestAgendaCountsSeries(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-13T00:00:00Z")
	countsItem(t, f, c, "stays open", "2031-03-01T00:00:00Z")
	countsItem(t, f, c, "closes day one", "2031-03-05T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	countsItem(t, f, c, "done before", "2031-03-01T00:00:00Z", [2]string{"2031-03-02T00:00:00Z", "done"})
	countsItem(t, f, c, "abandoned day two", "2031-03-05T00:00:00Z", [2]string{"2031-03-11T10:00:00Z", "cancelled"})
	countsItem(t, f, c, "new and done", "2031-03-11T08:00:00Z", [2]string{"2031-03-11T20:00:00Z", "done"})
	countsItem(t, f, c, "new and open", "2031-03-12T01:00:00Z")
	countsItem(t, f, c, "reopened", "2031-03-05T00:00:00Z", [2]string{"2031-03-09T00:00:00Z", "done"}, [2]string{"2031-03-12T05:00:00Z", "open"})
	days, _, truncated, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 1000)
	if err != nil || truncated {
		t.Fatalf("counts: %v truncated=%v", err, truncated)
	}
	// Open at the start: stays open, closes day one, abandoned day two = 3.
	// Day 10: closes day one done. Day 11: abandoned (not completed) and
	// new and done. Day 12: reopened, which is not a completion and does not
	// join the burn-down (it was closed at the start).
	want := "2031-03-10:1/2/2 2031-03-11:1/1/1 2031-03-12:0/3/1 "
	if got := countsGot(days); got != want {
		t.Fatalf("series = %s, want %s", got, want)
	}
}

func TestAgendaCountsScopeAndDeleted(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	hidden := createTestCollection(t, f.s, f.ws.ID, "Hidden")
	f.exec(t, `UPDATE collections SET schema = ? WHERE id = ?`, countsSchema, hidden.ID)
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	countsItem(t, f, c, "visible", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	countsItem(t, f, hidden.ID, "hidden", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	gone := countsItem(t, f, c, "deleted", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	f.exec(t, `UPDATE items SET deleted_at = ? WHERE id = ?`, "2031-03-10T11:00:00Z", gone.ID)
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{CollectionIDs: []string{c}}, from, to, time.UTC, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := countsGot(days), "2031-03-10:1/0/0 "; got != want {
		t.Fatalf("scoped = %s, want %s", got, want)
	}
	none, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{CollectionIDs: []string{}}, from, to, time.UTC, 1000)
	if err != nil || countsGot(none) != "2031-03-10:0/0/0 " {
		t.Fatalf("empty scope = %s %v", countsGot(none), err)
	}
}

// 23:30 local lands on that local day, and a window that starts mid-day still
// buckets by the zone's days.
func TestAgendaCountsLocalDays(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	from, to := rt("2031-03-10T07:00:00Z"), rt("2031-03-13T07:00:00Z")
	countsItem(t, f, c, "late local", "2031-03-01T00:00:00Z", [2]string{"2031-03-11T06:30:00Z", "done"})
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, la, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := countsGot(days), "2031-03-10:1/0/0 2031-03-11:0/0/0 2031-03-12:0/0/0 "; got != want {
		t.Fatalf("series = %s, want %s", got, want)
	}
}

// A zone that skips a whole local date must still end the day loop.
func TestAgendaCountsSkippedLocalDate(t *testing.T) {
	f := countsFixture(t)
	apia, err := time.LoadLocation("Pacific/Apia") // skipped 2011-12-30
	if err != nil {
		t.Skip("no tzdata")
	}
	from, to := rt("2011-12-29T00:00:00Z"), rt("2012-01-02T00:00:00Z")
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, apia, 1000)
	if err != nil || len(days) == 0 || len(days) > 6 {
		t.Fatalf("days = %s %v", countsGot(days), err)
	}
}

func TestAgendaCountsCap(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	for i := 0; i < 3; i++ {
		countsItem(t, f, c, fmt.Sprintf("capped %d", i), "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	}
	days, _, truncated, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 2)
	if err != nil || !truncated || days != nil {
		t.Fatalf("cap: days=%v truncated=%v err=%v", days, truncated, err)
	}
}
