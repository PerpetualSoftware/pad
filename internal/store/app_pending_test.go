package store

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TASK-3397 (U8a): pending app-install reservations and their caps.

func pendingOwner(t *testing.T, s *Store, n int) string {
	t.Helper()
	return createTestUser(t, s, fmt.Sprintf("owner-%d-%d@example.com", n, time.Now().UnixNano()), "Owner", "password123").ID
}

func TestPendingInstall_PerOwnerCap(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Apps")
	owner := pendingOwner(t, s, 1)
	for i := 0; i < PendingPerOwner; i++ {
		if _, err := s.ReservePendingInstall(ws.ID, owner, "https://app.example"); err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
	}
	if _, err := s.ReservePendingInstall(ws.ID, owner, "https://app.example"); !errors.Is(err, ErrPendingLimit) {
		t.Fatalf("over the per-owner cap: %v", err)
	}
	// Another owner is unaffected.
	if _, err := s.ReservePendingInstall(ws.ID, pendingOwner(t, s, 2), "https://app.example"); err != nil {
		t.Fatalf("another owner: %v", err)
	}
}

func TestPendingInstall_InstanceBytesCap(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Apps")
	max := int(PendingInstanceBytes / PendingInstallBytes)
	for i := 0; i < max; i++ {
		if _, err := s.ReservePendingInstall(ws.ID, pendingOwner(t, s, i), "https://app.example"); err != nil {
			t.Fatalf("reservation %d of %d: %v", i, max, err)
		}
	}
	if _, err := s.ReservePendingInstall(ws.ID, pendingOwner(t, s, max), "https://app.example"); !errors.Is(err, ErrPendingLimit) {
		t.Fatalf("over the instance cap: %v", err)
	}
}

