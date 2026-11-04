package store

import (
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4315: users.chat_enabled, a per-user display preference that defaults
// off. Migration 164 (SQLite) / 129 (Postgres) turns it on once for every user
// who has already sent a Chat message themselves.

// task4315CarryOverSQL pulls the carry-over UPDATE out of the embedded
// migration, so the test runs the shipped text and not a copy of it.
func task4315CarryOverSQL(t *testing.T, s *Store) string {
	t.Helper()
	name, read := "migrations/164_user_chat_enabled.sql", migrationsFS.ReadFile
	if s.dialect.Driver() == DriverPostgres {
		name, read = "pgmigrations/129_user_chat_enabled.sql", pgMigrationsFS.ReadFile
	}
	data, err := read(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	text := string(data)
	start := strings.Index(text, "UPDATE users SET chat_enabled")
	if start < 0 {
		t.Fatalf("%s: carry-over UPDATE not found", name)
	}
	end := strings.Index(text[start:], ";")
	if end < 0 {
		t.Fatalf("%s: carry-over UPDATE has no terminator", name)
	}
	return text[start : start+end]
}

func task4315ChatEnabled(t *testing.T, s *Store, id string) bool {
	t.Helper()
	u, err := s.GetUser(id)
	if err != nil || u == nil {
		t.Fatalf("GetUser(%s): %v", id, err)
	}
	return u.ChatEnabled
}

func task4315Send(t *testing.T, s *Store, convID string, author models.ConversationActor, body, source string) *models.ConversationMessage {
	t.Helper()
	res, err := s.SendConversationMessage(models.ConversationSend{
		ConversationID: convID, Author: author, Body: body, Source: source,
	})
	if err != nil {
		t.Fatalf("send %q: %v", body, err)
	}
	return &res.Message
}

func TestTASK4315_CarryOver(t *testing.T) {
	s := testStore(t)
	t.Logf("dialect: %s", s.dialect.Driver())
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" && s.dialect.Driver() != DriverPostgres {
		t.Fatalf("PAD_TEST_POSTGRES_URL is set but the store is %s", s.dialect.Driver())
	}

	dm := func(u *models.User) string {
		c, _, err := s.EnsureAgentDM(u.ID, "claude")
		if err != nil {
			t.Fatalf("EnsureAgentDM: %v", err)
		}
		return c.ID
	}
	human := func(u *models.User) models.ConversationActor { return models.ConversationActor{UserID: u.ID} }
	agent := func(u *models.User) models.ConversationActor {
		return models.ConversationActor{UserID: u.ID, AgentKey: "claude"}
	}

	sentOne := createTestUser(t, s, "sent@example.com", "Sent One", "")
	never := createTestUser(t, s, "never@example.com", "Never", "")
	receivedOnly := createTestUser(t, s, "received@example.com", "Received Only", "")
	tombstoned := createTestUser(t, s, "tomb@example.com", "Tombstoned", "")
	viaCLI := createTestUser(t, s, "cli@example.com", "Via CLI", "")
	both := createTestUser(t, s, "both@example.com", "Both", "")

	task4315Send(t, s, dm(sentOne), human(sentOne), "hello", "web")
	_ = never
	// The case the predicate exists for: an agent's message is recorded under
	// the owner's author_user_id with a non-empty author_agent_key.
	task4315Send(t, s, dm(receivedOnly), agent(receivedOnly), "agent says hi", "agent")
	tm := task4315Send(t, s, dm(tombstoned), human(tombstoned), "oops", "web")
	if _, _, err := s.DeleteConversationMessage(human(tombstoned), dm(tombstoned), tm.Seq); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	task4315Send(t, s, dm(viaCLI), human(viaCLI), "from the cli", "cli")
	task4315Send(t, s, dm(both), agent(both), "agent first", "agent")
	task4315Send(t, s, dm(both), human(both), "human reply", "web")

	// Before the carry-over, everyone is at the column default: off.
	for _, u := range []*models.User{sentOne, never, receivedOnly, tombstoned, viaCLI, both} {
		if task4315ChatEnabled(t, s, u.ID) {
			t.Fatalf("%s starts on; the default is off", u.Email)
		}
	}

	stmt := task4315CarryOverSQL(t, s)
	if _, err := s.db.Exec(s.q(stmt)); err != nil {
		t.Fatalf("carry-over: %v", err)
	}

	want := map[string]bool{
		"sent":     true,
		"never":    false,
		"received": false,
		"tomb":     true,
		"cli":      true,
		"both":     true,
	}
	check := func(label string) {
		t.Helper()
		for _, u := range []*models.User{sentOne, never, receivedOnly, tombstoned, viaCLI, both} {
			key := strings.SplitN(u.Email, "@", 2)[0]
			if got := task4315ChatEnabled(t, s, u.ID); got != want[key] {
				t.Errorf("%s: %s chat_enabled = %v, want %v", label, u.Email, got, want[key])
			}
		}
	}
	check("after carry-over")

	// A user created after the migration is off, even with agent traffic.
	late := createTestUser(t, s, "late@example.com", "Late", "")
	task4315Send(t, s, dm(late), agent(late), "agent hello", "agent")
	if task4315ChatEnabled(t, s, late.ID) {
		t.Error("a user created after the migration starts on")
	}

	// Re-running changes nothing: it only ever sets 1, and only for senders.
	if _, err := s.db.Exec(s.q(stmt)); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	check("after re-run")
	if task4315ChatEnabled(t, s, late.ID) {
		t.Error("the re-run turned on a user who never sent a message")
	}

	// It only ever sets 1: a user turned on by hand, with no messages, is
	// left on.
	if _, err := s.UpdateUser(never.ID, models.UserUpdate{ChatEnabled: ptrBool(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(stmt)); err != nil {
		t.Fatalf("re-run 2: %v", err)
	}
	if !task4315ChatEnabled(t, s, never.ID) {
		t.Error("the carry-over turned a user off")
	}
}

func ptrBool(b bool) *bool { return &b }

func TestTASK4315_UpdateUserChatEnabled(t *testing.T) {
	s := testStore(t)
	a := createTestUser(t, s, "a4315@example.com", "A", "")
	b := createTestUser(t, s, "b4315@example.com", "B", "")
	if a.ChatEnabled {
		t.Fatal("a new user starts on")
	}
	tz := "Europe/London"
	if _, err := s.UpdateUser(a.ID, models.UserUpdate{Timezone: &tz}); err != nil {
		t.Fatal(err)
	}
	u, err := s.UpdateUser(a.ID, models.UserUpdate{ChatEnabled: ptrBool(true)})
	if err != nil || !u.ChatEnabled {
		t.Fatalf("set on: %v %+v", err, u)
	}
	if u.Timezone == nil || *u.Timezone != tz {
		t.Errorf("the chat write disturbed the timezone: %v", u.Timezone)
	}
	if task4315ChatEnabled(t, s, b.ID) {
		t.Error("setting A turned B on")
	}
	// A write that does not name it leaves it alone.
	name := "Renamed"
	u, err = s.UpdateUser(a.ID, models.UserUpdate{Name: &name})
	if err != nil || !u.ChatEnabled {
		t.Fatalf("name-only update: %v %+v", err, u)
	}
	u, err = s.UpdateUser(a.ID, models.UserUpdate{ChatEnabled: ptrBool(false)})
	if err != nil || u.ChatEnabled {
		t.Fatalf("set off: %v %+v", err, u)
	}
	// The admin list carries it.
	if _, err := s.UpdateUser(b.ID, models.UserUpdate{ChatEnabled: ptrBool(true)}); err != nil {
		t.Fatal(err)
	}
	res, err := s.SearchUsers(AdminUserSearchParams{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range res.Users {
		got[e.ID] = e.ChatEnabled
	}
	if got[a.ID] || !got[b.ID] {
		t.Errorf("admin list chat_enabled: a=%v b=%v, want false/true", got[a.ID], got[b.ID])
	}
}

// A binary older than this migration must refuse a database that has it, with
// the documented schema-ahead message. There is no special downgrade handling:
// Pad is forward-only, and the override is `pad start --force`.
func TestTASK4315_SchemaAheadGuard(t *testing.T) {
	cases := []struct {
		name      string
		applied string
		fsNames func() ([]string, error)
	}{
		{"sqlite", "164_user_chat_enabled.sql",
			func() ([]string, error) { return readMigrationNames(migrationsFS, "migrations") }},
		{"postgres", "129_user_chat_enabled.sql",
			func() ([]string, error) { return readMigrationNames(pgMigrationsFS, "pgmigrations") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names, err := tc.fsNames()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, n := range names {
				found = found || n == tc.applied
			}
			if !found {
				t.Fatalf("%s is not embedded", tc.applied)
			}
			// The old binary ships every migration before the new file; the
			// database has those plus the new file. Later migrations are left
			// out of both, so this does not break when 165 or 130 lands.
			var old []string
			applied := map[string]bool{}
			for _, n := range names {
				if n < tc.applied {
					old = append(old, n)
					applied[n] = true
				}
			}
			applied[tc.applied] = true
			wantNewer := old[len(old)-1]
			prev := AllowSchemaAhead
			AllowSchemaAhead = false
			t.Cleanup(func() { AllowSchemaAhead = prev })
			err = guardSchemaAhead(applied, old)
			if err == nil {
				t.Fatal("an old binary started on a database with the new migration")
			}
			msg := err.Error()
			for _, part := range []string{
				"database schema is newer than this pad binary",
				"1 migration(s)",
				"(" + tc.applied + ")",
				"the newest known to this binary is " + wantNewer,
				"`pad start --force` (or PAD_ALLOW_SCHEMA_AHEAD=1)",
			} {
				if !strings.Contains(msg, part) {
					t.Errorf("error lacks %q: %s", part, msg)
				}
			}
		})
	}
}
