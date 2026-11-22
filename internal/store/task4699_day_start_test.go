package store

import (
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4699: users.day_start_minute, the minute of the day a user's day
// starts at. NULL means never set; the CHECK keeps it inside 0..1439.

func TestTask4699_DayStartStore(t *testing.T) {
	s := testStore(t)
	t.Logf("dialect: %s", s.dialect.Driver())
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" && s.dialect.Driver() != DriverPostgres {
		t.Fatalf("PAD_TEST_POSTGRES_URL is set but the store is %s", s.dialect.Driver())
	}
	u := createTestUser(t, s, "daystart@example.com", "Day Start", "")
	other := createTestUser(t, s, "other4699@example.com", "Other", "")

	got, err := s.GetUser(u.ID)
	if err != nil || got == nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.DayStartMinute != nil {
		t.Fatalf("fresh user DayStartMinute = %d, want nil", *got.DayStartMinute)
	}

	v := 450
	updated, err := s.UpdateUser(u.ID, models.UserUpdate{DayStartMinute: &v})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if updated.DayStartMinute == nil || *updated.DayStartMinute != 450 {
		t.Fatalf("UpdateUser answer DayStartMinute = %v, want 450", updated.DayStartMinute)
	}
	got, _ = s.GetUser(u.ID)
	if got.DayStartMinute == nil || *got.DayStartMinute != 450 {
		t.Fatalf("GetUser DayStartMinute = %v, want 450", got.DayStartMinute)
	}

	// An update that does not name it leaves it alone.
	name := "Renamed"
	if _, err := s.UpdateUser(u.ID, models.UserUpdate{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetUser(u.ID); got.DayStartMinute == nil || *got.DayStartMinute != 450 {
		t.Fatalf("an unrelated update changed DayStartMinute to %v", got.DayStartMinute)
	}
	if o, _ := s.GetUser(other.ID); o.DayStartMinute != nil {
		t.Fatalf("another user got DayStartMinute %d", *o.DayStartMinute)
	}

	// The admin search scan site reads it too.
	res, err := s.SearchUsers(AdminUserSearchParams{Query: "daystart@example.com", Limit: 10})
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if len(res.Users) != 1 || res.Users[0].DayStartMinute == nil || *res.Users[0].DayStartMinute != 450 {
		t.Fatalf("SearchUsers DayStartMinute: %+v", res.Users)
	}

	// The CHECK refuses an out-of-range value written past the store.
	for _, bad := range []int{1440, -1} {
		if _, err := s.db.Exec(s.q(`UPDATE users SET day_start_minute = ? WHERE id = ?`), bad, u.ID); err == nil {
			t.Errorf("raw UPDATE day_start_minute = %d was accepted", bad)
		}
	}
	if got, _ = s.GetUser(u.ID); got.DayStartMinute == nil || *got.DayStartMinute != 450 {
		t.Fatalf("a refused raw write changed DayStartMinute to %v", got.DayStartMinute)
	}
	if _, err := s.db.Exec(s.q(`UPDATE users SET day_start_minute = ? WHERE id = ?`), 1439, u.ID); err != nil {
		t.Errorf("raw UPDATE day_start_minute = 1439 refused: %v", err)
	}
}
