package store

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2797. RemapAttachmentReferencesInWorkspace (run by bundle import, while
// the imported workspace is already visible to its owner) is a read-modify-
// write: it scans every item's content and fields, rewrites
// `pad-attachment:OLD` to `pad-attachment:NEW` in Go, and writes the rewritten
// snapshot back. An item edit committing between the scan and the write was
// silently overwritten.
//
// #1328 (PLAN-2857 U5) closed it by making the remap take the workspace seq
// lock that every item writer takes. This file pins that: without the lock the
// test below fails, with it the edit survives.
//
// POSTGRES ONLY, skipped loudly elsewhere. SQLite's BEGIN IMMEDIATE takes the
// write lock at BEGIN and holds it across the whole window, so a concurrent
// write cannot commit inside it whether or not the remap takes the seq lock:
// a green SQLite run would be a property of the DSN, not evidence about the
// lock. The mutation that drops the lock must therefore be run on Postgres.

// TestRemap_ConcurrentItemEditIsNotOverwritten drives the interleaving through
// the afterRemapScan seam. The concurrent edit runs in ANOTHER goroutine,
// because the remap's own transaction holds the lock it needs; the seam then
// waits a bounded time for it.
//
// The concurrent write is a FIELD patch (status open -> done), because the
// remap writes content AND fields back together. That makes the right outcome
// unambiguous whichever order the two commit in: a content edit would carry
// its own stale reference and win as the later writer, which is correct
// last-writer-wins, and would make the remapped-reference check below
// meaningless.
//
// Three assertions, each catching a different wrong outcome (CONVE-12):
//
//   - the edit did NOT complete inside the window. This is the lock itself,
//     observed directly: without it the edit commits within milliseconds.
//   - the edited field survives. Without the lock the remap writes back the
//     fields it scanned ("open") and erases the edit; this is the defect.
//   - the content reference is remapped. A remap that skipped the contended
//     row would keep the edit and abandon the remap's job.
func TestRemap_ConcurrentItemEditIsNotOverwritten(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("asserts a Postgres READ COMMITTED lost-update property; SQLite's BEGIN IMMEDIATE closes the window structurally")
	}
	ws := createTestWorkspace(t, s, "RemapLostUpdate")
	col := createTestCollection(t, s, ws.ID, "Notes")

	const oldID = "0197aaaa-0000-7000-8000-000000000001"
	const newID = "0197bbbb-0000-7000-8000-000000000002"
	item := createTestItem(t, s, ws.ID, col.ID, "Imported", "see ![img](pad-attachment:"+oldID+") here")

	// How long the seam waits for the edit. Without the lock an item update
	// on a local Postgres commits in single-digit milliseconds, so this is
	// generous by two orders of magnitude; with the lock the edit cannot
	// finish at all until the remap commits, so no length makes it pass.
	const window = 2 * time.Second

	var (
		once          sync.Once
		editDone      = make(chan error, 1)
		inWindow      bool
		editStartedAt time.Time
	)
	s.afterRemapScan = func(string) {
		once.Do(func() {
			editStartedAt = time.Now()
			go func() {
				_, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "done"}})
				editDone <- err
			}()
			select {
			case err := <-editDone:
				inWindow = true
				editDone <- err // hand it on to the check after the remap
			case <-time.After(window):
			}
		})
	}
	defer func() { s.afterRemapScan = nil }()

	if err := s.RemapAttachmentReferencesInWorkspace(ws.ID, map[string]string{oldID: newID}); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if editStartedAt.IsZero() {
		t.Fatal("the seam never fired, so this run exercised nothing")
	}
	select {
	case err := <-editDone:
		if err != nil {
			t.Fatalf("the concurrent edit failed, so this run never exercised the race: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the concurrent edit never finished after the remap committed")
	}

	if inWindow {
		t.Errorf("the concurrent edit COMMITTED inside the remap's scan-to-write window: " +
			"nothing serialised it against the remap (the workspace seq lock is not held)")
	}
	got, err := s.GetItem(item.ID)
	if err != nil || got == nil {
		t.Fatalf("GetItem: %v", err)
	}
	// Parsed, not matched as text: Postgres jsonb renders `{"status": "done"}`
	// with a space, which a substring check on the compact form reads as lost.
	var fields map[string]any
	if err := json.Unmarshal([]byte(got.Fields), &fields); err != nil {
		t.Fatalf("decode fields %q: %v", got.Fields, err)
	}
	if fields["status"] != "done" {
		t.Errorf("the concurrent field edit was overwritten by the remap — this is BUG-2797.\n fields: %s", got.Fields)
	}
	if !strings.Contains(got.Content, "pad-attachment:"+newID) || strings.Contains(got.Content, "pad-attachment:"+oldID) {
		t.Errorf("the reference was not remapped on the surviving body.\n got: %q", got.Content)
	}
}
