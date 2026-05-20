package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func createTestUser(t *testing.T, s *Store, email, name, password string) *models.User {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email:    email,
		Name:     name,
		Password: password,
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	return u
}

func TestUserCRUD(t *testing.T) {
	s := testStore(t)

	// Create
	u, err := s.CreateUser(models.UserCreate{
		Email:    "Dave@Example.com",
		Name:     "Dave",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("CreateUser error: %v", err)
	}
	if u.Name != "Dave" {
		t.Errorf("expected name 'Dave', got %q", u.Name)
	}
	if u.Email != "dave@example.com" {
		t.Errorf("expected lowercased email, got %q", u.Email)
	}
	if u.Role != "member" {
		t.Errorf("expected default role 'member', got %q", u.Role)
	}
	if u.PasswordHash == "" {
		t.Error("password hash should not be empty")
	}
	if u.PasswordHash == "password123" {
		t.Error("password should be hashed, not stored plaintext")
	}

	// Get by ID
	got, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatalf("GetUser error: %v", err)
	}
	if got == nil || got.ID != u.ID {
		t.Error("GetUser returned wrong user")
	}

	// Get by email (case-insensitive)
	got, err = s.GetUserByEmail("DAVE@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail error: %v", err)
	}
	if got == nil || got.ID != u.ID {
		t.Error("GetUserByEmail returned wrong user")
	}

	// Get nonexistent
	got, err = s.GetUser("nonexistent-id")
	if err != nil {
		t.Fatalf("GetUser nonexistent error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent user")
	}

	// Update name
	newName := "David"
	updated, err := s.UpdateUser(u.ID, models.UserUpdate{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateUser error: %v", err)
	}
	if updated.Name != "David" {
		t.Errorf("expected updated name 'David', got %q", updated.Name)
	}

	// Update password
	newPass := "newpassword456"
	_, err = s.UpdateUser(u.ID, models.UserUpdate{Password: &newPass})
	if err != nil {
		t.Fatalf("UpdateUser password error: %v", err)
	}
	// Old password should no longer work
	result, _ := s.ValidatePassword("dave@example.com", "password123")
	if result != nil {
		t.Error("old password should not validate after change")
	}
	// New password should work
	result, _ = s.ValidatePassword("dave@example.com", "newpassword456")
	if result == nil {
		t.Error("new password should validate after change")
	}
}

func TestUserCreateAdmin(t *testing.T) {
	s := testStore(t)

	u, err := s.CreateUser(models.UserCreate{
		Email:    "admin@test.com",
		Name:     "Admin",
		Password: "admin123",
		Role:     "admin",
	})
	if err != nil {
		t.Fatalf("CreateUser error: %v", err)
	}
	if u.Role != "admin" {
		t.Errorf("expected role 'admin', got %q", u.Role)
	}
}

func TestUserDuplicateEmail(t *testing.T) {
	s := testStore(t)

	_, err := s.CreateUser(models.UserCreate{
		Email: "dave@test.com", Name: "Dave", Password: "pass123",
	})
	if err != nil {
		t.Fatalf("first CreateUser error: %v", err)
	}

	_, err = s.CreateUser(models.UserCreate{
		Email: "dave@test.com", Name: "Other Dave", Password: "pass456",
	})
	if err == nil {
		t.Error("expected error for duplicate email")
	}
}

func TestValidatePassword(t *testing.T) {
	s := testStore(t)

	createTestUser(t, s, "test@test.com", "Test", "correctpassword")

	// Correct password
	u, err := s.ValidatePassword("test@test.com", "correctpassword")
	if err != nil {
		t.Fatalf("ValidatePassword error: %v", err)
	}
	if u == nil {
		t.Error("expected user for correct password")
	}

	// Wrong password
	u, err = s.ValidatePassword("test@test.com", "wrongpassword")
	if err != nil {
		t.Fatalf("ValidatePassword wrong password error: %v", err)
	}
	if u != nil {
		t.Error("expected nil for wrong password")
	}

	// Nonexistent email
	u, err = s.ValidatePassword("nobody@test.com", "anything")
	if err != nil {
		t.Fatalf("ValidatePassword nonexistent error: %v", err)
	}
	if u != nil {
		t.Error("expected nil for nonexistent email")
	}
}

func TestListUsers(t *testing.T) {
	s := testStore(t)

	createTestUser(t, s, "a@test.com", "Alice", "pass1")
	createTestUser(t, s, "b@test.com", "Bob", "pass2")

	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("expected 2 users, got %d", len(users))
	}
}

