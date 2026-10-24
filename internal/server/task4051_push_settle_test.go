package server

import (
	"sort"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4051: a retry removes committed chunks and keeps only the failed
// chunk's ids parked. Kills: M3b, M3c.
// The tick caller at push_dispatch.go:824 is an equivalent mutant today (prefix always 0).
func TestPushDispatch_RetryUnrecordedClearsOnlyLandedIDs(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		const chunk = 500 // store.notificationIDChunk
		now := time.Now()
		ids := make([]string, chunk+3)
		meta := make(map[string]store.Notification, len(ids))
		for i := range ids {
			id := f.row("comment", func(o *rowOpt) { o.age = time.Minute })
			ids[i] = id
			meta[id] = store.Notification{UserID: f.bob.ID, CreatedAt: now.Add(-time.Minute).UTC().Format(time.RFC3339)}
		}
		// retryUnrecorded sorts before recording, so fault the second chunk
		// in that order rather than relying on the fixture's insertion order.
		sort.Strings(ids)
		bad := ids[chunk+1]
		if f.pg() {
			f.exec(`CREATE FUNCTION rs_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rs fault'; END $$`)
			f.exec(`CREATE TRIGGER rs_fail BEFORE UPDATE ON user_notifications FOR EACH ROW WHEN (NEW.push_state = 'sent' AND NEW.id = '` + bad + `') EXECUTE FUNCTION rs_fail()`)
		} else {
			f.exec(`CREATE TRIGGER rs_fail BEFORE UPDATE ON user_notifications WHEN NEW.push_state = 'sent' AND NEW.id = '` + bad + `' BEGIN SELECT RAISE(ABORT, 'rs fault'); END`)
		}
		srv.parkUnrecorded(ids, meta, now)
		srv.retryUnrecorded(time.Now())
		parked, _ := srv.unrecordedSnapshot()
		if len(parked) != len(ids[chunk:]) {
			t.Fatalf("%d ids still parked, want %d (only the failed second chunk)", len(parked), len(ids[chunk:]))
		}
		for i, id := range ids {
			if want := i >= chunk; parked[id] != want {
				t.Fatalf("id at sorted index %d: parked = %v, want %v", i, parked[id], want)
			}
		}
	})
}
