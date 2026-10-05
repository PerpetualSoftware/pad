package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3408 (U10b): admission, the in-flight fence and the drain.

type task3408Fix struct {
	task3394Fix
	hookID    string
	companion *models.Collection
	other     *models.Collection
}

// task3408Fixture: an active install with one companion collection and a
// released hook subscribed to item.created on it.
func task3408Fixture(t *testing.T, installID string) task3408Fix {
	t.Helper()
	return task3408FixtureIn(t, task3394Fixture(t, installID))
}

func task3408FixtureIn(t *testing.T, f task3394Fix) task3408Fix {
	t.Helper()
	installID := f.installID
	companion := createTestCollection(t, f.s, f.ws.ID, "Tickets "+installID)
	other := createTestCollection(t, f.s, f.ws.ID, "Other "+installID)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE collections SET via_app = ? WHERE id = ?`), installID, companion.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.upsertAppWebhookTx(tx, f.ws.ID, installID, &AppWebhookSpec{URL: "https://portal.example/hooks",
		Events: []AppWebhookEvent{{Name: "item.created", CollectionSlugs: []string{companion.Slug}}}}); err != nil {
		t.Fatal(err)
	}
	secret, err := f.s.rotateAppWebhookSecretTx(tx, installID)
	if err != nil || secret == "" {
		t.Fatalf("release: %q %v", secret, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var hookID string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM webhooks WHERE app_install_id = ?`), installID).Scan(&hookID); err != nil {
		t.Fatal(err)
	}
	return task3408Fix{task3394Fix: f, hookID: hookID, companion: companion, other: other}
}

func (f task3408Fix) inflight(t *testing.T) int {
	t.Helper()
	return task3394Count(t, f.s, `SELECT COUNT(*) FROM app_delivery_inflight WHERE install_id = ?`, f.installID)
}

func wantRefused(t *testing.T, err error, reason string) {
	t.Helper()
	var r *AppDeliveryRefusedError
	if !errors.As(err, &r) || r.Reason != reason {
		t.Fatalf("got %v, want refusal %q", err, reason)
	}
}

func TestTask3408_AdmissionRecordsInFlightInDatabaseTime(t *testing.T) {
	f := task3408Fixture(t, "inst-admit")
	adm, err := f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), "d1")
	if err != nil {
		t.Fatal(err)
	}
	if adm.URL != "https://portal.example/hooks" || adm.InstallID != f.installID || adm.Secret == "" {
		t.Fatalf("admission %+v", adm)
	}
	if n, err := f.s.liveAppDeliveries(f.installID); err != nil || n != 1 {
		t.Fatalf("live deliveries %d (%v), want 1", n, err)
	}
	// The expiry is the database's now + 12 s: unexpired now, expired 13 s
	// from now, both asked of the database.
	q := `SELECT COUNT(*) FROM app_delivery_inflight WHERE delivery_id = 'd1' AND expires_at > strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '+11 seconds') AND expires_at <= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '+13 seconds')`
	if f.s.dialect.Driver() == DriverPostgres {
		q = `SELECT COUNT(*) FROM app_delivery_inflight WHERE delivery_id = 'd1' AND expires_at > now() + interval '11 seconds' AND expires_at <= now() + interval '13 seconds'`
	}
	if n := task3394Count(t, f.s, q); n != 1 {
		t.Fatal("the in-flight row's expiry is not database now + 12 s")
	}
	if err := f.s.EndAppDelivery("d1"); err != nil || f.inflight(t) != 0 {
		t.Fatalf("end: %v, %d rows left", err, f.inflight(t))
	}
}