func TestUserCount(t *testing.T) {
	s := testStore(t)

	count, _ := s.UserCount()
	if count != 0 {
		t.Errorf("expected 0 users, got %d", count)
	}

	createTestUser(t, s, "a@test.com", "Alice", "pass1")
	createTestUser(t, s, "b@test.com", "Bob", "pass2")

	count, _ = s.UserCount()
	if count != 2 {
		t.Errorf("expected 2 users, got %d", count)
	}
}

func TestCountBillingAggregates(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)

	// Empty store: zero counts, no plans, no signups.
	agg, err := s.CountBillingAggregates(cutoff)
	if err != nil {
		t.Fatalf("CountBillingAggregates on empty store: %v", err)
	}
	if len(agg.CustomersByPlan) != 0 {
		t.Errorf("empty store: want no plan entries, got %v", agg.CustomersByPlan)
	}
	if agg.NewProSignups != 0 {
		t.Errorf("empty store: want NewProSignups=0, got %d", agg.NewProSignups)
	}

	// Mix: explicit-empty plan (older legacy rows) + explicit "free"
	// (current default) + pro recent + pro old + self-hosted. Both the
	// '' and 'free' rows must roll up into the same "free" bucket — Codex
	// round 2 caught this regression where GROUP BY plan (raw column)
	// produced two scanned rows both labelled "free" that overwrote each
	// other in the result map.
	insertWithPlanAndDate(t, s, "legacy_blank@test.com", "", now.Add(-100*24*time.Hour))
	insertWithPlanAndDate(t, s, "free_explicit@test.com", "free", now.Add(-2*24*time.Hour))
	insertWithPlanAndDate(t, s, "free_explicit2@test.com", "free", now.Add(-1*24*time.Hour))
	insertWithPlanAndDate(t, s, "pro_recent@test.com", "pro", now.Add(-5*24*time.Hour))
	insertWithPlanAndDate(t, s, "pro_old@test.com", "pro", now.Add(-90*24*time.Hour))
	insertWithPlanAndDate(t, s, "self@test.com", "self-hosted", now.Add(-60*24*time.Hour))

	agg, err = s.CountBillingAggregates(cutoff)
	if err != nil {
		t.Fatalf("CountBillingAggregates: %v", err)
	}
	// 1 legacy blank + 2 explicit "free" must roll up to 3.
	if got, want := agg.CustomersByPlan["free"], 3; got != want {
		t.Errorf("free (rolls up '' + 'free'): want %d, got %d (full map: %v)", want, got, agg.CustomersByPlan)
	}
	if got, want := agg.CustomersByPlan["pro"], 2; got != want {
		t.Errorf("pro: want %d, got %d", want, got)
	}
	if got, want := agg.CustomersByPlan["self-hosted"], 1; got != want {
		t.Errorf("self-hosted: want %d, got %d", want, got)
	}
	if got, want := agg.NewProSignups, 1; got != want {
		t.Errorf("new pro signups in last 30d: want %d (recent only), got %d", want, got)
	}
}

// TestComputeAdminUserStatus locks in the precedence order: disabled wins
// over no-workspace wins over inactive wins over active. Documented in the
// admin user list contract (PLAN-1542 / TASK-1544).
func TestComputeAdminUserStatus(t *testing.T) {
	recent := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	old := time.Now().UTC().Add(-60 * 24 * time.Hour).Format(time.RFC3339)
	cases := []struct {
		name           string
		disabledAt     string
		lastWriteAt    string
		workspaceCount int
		want           string
	}{
		{"disabled overrides everything", "2026-01-01T00:00:00Z", recent, 5, "disabled"},
		{"disabled with no workspace still disabled", "2026-01-01T00:00:00Z", "", 0, "disabled"},
		{"no workspace beats inactive", "", "", 0, "no-workspace"},
		{"never written, has workspace", "", "", 3, "inactive"},
		{"wrote long ago", "", old, 1, "inactive"},
		{"wrote recently", "", recent, 1, "active"},
		{"malformed timestamp treated as inactive", "", "not-a-date", 1, "inactive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeAdminUserStatus(c.disabledAt, c.lastWriteAt, c.workspaceCount)
			if got != c.want {
				t.Fatalf("want %q got %q", c.want, got)
			}
		})
	}
}

