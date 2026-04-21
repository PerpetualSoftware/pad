package models

import (
	"testing"
	"time"
)

func TestWorkspaceInvitation_IsExpired(t *testing.T) {
	past := time.Now().UTC().Add(-1 * time.Hour)
	future := time.Now().UTC().Add(1 * time.Hour)

	tests := []struct {
		name      string
		expiresAt *time.Time
		want      bool
	}{
		{"nil expiry is not expired (legacy invitation)", nil, false},
		{"future expiry is not expired", &future, false},
		{"past expiry is expired", &past, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := &WorkspaceInvitation{ExpiresAt: tt.expiresAt}
			if got := inv.IsExpired(); got != tt.want {
				t.Fatalf("IsExpired() = %v, want %v", got, tt.want)
			}
		})
	}

	// Nil-safe: a nil receiver shouldn't panic.
	var nilInv *WorkspaceInvitation
	if nilInv.IsExpired() {
		t.Fatalf("nil invitation reported expired")
	}
}