func TestTask3408_AdmissionRefusals(t *testing.T) {
	f := task3408Fixture(t, "inst-refuse")
	// Not subscribed: another event, or the subscribed event on a collection
	// the subscription does not name.
	_, err := f.s.AdmitAppDelivery(f.hookID, "item.updated", f.companion.ID, now(), "r1")
	wantRefused(t, err, "not_subscribed")
	_, err = f.s.AdmitAppDelivery(f.hookID, "item.created", f.other.ID, now(), "r2")
	wantRefused(t, err, "not_subscribed")

	// The ceiling: a companion no longer the app's.
	if _, err := f.s.db.Exec(f.s.q(`UPDATE collections SET via_app = NULL WHERE id = ?`), f.companion.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), "r3")
	wantRefused(t, err, "not_visible")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE collections SET via_app = ? WHERE id = ?`), f.installID, f.companion.ID); err != nil {
		t.Fatal(err)
	}

	// Held: secret not delivered.
	if _, err := f.s.db.Exec(f.s.q(`UPDATE webhooks SET secret_delivered_at = NULL WHERE id = ?`), f.hookID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), "r4")
	wantRefused(t, err, "hook_held")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE webhooks SET secret_delivered_at = ? WHERE id = ?`), now(), f.hookID); err != nil {
		t.Fatal(err)
	}

	// Install not active: phase 1 of a disable.
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownDisable); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), "r5")
	wantRefused(t, err, "install_disabling")

	// A hook that does not exist.
	_, err = f.s.AdmitAppDelivery("no-such-hook", "item.created", f.companion.ID, now(), "r6")
	wantRefused(t, err, "hook_gone")

	if f.inflight(t) != 0 {
		t.Fatalf("a refused admission left %d in-flight rows", f.inflight(t))
	}
}

func TestTask3408_DrainWaitsForInFlight(t *testing.T) {
	f := task3408Fixture(t, "inst-drain")
	if _, err := f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), "w1"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownDisable); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.DrainInstallDeliveries(context.Background(), f.installID) }()
	select {
	case err := <-done:
		t.Fatalf("the drain returned (%v) while an admitted attempt was in flight", err)
	case <-time.After(600 * time.Millisecond):
	}
	if err := f.s.EndAppDelivery("w1"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not return after the attempt ended")
	}
}

func TestTask3408_DrainIgnoresExpiredAndTimesOut(t *testing.T) {
	f := task3408Fixture(t, "inst-expire")
	past, future := `strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-1 seconds')`, `strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '+1 hours')`
	if f.s.dialect.Driver() == DriverPostgres {
		past, future = `now() - interval '1 second'`, `now() + interval '1 hour'`
	}
	// An expired row (a crashed attempt) does not hold the drain.
	if _, err := f.s.db.Exec(f.s.q(`INSERT INTO app_delivery_inflight (delivery_id, install_id, expires_at) VALUES ('old', ?, `+past+`)`), f.installID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DrainInstallDeliveries(context.Background(), f.installID); err != nil {
		t.Fatalf("an expired row held the drain: %v", err)
	}
	// A row that does not expire within the bound: the drain gives up.
	if _, err := f.s.db.Exec(f.s.q(`INSERT INTO app_delivery_inflight (delivery_id, install_id, expires_at) VALUES ('stuck', ?, `+future+`)`), f.installID); err != nil {
		t.Fatal(err)
	}
	was := appDeliveryDrainBound
	appDeliveryDrainBound = 400 * time.Millisecond
	defer func() { appDeliveryDrainBound = was }()
	if err := f.s.DrainInstallDeliveries(context.Background(), f.installID); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("got %v, want ErrDrainTimeout", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.s.DrainInstallDeliveries(ctx, f.installID); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// Admission and phase 1 race: once BeginInstallTeardown has returned, no
// admission succeeds, and the drain returns once the admitted attempts end.
func TestTask3408_AdmissionRacesPhaseOne(t *testing.T) {
	f := task3408Fixture(t, "inst-race")
	var phaseOneDone atomic.Bool
	var violations, admitted atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				startedAfter := phaseOneDone.Load()
				id := newID()
				_, err := f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), id)
				if err == nil {
					admitted.Add(1)
					if startedAfter {
						violations.Add(1)
					}
					time.Sleep(2 * time.Millisecond)
					_ = f.s.EndAppDelivery(id)
					continue
				}
				var r *AppDeliveryRefusedError
				if !errors.As(err, &r) {
					t.Errorf("admission error: %v", err)
					return
				}
			}
		}(w)
	}
	time.Sleep(150 * time.Millisecond)
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownDisable); err != nil {
		t.Fatal(err)
	}
	phaseOneDone.Store(true)
	if err := f.s.DrainInstallDeliveries(context.Background(), f.installID); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n, _ := f.s.liveAppDeliveries(f.installID); n != 0 {
		t.Fatalf("%d live deliveries after the drain returned", n)
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	if admitted.Load() == 0 {
		t.Fatal("no admission succeeded before phase 1: the race was not exercised")
	}
	if v := violations.Load(); v != 0 {
		t.Fatalf("%d admissions that started after phase 1 returned succeeded", v)
	}
}

