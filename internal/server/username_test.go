package server

import (
	"strings"
	"testing"
)

func TestUsernameAccountRoutesReserved(t *testing.T) {
	for _, name := range []string{"agenda", "needs", "tutorials"} {
		for _, variant := range []string{name, strings.ToUpper(name), strings.ToUpper(name[:1]) + name[1:]} {
			t.Run(variant, func(t *testing.T) {
				if err := ValidateUsername(variant); err == nil {
					t.Errorf("ValidateUsername(%q) accepted a reserved account route", variant)
				}
			})
		}
	}
}
