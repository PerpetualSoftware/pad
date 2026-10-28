package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4101: users.timezone round-trips, defaults to NULL, and the
// first-sign-in default is one conditional UPDATE. Runs on SQLite in make test
// and on Postgres in make test-pg (testStore picks the dialect).
func TestUserTimezone(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "tz4101@example.com", "TZ", "password123")
	if u.Timezone != nil {
		t.Fatalf("new user timezone = %q, want nil", *u.Timezone)
	}

	// null + if-unset -> set.
	got, err := s.SetUserTimezoneIfUnset(u.ID, "Europe/London")
	if err != nil || got.Timezone == nil || *got.Timezone != "Europe/London" {
		t.Fatalf("if-unset on NULL: %v %+v", err, got)
	}
	// An explicit write replaces it.
	tokyo := "Asia/Tokyo"
	got, err = s.UpdateUser(u.ID, modelsUserTimezone(tokyo))
	if err != nil || got.Timezone == nil || *got.Timezone != tokyo {
		t.Fatalf("explicit write: %v %+v", err, got)
	}
	// If-unset over a set zone changes nothing and returns the winner.
	got, err = s.SetUserTimezoneIfUnset(u.ID, "America/Chicago")
	if err != nil || got.Timezone == nil || *got.Timezone != tokyo {
		t.Fatalf("if-unset over a set zone: %v %+v", err, got)
	}
	// A password-only update keeps the zone.
	pw := "another-password-123"
	got, err = s.UpdateUser(u.ID, modelsUserPassword(pw))
	if err != nil || got.Timezone == nil || *got.Timezone != tokyo {
		t.Fatalf("password update dropped the zone: %v %+v", err, got)
	}
	if _, err := s.SetUserTimezoneIfUnset("no-such-user", "UTC"); err == nil {
		t.Error("an unknown user must not succeed")
	}
	// The admin list scan path still lines up with the column list.
	if _, err := s.ListUsers(); err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if _, err := s.SearchUsers(AdminUserSearchParams{}); err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
}

func modelsUserTimezone(tz string) models.UserUpdate { return models.UserUpdate{Timezone: &tz} }
func modelsUserPassword(pw string) models.UserUpdate { return models.UserUpdate{Password: &pw} }
