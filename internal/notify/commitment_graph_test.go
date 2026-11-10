package notify

import "testing"

func TestTASK4423_R05Text(t *testing.T) {
	for kind, want := range map[string]string{"deadline_approval_requested": "A deadline change needs your approval", "commitment_moved": "A commitment date changed", "commitment_moved_past_deadline": "A commitment target is now past its deadline", "commitment_gate_opened": "A blocker succeeded for your item"} {
		if got, ok := Text(kind, 1); !ok || got != want {
			t.Errorf("%s: %q %v", kind, got, ok)
		}
	}
}
