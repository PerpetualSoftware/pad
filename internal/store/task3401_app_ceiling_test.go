package store

import (
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3401 (SPEC-6 U6a): the store applies a bot's install ceiling inside
// VisibleCollectionIDsQ, so every visibility caller gets it.

type task3401Fix struct {
	s                        *Store
	ws                       *models.Workspace
	installID                string
	bot                      *models.User
	companion, system, other *models.Collection
}

func task3401Fixture(t *testing.T, access string) task3401Fix {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Ceiling")
	f := task3401Fix{s: s, ws: ws, installID: "inst-ceiling"}
	task3392Install(t, s, ws.ID, f.installID, "active")
	f.companion = createTestCollection(t, s, ws.ID, "Requests")
	f.system = createTestCollection(t, s, ws.ID, "Conventions Sys")
	f.other = createTestCollection(t, s, ws.ID, "Private")
	if _, err := s.db.Exec(s.q(`UPDATE collections SET via_app = ? WHERE id = ?`), f.installID, f.companion.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE collections SET is_system = ? WHERE id = ?`), true, f.system.ID); err != nil {
		t.Fatal(err)
	}
	f.bot = task3392Bot(t, s, f.installID)
	if _, err := s.db.Exec(s.q(`UPDATE app_installs SET bot_user_id = ? WHERE id = ?`), f.bot.ID, f.installID); err != nil {
		t.Fatal(err)
	}
	task3392Member(t, s, ws.ID, f.bot.ID, "editor")
	if _, err := s.db.Exec(s.q(`UPDATE workspace_members SET collection_access = ? WHERE workspace_id = ? AND user_id = ?`), access, ws.ID, f.bot.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func task3401Sorted(ids []string) string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return strings.Join(out, ",")
}

// A bot with "all" access (as a member) still sees only its companions and
// the system collections: the ceiling does not rely on its membership.
func TestTask3401_ABotsVisibilityIsItsInstallCeiling(t *testing.T) {
	f := task3401Fixture(t, "all")
	got, err := f.s.VisibleCollectionIDsQ(f.s.db, f.ws.ID, f.bot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("a bot's visibility came back nil (unrestricted)")
	}
	want := task3401Sorted([]string{f.companion.ID, f.system.ID})
	if task3401Sorted(got) != want {
		t.Errorf("visible = %v, want only the companion and the system collection", got)
	}
	// The narrower membership wins inside the ceiling.
	if _, err := f.s.db.Exec(f.s.q(`UPDATE workspace_members SET collection_access = 'specific' WHERE workspace_id = ? AND user_id = ?`), f.ws.ID, f.bot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`INSERT INTO member_collection_access (workspace_id, user_id, collection_id, created_at) VALUES (?, ?, ?, ?)`), f.ws.ID, f.bot.ID, f.companion.ID, now()); err != nil {
		t.Fatal(err)
	}
	got, err = f.s.VisibleCollectionIDsQ(f.s.db, f.ws.ID, f.bot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task3401Sorted(got) != f.companion.ID {
		t.Errorf("specific access over the companion: visible = %v", got)
	}
}

// A bot with no install sees nothing; a person's visibility is untouched.
func TestTask3401_OrphanBotSeesNothingAndPeopleAreUnchanged(t *testing.T) {
	f := task3401Fixture(t, "all")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET bot_user_id = NULL WHERE id = ?`), f.installID); err != nil {
		t.Fatal(err)
	}
	// Still found through its address, so still ceilinged.
	got, err := f.s.VisibleCollectionIDsQ(f.s.db, f.ws.ID, f.bot.ID)
	if err != nil || got == nil || task3401Sorted(got) != task3401Sorted([]string{f.companion.ID, f.system.ID}) {
		t.Errorf("found by address: %v %v", got, err)
	}
	// An install that names a different bot disowns this one.
	other := task3392Bot(t, f.s, "inst-other-bot")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET bot_user_id = ? WHERE id = ?`), other.ID, f.installID); err != nil {
		t.Fatal(err)
	}
	got, err = f.s.VisibleCollectionIDsQ(f.s.db, f.ws.ID, f.bot.ID)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("a disowned bot sees %v (%v), want nothing", got, err)
	}
	human := createTestUser(t, f.s, "person@test.com", "Person", "password123")
	if err := f.s.AddWorkspaceMember(f.ws.ID, human.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	got, err = f.s.VisibleCollectionIDsQ(f.s.db, f.ws.ID, human.ID)
	if err != nil || got != nil {
		t.Errorf("a person with all access: %v %v, want nil (unrestricted)", got, err)
	}
}
