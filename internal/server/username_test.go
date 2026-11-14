package server

import (
	"fmt"
	"testing"
)

func TestUsernameAccountRoutesReserved(t *testing.T) {
	for _, name := range []string{"agenda", "needs", "tutorials"} {
		t.Run(name, func(t *testing.T) {
			want := fmt.Sprintf("username %q is reserved", name)
			if err := ValidateUsername(name); err == nil || err.Error() != want {
				t.Errorf("ValidateUsername(%q) = %v, want %q", name, err, want)
			}
		})
	}
}

func TestIsReservedUsernameCaseInsensitive(t *testing.T) {
	for _, name := range []string{"Tutorials", "NEEDS", "Agenda"} {
		if !IsReservedUsername(name) {
			t.Errorf("IsReservedUsername(%q) = false", name)
		}
	}
	if IsReservedUsername("mara") {
		t.Error("mara must not be reserved")
	}
}