// TestSearchUsersAggregations verifies that SearchUsers returns correct
// workspace_count + storage_bytes + status aggregations and that the new
// sort/filter knobs route to the right rows. Single end-to-end test against
// a multi-user fixture; finer-grained assertions are split into subtests.
func TestSearchUsersAggregations(t *testing.T) {
	s := testStore(t)

	// Three users:
	//   alice — owns 2 workspaces; one has a 100-byte attachment, one empty
	//   bob   — owns 1 workspace, no attachments
	//   carol — owns 0 workspaces, disabled
	alice := createTestUser(t, s, "alice@example.com", "Alice", "password123")
	bob := createTestUser(t, s, "bob@example.com", "Bob", "password123")
	carol := createTestUser(t, s, "carol@example.com", "Carol", "password123")
	if err := s.DisableUser(carol.ID); err != nil {
		t.Fatalf("disable carol: %v", err)
	}

	mkWS := func(owner string, slug string) string {
		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: slug, Slug: slug, OwnerID: owner})
		if err != nil {
			t.Fatalf("create ws %s: %v", slug, err)
		}
		return ws.ID
	}
	aliceWS1 := mkWS(alice.ID, "alice-ws-1")
	mkWS(alice.ID, "alice-ws-2")
	mkWS(bob.ID, "bob-ws-1")

	// Drop a 100-byte attachment row into alice's first workspace. Skip the
	// real upload pipeline (we don't need blob bytes for the SUM check).
	if _, err := s.db.Exec(s.q(`
		INSERT INTO attachments (id, workspace_id, item_id, uploaded_by, storage_key, content_hash, mime_type, size_bytes, filename, created_at)
		VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?)
	`), newID(), aliceWS1, alice.ID, "k1", "h1", "text/plain", int64(100), "a.txt", now()); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	// Plain list (no filters) — all three users, ordered by created_at DESC
	// (carol newest, alice oldest because tests insert in order).
	res, err := s.SearchUsers(AdminUserSearchParams{})
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("total: want 3 got %d", res.Total)
	}
	if len(res.Users) != 3 {
		t.Fatalf("rows: want 3 got %d", len(res.Users))
	}
	byEmail := map[string]AdminUserListEntry{}
	for _, u := range res.Users {
		byEmail[u.Email] = u
	}
	if got := byEmail["alice@example.com"].WorkspaceCount; got != 2 {
		t.Errorf("alice workspace_count: want 2 got %d", got)
	}
	if got := byEmail["alice@example.com"].StorageBytes; got != 100 {
		t.Errorf("alice storage_bytes: want 100 got %d", got)
	}
	if got := byEmail["bob@example.com"].WorkspaceCount; got != 1 {
		t.Errorf("bob workspace_count: want 1 got %d", got)
	}
	if got := byEmail["bob@example.com"].StorageBytes; got != 0 {
		t.Errorf("bob storage_bytes: want 0 got %d", got)
	}
	if got := byEmail["carol@example.com"].WorkspaceCount; got != 0 {
		t.Errorf("carol workspace_count: want 0 got %d", got)
	}
	// Status precedence: carol = disabled (regardless of workspace_count=0).
	if got := byEmail["carol@example.com"].Status; got != "disabled" {
		t.Errorf("carol status: want disabled got %q", got)
	}
	// Bob has a workspace but never wrote anything → inactive.
	if got := byEmail["bob@example.com"].Status; got != "inactive" {
		t.Errorf("bob status: want inactive got %q", got)
	}

	t.Run("filter disabled=true", func(t *testing.T) {
		yes := true
		res, err := s.SearchUsers(AdminUserSearchParams{Disabled: &yes})
		if err != nil {
			t.Fatalf("SearchUsers: %v", err)
		}
		if res.Total != 1 || res.Users[0].Email != "carol@example.com" {
			t.Fatalf("want only carol; got total=%d users=%v", res.Total, res.Users)
		}
	})

	t.Run("filter has_workspaces=true", func(t *testing.T) {
		yes := true
		res, err := s.SearchUsers(AdminUserSearchParams{HasWorkspaces: &yes})
		if err != nil {
			t.Fatalf("SearchUsers: %v", err)
		}
		if res.Total != 2 {
			t.Fatalf("want 2 (alice+bob), got total=%d", res.Total)
		}
	})

	t.Run("sort by workspaces desc", func(t *testing.T) {
		res, err := s.SearchUsers(AdminUserSearchParams{Sort: "workspaces", Order: "desc"})
		if err != nil {
			t.Fatalf("SearchUsers: %v", err)
		}
		if res.Users[0].Email != "alice@example.com" {
			t.Fatalf("want alice first (2 workspaces); got %v", res.Users[0].Email)
		}
	})

	t.Run("sort by storage desc", func(t *testing.T) {
		res, err := s.SearchUsers(AdminUserSearchParams{Sort: "storage", Order: "desc"})
		if err != nil {
			t.Fatalf("SearchUsers: %v", err)
		}
		if res.Users[0].Email != "alice@example.com" {
			t.Fatalf("want alice first (100 bytes); got %v", res.Users[0].Email)
		}
	})

	t.Run("sort by email asc", func(t *testing.T) {
		res, err := s.SearchUsers(AdminUserSearchParams{Sort: "email", Order: "asc"})
		if err != nil {
			t.Fatalf("SearchUsers: %v", err)
		}
		emails := []string{res.Users[0].Email, res.Users[1].Email, res.Users[2].Email}
		want := []string{"alice@example.com", "bob@example.com", "carol@example.com"}
		for i := range want {
			if emails[i] != want[i] {
				t.Fatalf("asc sort: want %v got %v", want, emails)
			}
		}
	})

	t.Run("active_within_days filters on last_write_at", func(t *testing.T) {
		// Touch alice's last_write_at to "now"; bob stays NULL.
		s.TouchUserWrite(t.Context(), alice.ID)
		n := 7
		res, err := s.SearchUsers(AdminUserSearchParams{ActiveWithinDays: &n})
		if err != nil {
			t.Fatalf("SearchUsers: %v", err)
		}
		if res.Total != 1 || res.Users[0].Email != "alice@example.com" {
			t.Fatalf("want only alice; got total=%d users=%v", res.Total, res.Users)
		}
	})

	t.Run("query filter still works", func(t *testing.T) {
		res, err := s.SearchUsers(AdminUserSearchParams{Query: "alice"})
		if err != nil {
			t.Fatalf("SearchUsers: %v", err)
		}
		if res.Total != 1 || res.Users[0].Email != "alice@example.com" {
			t.Fatalf("query filter: want only alice; got total=%d", res.Total)
		}
	})
}