// Rotate holds the hook again with a secret nobody holds.
func TestTask3408_RotateHoldsTheHook(t *testing.T) {
	f := task3408Fixture(t, "inst-rotate")
	var before string
	if err := f.s.db.QueryRow(f.s.q(`SELECT secret FROM webhooks WHERE id = ?`), f.hookID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownRotate); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.FinishRotate(f.ws.ID, f.installID); err != nil {
		t.Fatal(err)
	}
	var after string
	var delivered *string
	if err := f.s.db.QueryRow(f.s.q(`SELECT secret, secret_delivered_at FROM webhooks WHERE id = ?`), f.hookID).Scan(&after, &delivered); err != nil {
		t.Fatal(err)
	}
	if after == before || delivered != nil {
		t.Fatalf("rotate left the hook releasing the old secret (changed %v, delivered %v)", after != before, delivered)
	}
	_, err := f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), "after-rotate")
	wantRefused(t, err, "hook_held")
}

// An admitted attempt whose secret cannot be decrypted releases its record
// at once, rather than holding a drain for the record's lifetime.
func TestTask3408_UndecryptableSecretReleasesTheRecord(t *testing.T) {
	f := task3394Fixture(t, "inst-decrypt")
	f.s.SetEncryptionKey([]byte("0123456789abcdef0123456789abcdef"))
	g := task3408FixtureIn(t, f)
	f.s.SetEncryptionKey([]byte("fedcba9876543210fedcba9876543210"))
	_, err := f.s.AdmitAppDelivery(g.hookID, "item.created", g.companion.ID, now(), "dx")
	var r *AppDeliveryRefusedError
	if err == nil || errors.As(err, &r) {
		t.Fatalf("got %v, want a store error", err)
	}
	if g.inflight(t) != 0 {
		t.Fatal("an unsendable admission left its in-flight record")
	}
}

