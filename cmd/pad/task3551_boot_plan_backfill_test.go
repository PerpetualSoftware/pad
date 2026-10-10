package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3551 at the boot: a cloud boot marks the database, and a later
// non-cloud boot of the same database leaves its free users free instead of
// converting them to self-hosted (unlimited). Without the cloud boot, the
// non-cloud boot converts as it always has.

func freeUser(t *testing.T, s *store.Store, email string) string {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{Email: email, Name: "U", Password: "s3cret-pass"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BackfillUserPlans("free"); err != nil { // '' -> free, as a cloud boot does
		t.Fatal(err)
	}
	return u.ID
}

func planIs(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	u, err := s.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}
	return u.Plan
}

func TestBootPlanBackfill_CloudBootThenSelfHostedBootLeavesFreeUsersFree(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "cloud.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := freeUser(t, s, "cloud-user@example.com")

	if err := bootPlanBackfill(s, true); err != nil {
		t.Fatalf("cloud boot: %v", err)
	}
	if err := bootPlanBackfill(s, false); err != nil {
		t.Fatalf("a non-cloud boot of a cloud database must not fail the boot: %v", err)
	}
	if got := planIs(t, s, id); got != "free" {
		t.Fatalf("after a cloud boot and then a non-cloud boot, plan = %q, want free", got)
	}
}

// Control: a self-hosted database (never booted as cloud) converts.
func TestBootPlanBackfill_SelfHostedBootConverts(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "selfhost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := freeUser(t, s, "sh-user@example.com")

	if err := bootPlanBackfill(s, false); err != nil {
		t.Fatal(err)
	}
	if got := planIs(t, s, id); got != "self-hosted" {
		t.Fatalf("plan = %q, want self-hosted", got)
	}
}

// release-cloud against the database the server would open (PAD_DB_PATH):
// refused while billing state remains, then forced; the next non-cloud boot
// converts.
func TestDBReleaseCloudCmd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "release.db")
	t.Setenv("PAD_DB_DRIVER", "")
	t.Setenv("PAD_DATA_DIR", "")
	t.Setenv("PAD_DB_PATH", dbPath)
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id := freeUser(t, s, "leaving@example.com")
	if err := s.SetUserStripeCustomerID(id, "cus_1"); err != nil {
		t.Fatal(err)
	}
	if err := bootPlanBackfill(s, true); err != nil {
		t.Fatal(err)
	}
	s.Close()

	run := func(args ...string) (string, error) {
		cmd := dbReleaseCloudCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	if _, err := run(); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("release without --force = %v, want a refusal naming --force", err)
	}
	if out, err := run("--force"); err != nil || !strings.Contains(out, "Released") {
		t.Fatalf("release --force: out=%q err=%v", out, err)
	}

	s, err = store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := bootPlanBackfill(s, false); err != nil {
		t.Fatal(err)
	}
	if got := planIs(t, s, id); got != "self-hosted" {
		t.Fatalf("after release-cloud, the non-cloud boot left plan = %q, want self-hosted", got)
	}
}