// The full charge is held while fetching, however little has been staged, and
// reduced to the staged bytes only when the fetch ends.
func TestPendingInstall_ChargeIsHeldUntilTheFetchEnds(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Apps")
	max := int(PendingInstanceBytes / PendingInstallBytes)
	var first *PendingInstall
	for i := 0; i < max; i++ {
		p, err := s.ReservePendingInstall(ws.ID, pendingOwner(t, s, i), "https://app.example")
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = p
		}
	}
	if err := s.StagePendingBlob(first.ID, PendingBlob{Key: PendingManifestKey, URL: "https://app.example/m", SHA256: "x", Data: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	// Staging 2 bytes did not release capacity.
	if _, err := s.ReservePendingInstall(ws.ID, pendingOwner(t, s, 99), "https://app.example"); !errors.Is(err, ErrPendingLimit) {
		t.Fatalf("mid-fetch staging released capacity: %v", err)
	}
	reserved := func() int64 {
		var sum int64
		if err := s.db.QueryRow(`SELECT COALESCE(SUM(reserved_bytes), 0) FROM app_install_pending`).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		return sum
	}
	if got := reserved(); got != PendingInstanceBytes {
		t.Fatalf("reserved %d mid-fetch, want the full %d", got, PendingInstanceBytes)
	}
	if err := s.FinishPendingInstall(first.ID, "sha", `{"p":1}`); err != nil {
		t.Fatal(err)
	}
	if got, want := reserved(), PendingInstanceBytes-PendingInstallBytes+2; got != want {
		t.Fatalf("reserved %d after finish, want %d (the charge reduced to the 2 staged bytes)", got, want)
	}
	got, err := s.GetPendingInstall(first.ID, ws.ID, first.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "staged" || got.ReservedBytes != 2 || got.Preview != `{"p":1}` {
		t.Fatalf("after finish: %+v", got)
	}
}

func TestPendingInstall_StagingCannotExceedTheInstallCap(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Apps")
	p, err := s.ReservePendingInstall(ws.ID, pendingOwner(t, s, 1), "https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	half := bytes.Repeat([]byte("a"), int(PendingInstallBytes/2))
	for i, c := range []struct {
		data []byte
		want error
	}{{half, nil}, {half, nil}, {[]byte("x"), ErrPendingOverCap}} {
		err := s.StagePendingBlob(p.ID, PendingBlob{Key: fmt.Sprintf("a%d", i), URL: "https://app.example/a", SHA256: "x", Data: c.data})
		if !errors.Is(err, c.want) {
			t.Fatalf("blob %d: %v, want %v", i, err, c.want)
		}
	}
}

func TestPendingInstall_ScopingDeleteAndSweep(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Apps")
	other := createTestWorkspace(t, s, "Other")
	owner := pendingOwner(t, s, 1)
	p, err := s.ReservePendingInstall(ws.ID, owner, "https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("manifest bytes\x00with a nul")
	if err := s.StagePendingBlob(p.ID, PendingBlob{Key: PendingManifestKey, URL: "https://app.example/m", SHA256: "h", Data: data}); err != nil {
		t.Fatal(err)
	}
	blobs, err := s.PendingInstallBlobs(p.ID)
	if err != nil || !bytes.Equal(blobs[PendingManifestKey].Data, data) {
		t.Fatalf("blob round trip: %v %q", err, blobs[PendingManifestKey].Data)
	}
	for name, args := range map[string][3]string{
		"another owner":     {p.ID, ws.ID, pendingOwner(t, s, 2)},
		"another workspace": {p.ID, other.ID, owner},
		"unknown":           {newID(), ws.ID, owner},
	} {
		if _, err := s.GetPendingInstall(args[0], args[1], args[2]); !errors.Is(err, ErrPendingNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Expire it, then sweep: the record and its blobs go together.
	if _, err := s.db.Exec(s.q(`UPDATE app_install_pending SET expires_at = ? WHERE id = ?`), timeText(time.Now().Add(-time.Minute)), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPendingInstall(p.ID, ws.ID, owner); !errors.Is(err, ErrPendingNotFound) {
		t.Errorf("expired record still readable: %v", err)
	}
	// Expired but NOT yet swept, it already no longer counts against its
	// owner: the caps read expires_at, not the sweep.
	for i := 0; i < PendingPerOwner; i++ {
		if _, err := s.ReservePendingInstall(ws.ID, owner, "https://app.example"); err != nil {
			t.Fatalf("with an expired, unswept reservation, reservation %d: %v", i, err)
		}
	}
	n, err := s.SweepExpiredPendingInstalls()
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	var left int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM app_install_pending_blobs WHERE pending_id = ?`), p.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("blobs left after sweep: %d %v", left, err)
	}
}

// Concurrent reservations never exceed either cap. On SQLite BEGIN IMMEDIATE
// serializes them; on Postgres the owner row lock and the instance advisory
// lock do.
func TestPendingInstall_ConcurrentReservationsRespectBothCaps(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Apps")
	shared := pendingOwner(t, s, 0)
	owners := make([]string, 24)
	for i := range owners {
		owners[i] = pendingOwner(t, s, i+1)
	}
	var mu sync.Mutex
	okShared, okTotal := 0, 0
	var wg sync.WaitGroup
	run := func(owner string, isShared bool) {
		defer wg.Done()
		_, err := s.ReservePendingInstall(ws.ID, owner, "https://app.example")
		switch {
		case err == nil:
			mu.Lock()
			okTotal++
			if isShared {
				okShared++
			}
			mu.Unlock()
		case errors.Is(err, ErrPendingLimit):
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go run(shared, true)
	}
	for _, o := range owners {
		wg.Add(1)
		go run(o, false)
	}
	wg.Wait()
	if okShared > PendingPerOwner {
		t.Errorf("one owner holds %d reservations, cap %d", okShared, PendingPerOwner)
	}
	if max := int(PendingInstanceBytes / PendingInstallBytes); okTotal > max {
		t.Errorf("%d reservations, instance cap %d", okTotal, max)
	}
	var sum int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(reserved_bytes), 0) FROM app_install_pending`).Scan(&sum); err != nil || sum > PendingInstanceBytes {
		t.Errorf("reserved bytes %d > %d (%v)", sum, PendingInstanceBytes, err)
	}
}