// codex r1 on U10b: a soft-deleted workspace keeps its installs and
// collections; its pending events are refused at admission.
func TestTask3408_DeletedWorkspaceRefusesAdmission(t *testing.T) {
	f := task3408Fixture(t, "inst-wsdel")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE workspaces SET deleted_at = ? WHERE id = ?`), now(), f.ws.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), "wd")
	wantRefused(t, err, "workspace_deleted")
}

// codex r4 on U10b: a comment an app wrote carries its install as the
// creator's via_app, as an app-created item does.
func TestTask3408_CommentBlockRecordsTheWritingInstall(t *testing.T) {
	f := task3408Fixture(t, "inst-cvia")
	item, err := f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "T", Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.s.CreateComment(f.ws.ID, item.ID, "", models.CommentCreate{Body: "hi", Author: "A"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	p, err := f.s.buildCommentAppProjectionTx(tx, c.ID, item.ID, c.UserID, c.Author, c.CreatedBy, "")
	if err != nil || p == nil || p.Creator.ViaApp != "" {
		t.Fatalf("a person's comment: %+v %v", p, err)
	}
	if _, err := tx.Exec(f.s.q(`UPDATE comments SET via_app = ? WHERE id = ?`), f.installID, c.ID); err != nil {
		t.Fatal(err)
	}
	p, err = f.s.buildCommentAppProjectionTx(tx, c.ID, item.ID, c.UserID, c.Author, c.CreatedBy, "")
	if err != nil || p.Creator.ViaApp != f.installID {
		t.Fatalf("an app's comment: %+v %v", p, err)
	}
}

// codex r5 on U10b: an event older than the hook's deliver_from is refused.
func TestTask3408_EventsBeforeDeliverFromAreRefused(t *testing.T) {
	f := task3408Fixture(t, "inst-from")
	_, err := f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, "2000-01-01T00:00:00Z", "old")
	wantRefused(t, err, "before_deliverable")
	if _, err := f.s.AdmitAppDelivery(f.hookID, "item.created", f.companion.ID, now(), "new"); err != nil {
		t.Fatalf("a current event: %v", err)
	}
}

// A subscription change moves deliver_from; an unchanged one does not.
func TestTask3408_SubscriptionChangeMovesDeliverFrom(t *testing.T) {
	f := task3408Fixture(t, "inst-subs")
	set := func(v string) {
		if _, err := f.s.db.Exec(f.s.q(`UPDATE webhooks SET deliver_from = ? WHERE id = ?`), v, f.hookID); err != nil {
			t.Fatal(err)
		}
	}
	get := func() string {
		var v string
		if err := f.s.db.QueryRow(f.s.q(`SELECT deliver_from FROM webhooks WHERE id = ?`), f.hookID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	upsert := func(events ...string) {
		spec := &AppWebhookSpec{URL: "https://portal.example/hooks"}
		for _, e := range events {
			spec.Events = append(spec.Events, AppWebhookEvent{Name: e, CollectionSlugs: []string{f.companion.Slug}})
		}
		tx, err := f.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.upsertAppWebhookTx(tx, f.ws.ID, f.installID, spec); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	const old = "2000-01-01T00:00:00Z"
	set(old)
	upsert("item.created")
	if got := get(); got != old {
		t.Fatalf("an unchanged subscription moved deliver_from to %s", got)
	}
	upsert("item.created", "item.updated")
	if got := get(); got == old {
		t.Fatal("a changed subscription left deliver_from where it was")
	}
}

// codex r6 on U10b: hooks released under migration 122 get a deliver_from
// from their secret_delivered_at; held hooks stay NULL; set ones are kept.
func TestTask3408_DeliverFromBackfill(t *testing.T) {
	f := task3408Fixture(t, "inst-backfill")
	fs, path := migrationsFS, "migrations/123_app_delivery_inflight.sql"
	if f.s.dialect.Driver() != DriverSQLite {
		fs, path = pgMigrationsFS, "pgmigrations/097_app_delivery_inflight.sql"
	}
	body, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var backfill string
	for _, stmt := range strings.Split(string(body), ";") {
		var lines []string
		for _, l := range strings.Split(stmt, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				lines = append(lines, l)
			}
		}
		if q := strings.TrimSpace(strings.Join(lines, "\n")); strings.HasPrefix(q, "UPDATE webhooks") {
			backfill = q
		}
	}
	if backfill == "" {
		t.Fatalf("%s: no UPDATE webhooks statement", path)
	}
	// Three hooks: released (as under 122), held, and already stamped.
	g := task3408FixtureIn(t, task3394FixtureIn(t, f.s, f.ws, "inst-backfill-held"))
	h := task3408FixtureIn(t, task3394FixtureIn(t, f.s, f.ws, "inst-backfill-set"))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.s.db.Exec(f.s.q(q), args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE webhooks SET deliver_from = NULL, secret_delivered_at = '2026-01-01T00:00:00Z' WHERE id = ?`, f.hookID)
	exec(`UPDATE webhooks SET deliver_from = NULL, secret_delivered_at = NULL WHERE id = ?`, g.hookID)
	exec(`UPDATE webhooks SET deliver_from = '2026-05-05T00:00:00Z' WHERE id = ?`, h.hookID)
	if _, err := f.s.db.Exec(backfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	read := func(id string) string {
		var v *string
		if err := f.s.db.QueryRow(f.s.q(`SELECT deliver_from FROM webhooks WHERE id = ?`), id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return "NULL"
		}
		return *v
	}
	if got := read(f.hookID); got != "2026-01-01T00:00:00Z" {
		t.Errorf("released hook: %s", got)
	}
	if got := read(g.hookID); got != "NULL" {
		t.Errorf("held hook: %s", got)
	}
	if got := read(h.hookID); got != "2026-05-05T00:00:00Z" {
		t.Errorf("stamped hook: %s", got)
	}
}
