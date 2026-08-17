package store

// BUG-2415 — the orphan-GC claim protocol at the store layer: writer-side
// pad-attachment: reference stamps (stampAttachmentRefsTx via the content
// writers) and the conditional Claim* deletes whose predicates ARE the
// serialization. The filed race is pinned as an explicit sequence test,
// plus a counterfactual arm proving the stamp condition — not something
// else — is what refuses the claim.

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

type gcClaimFixture struct {
	s      *Store
	wsID   string
	collID string
}

func newGCClaimFixture(t *testing.T) *gcClaimFixture {
	t.Helper()
	s := testStore(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "GC Claim WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	return &gcClaimFixture{s: s, wsID: ws.ID, collID: coll.ID}
}

// seedNeverAttached inserts a never-attached attachment row aged past any
// grace cutoff a test will use.
func (f *gcClaimFixture) seedNeverAttached(t *testing.T) *models.Attachment {
	t.Helper()
	variant := "original"
	a := &models.Attachment{
		ID:          newID(),
		WorkspaceID: f.wsID,
		StorageKey:  "fs:deadbeef" + newID()[:8],
		ContentHash: "hash-" + newID()[:8],
		Filename:    "orphan.png",
		MimeType:    "image/png",
		SizeBytes:   42,
		Variant:     &variant,
	}
	if err := f.s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	// Age it so a candidate SELECT with a now-ish cutoff matches.
	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE attachments SET created_at = ? WHERE id = ?`), old, a.ID); err != nil {
		t.Fatalf("age attachment: %v", err)
	}
	return a
}

func (f *gcClaimFixture) lastReferencedAt(t *testing.T, id string) *string {
	t.Helper()
	var v *string
	if err := f.s.db.QueryRow(f.s.q(`SELECT last_referenced_at FROM attachments WHERE id = ?`), id).Scan(&v); err != nil {
		t.Fatalf("read last_referenced_at: %v", err)
	}
	return v
}

func (f *gcClaimFixture) rowExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(f.s.q(`SELECT COUNT(*) FROM attachments WHERE id = ?`), id).Scan(&n); err != nil {
		t.Fatalf("count row: %v", err)
	}
	return n == 1
}

func TestStampAttachmentRefs_ItemContentAndFields(t *testing.T) {
	f := newGCClaimFixture(t)
	inContent := f.seedNeverAttached(t)
	inFields := f.seedNeverAttached(t)
	unrelated := f.seedNeverAttached(t)

	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{
		Title:   "stamps",
		Content: "see ![img](pad-attachment:" + inContent.ID + ")",
		Fields:  `{"status":"open","cover":"pad-attachment:` + inFields.ID + `"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if f.lastReferencedAt(t, inContent.ID) == nil {
		t.Errorf("content-referenced attachment not stamped on create")
	}
	if f.lastReferencedAt(t, inFields.ID) == nil {
		t.Errorf("fields-referenced attachment not stamped on create")
	}
	if f.lastReferencedAt(t, unrelated.ID) != nil {
		t.Errorf("unreferenced attachment stamped spuriously")
	}

	// Update path: a new reference arriving via UpdateItem stamps too.
	late := f.seedNeverAttached(t)
	newContent := "now also [f](pad-attachment:" + late.ID + ")"
	if _, err := f.s.UpdateItem(item.ID, models.ItemUpdate{Content: &newContent}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if f.lastReferencedAt(t, late.ID) == nil {
		t.Errorf("attachment referenced via UpdateItem not stamped")
	}
}

func TestStampAttachmentRefs_Comments(t *testing.T) {
	f := newGCClaimFixture(t)
	inCreate := f.seedNeverAttached(t)
	inEdit := f.seedNeverAttached(t)

	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{Title: "host"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	c, err := f.s.CreateComment(f.wsID, item.ID, "", models.CommentCreate{
		Body: "look: ![x](pad-attachment:" + inCreate.ID + ")",
	})
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if f.lastReferencedAt(t, inCreate.ID) == nil {
		t.Errorf("comment-create reference not stamped")
	}
	if _, err := f.s.UpdateComment(c.ID, "edited: pad-attachment:"+inEdit.ID); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if f.lastReferencedAt(t, inEdit.ID) == nil {
		t.Errorf("comment-edit reference not stamped")
	}
}

func TestStampAttachmentRefs_WorkspaceScoped(t *testing.T) {
	f := newGCClaimFixture(t)
	foreign := f.seedNeverAttached(t)

	// A second workspace pastes a reference to the first workspace's
	// attachment id — it must NOT refresh the foreign row.
	ws2, err := f.s.CreateWorkspace(models.WorkspaceCreate{Name: "Other WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll2, err := f.s.CreateCollection(ws2.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := f.s.CreateItem(ws2.ID, coll2.ID, models.ItemCreate{
		Title:   "foreign paste",
		Content: "stolen ref pad-attachment:" + foreign.ID,
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if f.lastReferencedAt(t, foreign.ID) != nil {
		t.Errorf("foreign-workspace paste refreshed another workspace's attachment")
	}
}

func TestClaimNeverAttached_PredicateLegs(t *testing.T) {
	f := newGCClaimFixture(t)
	cutoff := time.Now().Add(-15 * time.Minute)

	// Leg 1: unstamped row → claimable.
	plain := f.seedNeverAttached(t)
	claimed, err := f.s.ClaimNeverAttachedAttachment(plain.ID, cutoff)
	if err != nil || !claimed {
		t.Fatalf("unstamped claim = (%v, %v), want (true, nil)", claimed, err)
	}
	if f.rowExists(t, plain.ID) {
		t.Errorf("claimed row still present")
	}

	// Leg 2: fresh stamp → refused, row survives.
	fresh := f.seedNeverAttached(t)
	nowTS := time.Now().UTC().Format(time.RFC3339)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE attachments SET last_referenced_at = ? WHERE id = ?`), nowTS, fresh.ID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	claimed, err = f.s.ClaimNeverAttachedAttachment(fresh.ID, cutoff)
	if err != nil || claimed {
		t.Fatalf("fresh-stamp claim = (%v, %v), want (false, nil)", claimed, err)
	}
	if !f.rowExists(t, fresh.ID) {
		t.Errorf("refused claim deleted the row anyway")
	}

	// Leg 3 (counterfactual for leg 2): the SAME row claims fine once
	// the cutoff moves past its stamp — proving the stamp condition,
	// not some other predicate, is what refused above.
	claimed, err = f.s.ClaimNeverAttachedAttachment(fresh.ID, time.Now().Add(time.Minute))
	if err != nil || !claimed {
		t.Fatalf("stale-stamp claim = (%v, %v), want (true, nil)", claimed, err)
	}

	// Leg 4: attached row → refused regardless of stamp.
	attached := f.seedNeverAttached(t)
	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{Title: "owner"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE attachments SET item_id = ? WHERE id = ?`), item.ID, attached.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	claimed, err = f.s.ClaimNeverAttachedAttachment(attached.ID, cutoff)
	if err != nil || claimed {
		t.Fatalf("attached claim = (%v, %v), want (false, nil)", claimed, err)
	}

	// Leg 5: soft-deleted row → wrong class, refused.
	soft := f.seedNeverAttached(t)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE attachments SET deleted_at = ? WHERE id = ?`), nowTS, soft.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	claimed, err = f.s.ClaimNeverAttachedAttachment(soft.ID, cutoff)
	if err != nil || claimed {
		t.Fatalf("soft-deleted claim via never-attached = (%v, %v), want (false, nil)", claimed, err)
	}
}

func TestClaimSoftDeleted_RefusesRestoredRow(t *testing.T) {
	f := newGCClaimFixture(t)
	a := f.seedNeverAttached(t)
	oldTS := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE attachments SET deleted_at = ? WHERE id = ?`), oldTS, a.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Restore between the candidate SELECT and the claim: clear
	// deleted_at, then claim with a cutoff that WOULD have reclaimed it.
	if _, err := f.s.db.Exec(f.s.q(`UPDATE attachments SET deleted_at = NULL WHERE id = ?`), a.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	claimed, err := f.s.ClaimSoftDeletedAttachment(a.ID, time.Now())
	if err != nil || claimed {
		t.Fatalf("restored-row claim = (%v, %v), want (false, nil)", claimed, err)
	}
	if !f.rowExists(t, a.ID) {
		t.Errorf("restored row deleted anyway")
	}

	// Counterfactual: re-soft-delete past grace → claims.
	if _, err := f.s.db.Exec(f.s.q(`UPDATE attachments SET deleted_at = ? WHERE id = ?`), oldTS, a.ID); err != nil {
		t.Fatalf("re-soft-delete: %v", err)
	}
	claimed, err = f.s.ClaimSoftDeletedAttachment(a.ID, time.Now())
	if err != nil || !claimed {
		t.Fatalf("past-grace claim = (%v, %v), want (true, nil)", claimed, err)
	}
}

// TestClaimProtocol_FiledRaceSequence pins BUG-2415's exact interleaving
// at the store layer: the sweep's content scan finds no reference, a
// writer then commits a reference (whose transaction stamps the row),
// and the claim that follows must refuse — row survives, and since the
// blob is only reclaimed after a successful claim (row-before-bytes in
// the sweep), the bytes survive by construction.
func TestClaimProtocol_FiledRaceSequence(t *testing.T) {
	f := newGCClaimFixture(t)
	a := f.seedNeverAttached(t)

	// Step 1 — the sweep's scan: no reference anywhere.
	referenced, err := f.s.AttachmentReferenced(f.wsID, a.ID)
	if err != nil {
		t.Fatalf("AttachmentReferenced: %v", err)
	}
	if referenced {
		t.Fatalf("precondition: attachment unexpectedly referenced")
	}

	// Step 2 — a writer commits a reference AFTER the scan. The stamp
	// rides the same transaction as the content write.
	if _, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{
		Title:   "late reference",
		Content: "raced: ![x](pad-attachment:" + a.ID + ")",
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Step 3 — the sweep's claim runs with the production-shaped cutoff
	// (now - stale window). The fresh stamp must refuse it.
	claimed, err := f.s.ClaimNeverAttachedAttachment(a.ID, time.Now().Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatalf("claim succeeded against a just-referenced attachment — the filed data-loss race")
	}
	if !f.rowExists(t, a.ID) {
		t.Errorf("row destroyed despite live reference")
	}
}

// TestStampAttachmentRefs_StampsVariantsOfReferencedOriginal pins the
// codex-round-4 half of the variant story: stamping a referenced
// ORIGINAL also stamps rows whose parent_id points at it, so a
// concurrently-claimed thumbnail is protected by its OWN row's stamp
// (and, on Postgres, its own row lock) — not just the claim's parent
// NOT EXISTS belt.
func TestStampAttachmentRefs_StampsVariantsOfReferencedOriginal(t *testing.T) {
	f := newGCClaimFixture(t)
	orig := f.seedNeverAttached(t)

	// A variant hanging off the original.
	thumbID := orig.ID + "-thumb"
	if _, err := f.s.db.Exec(f.s.q(
		`INSERT INTO attachments (id, workspace_id, item_id, uploaded_by, storage_key, content_hash, mime_type, size_bytes, filename, parent_id, variant, created_at)
		 VALUES (?, ?, NULL, '', ?, ?, 'image/png', 10, 't.png', ?, 'thumb-md', ?)`),
		thumbID, f.wsID, "fs:t-"+thumbID, "h-t-"+thumbID, orig.ID,
		time.Now().Add(-40*24*time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert variant: %v", err)
	}

	if _, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{
		Title:   "ref holder",
		Content: "ref ![x](pad-attachment:" + orig.ID + ")",
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	if f.lastReferencedAt(t, orig.ID) == nil {
		t.Errorf("original not stamped")
	}
	if f.lastReferencedAt(t, thumbID) == nil {
		t.Errorf("variant of referenced original not stamped")
	}
	// And the variant's own fresh stamp refuses its claim directly.
	claimed, err := f.s.ClaimNeverAttachedAttachment(thumbID, time.Now().Add(-15*time.Minute))
	if err != nil || claimed {
		t.Fatalf("fresh-own-stamp variant claim = (%v, %v), want (false, nil)", claimed, err)
	}
}