// TestTouchUserWrite verifies the last_write_at bump path:
//   - empty userID is a no-op
//   - first call populates the column
//   - second call within 5 minutes does NOT overwrite (throttle)
//   - a forced backdated timestamp followed by Touch DOES overwrite (past throttle)
func TestTouchUserWrite(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "writer@example.com", "Writer", "password123")
	ctx := t.Context()

	// Empty userID: silent no-op, no error, no row touched.
	s.TouchUserWrite(ctx, "")

	readLastWrite := func() string {
		t.Helper()
		var lw *string
		if err := s.db.QueryRow(s.q(`SELECT last_write_at FROM users WHERE id = ?`), u.ID).Scan(&lw); err != nil {
			t.Fatalf("query last_write_at: %v", err)
		}
		if lw == nil {
			return ""
		}
		return *lw
	}

	if got := readLastWrite(); got != "" {
		t.Fatalf("expected last_write_at to start NULL, got %q", got)
	}

	// First call populates the column.
	s.TouchUserWrite(ctx, u.ID)
	first := readLastWrite()
	if first == "" {
		t.Fatalf("expected last_write_at to be set after first touch")
	}

	// Second call within the throttle window must not overwrite — re-touch
	// then verify the value is unchanged. (Without throttle the value would
	// change because `now()` advances per call.)
	time.Sleep(1100 * time.Millisecond) // ensure now() RFC3339 second-resolution moves forward
	s.TouchUserWrite(ctx, u.ID)
	if got := readLastWrite(); got != first {
		t.Fatalf("expected throttle to suppress second write within 5m; first=%q got=%q", first, got)
	}

	// Backdate to outside the throttle window, then touch — value SHOULD
	// move forward.
	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE users SET last_write_at = ? WHERE id = ?`), old, u.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	s.TouchUserWrite(ctx, u.ID)
	if got := readLastWrite(); got == old {
		t.Fatalf("expected touch outside throttle to advance last_write_at; still %q", got)
	}
}

// insertWithPlanAndDate is a test-only helper that inserts a user with a
// specific plan + created_at timestamp. CreateUser doesn't expose either
// directly. Times are RFC3339-formatted strings (matching store.now) so
// CountBillingAggregates can do a string lex-compare against the cutoff.
func insertWithPlanAndDate(t *testing.T, s *Store, email, plan string, createdAt time.Time) {
	t.Helper()
	id := newID()
	ts := createdAt.UTC().Format(time.RFC3339)
	_, err := s.db.Exec(s.q(`
		INSERT INTO users (id, email, username, name, password_hash, role, plan, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, email, "u_"+id, "Test "+id, "x", "member", plan, ts, ts)
	if err != nil {
		t.Fatalf("insert user with plan=%s: %v", plan, err)
	}
}
