package server

import (
	"strings"
	"testing"
	"time"
)

// The preview says when apply will lapse the outstanding occurrence instead of
// applying the change: open item, deadline already passed, behavior lapses.
func TestCommitmentRenegotiatePreviewLapsesOnApplyHTTP(t *testing.T) {
	t.Parallel()
	for _, behavior := range []string{"lapses", "stays_owed"} {
		t.Run(behavior, func(t *testing.T) {
			f := newSchedHTTP(t)
			create := commitmentBody()
			create["dtstart"] = time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format("2006-01-02T15:04:05")
			create["roles"].(map[string]any)["deadline_days"] = -2
			readCommitment(t, commitmentCall(f, "POST", create), 201)
			if _, err := f.srv.store.DB().Exec(`UPDATE item_schedules SET deadline_behavior=? WHERE item_id=?`, behavior, f.item.ID); err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"reason": "agreed", "scope": "series", "preview": true, "deadline_offset": map[string]any{"deadline_days": 2, "deadline_secs": 0}}
			rr := renegotiateCall(f, f.owner, body)
			if rr.Code != 200 {
				t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
			}
			want := `"lapses_on_apply":` + map[string]string{"lapses": "true", "stays_owed": "false"}[behavior]
			if !strings.Contains(rr.Body.String(), want) || !strings.Contains(rr.Body.String(), `"miss_recorded_first":true`) {
				t.Fatalf("preview body lacks %s: %s", want, rr.Body.String())
			}
			body["preview"], body["revision"] = false, schedMap(t, rr)["revision"]
			rr = renegotiateCall(f, f.owner, body)
			if behavior == "lapses" {
				f.expectErr(rr, 409, "commitment_changed", "")
				if n := f.count(`SELECT COUNT(*) FROM schedule_occurrences WHERE state='lapsed'`); n != 1 {
					t.Fatalf("lapsed rows = %d, want 1", n)
				}
			} else {
				readCommitment(t, rr, 200)
			}
		})
	}
}
