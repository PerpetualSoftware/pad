package server

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"testing"
)

func TestTASK4423_R05Notices(t *testing.T) {
	for _, kind := range []string{"deadline_approval_requested", "commitment_moved", "commitment_moved_past_deadline", "commitment_gate_opened"} {
		t.Run(kind, func(t *testing.T) {
			set := models.DefaultNotificationSettings()
			n := store.Notification{Kind: kind}
			if reason := kindEnabled(set, n); reason != "" {
				t.Fatalf("enabled: %s", reason)
			}
			set.NotifyAssigned = false
			if reason := kindEnabled(set, n); reason != skipSettingOff {
				t.Fatalf("disabled: %s", reason)
			}
			f := newPdx(t, testServer(t))
			id := f.row(kind)
			f.tick()
			if st := f.state(id); st.State != "sent" {
				t.Fatalf("dispatch: %+v", st)
			}
		})
	}
}
