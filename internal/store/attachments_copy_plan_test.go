package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/items"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// planFixture is two source workspaces (A, and a third-party workspace C
// used for the confused-deputy tests) plus a destination workspace B.
type planFixture struct {
	s     *Store
	wsA   *models.Workspace
	wsB   *models.Workspace
	wsC   *models.Workspace
	actor string
}

func newPlanFixture(t *testing.T) planFixture {
	t.Helper()
	s := testStore(t)
	return planFixture{
		s:     s,
		wsA:   createTestWorkspace(t, s, "Plan Source"),
		wsB:   createTestWorkspace(t, s, "Plan Dest"),
		wsC:   createTestWorkspace(t, s, "Plan Third Party"),
		actor: "copier-user",
	}
}

// attach creates an original attachment in ws and returns it.
func (f planFixture) attach(t *testing.T, ws *models.Workspace, filename string, size int64) *models.Attachment {
	t.Helper()
	return f.attachWith(t, ws, filename, size, "fs:"+newID(), newID())
}

func (f planFixture) attachWith(t *testing.T, ws *models.Workspace, filename string, size int64, storageKey, hash string) *models.Attachment {
	t.Helper()
	width, height := 800, 600
	a := &models.Attachment{
		WorkspaceID: ws.ID,
		UploadedBy:  "original-uploader",
		StorageKey:  storageKey,
		ContentHash: hash,
		MimeType:    "image/png",
		SizeBytes:   size,
		Filename:    filename,
		Width:       &width,
		Height:      &height,
	}
	if err := f.s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment(%s): %v", filename, err)
	}
	return a
}

// variant creates a derived thumbnail row for parent, in ws.
func (f planFixture) variant(t *testing.T, ws *models.Workspace, parent *models.Attachment, kind string, size int64) *models.Attachment {
	t.Helper()
	parentID := parent.ID
	variant := kind
	a := &models.Attachment{
		WorkspaceID: ws.ID,
		UploadedBy:  "original-uploader",
		StorageKey:  "fs:" + newID(),
		ContentHash: newID(),
		MimeType:    "image/webp",
		SizeBytes:   size,
		Filename:    kind + "-" + parent.Filename,
		ParentID:    &parentID,
		Variant:     &variant,
	}
	if err := f.s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment(variant %s): %v", kind, err)
	}
	return a
}

// req builds a request from A to B with the standard actor.
func (f planFixture) req(content string, fields map[string]any) AttachmentCopyRequest {
	return AttachmentCopyRequest{
		SourceWorkspaceID: f.wsA.ID,
		TargetWorkspaceID: f.wsB.ID,
		TargetItemID:      "dest-item-id",
		UploadedBy:        f.actor,
		Content:           content,
		Fields:            fields,
	}
}

func (f planFixture) plan(t *testing.T, req AttachmentCopyRequest) *AttachmentCopyPlan {
	t.Helper()
	plan, err := f.s.PlanAttachmentCopy(req)
	if err != nil {
		t.Fatalf("PlanAttachmentCopy: %v", err)
	}
	return plan
}

func imageRef(id string) string { return fmt.Sprintf("![shot](pad-attachment:%s)", id) }
func fileRef(id string) string  { return fmt.Sprintf("[spec](pad-attachment:%s)", id) }
func sourceIDs(p *AttachmentCopyPlan) []string {
	out := make([]string, 0, len(p.Rows))
	for _, r := range p.Rows {
		out = append(out, r.SourceID)
	}
	return out
}

func assertSourceSet(t *testing.T, p *AttachmentCopyPlan, want ...string) {
	t.Helper()
	got := sourceIDs(p)
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if strings.Join(gotSorted, ",") != strings.Join(wantSorted, ",") {
		t.Fatalf("planned source ids = %v, want %v", got, want)
	}
}

// --- reference enumeration -------------------------------------------------

// TestPlanAttachmentCopy_RefsInContentOnly is the base case: one image
// reference in the body, one row, one map entry, the bytes counted.
func TestPlanAttachmentCopy_RefsInContentOnly(t *testing.T) {
	f := newPlanFixture(t)
	a := f.attach(t, f.wsA, "shot.png", 1234)

	plan := f.plan(t, f.req("intro\n\n"+imageRef(a.ID)+"\n", nil))

	assertSourceSet(t, plan, a.ID)
	if len(plan.IDMap) != 1 || plan.IDMap[a.ID] == "" {
		t.Fatalf("IDMap = %v, want one entry keyed by %s", plan.IDMap, a.ID)
	}
	if plan.IDMap[a.ID] == a.ID {
		t.Errorf("new id equals source id — the copy must get a fresh UUID")
	}
	if plan.TotalBytes != 1234 {
		t.Errorf("TotalBytes = %d, want 1234", plan.TotalBytes)
	}
	if len(plan.UnresolvableRefs) != 0 {
		t.Errorf("UnresolvableRefs = %v, want none", plan.UnresolvableRefs)
	}
}

// TestPlanAttachmentCopy_RefsInFieldsOnly proves fields are enumerated —
// a reference living only in a field value (no markdown around it, nested
// inside an array) must still be cloned.
func TestPlanAttachmentCopy_RefsInFieldsOnly(t *testing.T) {
	f := newPlanFixture(t)
	bare := f.attach(t, f.wsA, "bare.png", 10)
	nested := f.attach(t, f.wsA, "nested.pdf", 20)

	plan := f.plan(t, f.req("no refs here", map[string]any{
		"cover":       "pad-attachment:" + bare.ID,
		"attachments": []any{"noise", fileRef(nested.ID)},
	}))

	assertSourceSet(t, plan, bare.ID, nested.ID)
	if plan.TotalBytes != 30 {
		t.Errorf("TotalBytes = %d, want 30", plan.TotalBytes)
	}
}

// TestPlanAttachmentCopy_RefsInBoth covers the union, and pins that the
// two sources are merged rather than one shadowing the other.
func TestPlanAttachmentCopy_RefsInBoth(t *testing.T) {
	f := newPlanFixture(t)
	inBody := f.attach(t, f.wsA, "body.png", 5)
	inField := f.attach(t, f.wsA, "field.png", 7)

	plan := f.plan(t, f.req(imageRef(inBody.ID), map[string]any{"cover": "pad-attachment:" + inField.ID}))

	assertSourceSet(t, plan, inBody.ID, inField.ID)
	if plan.TotalBytes != 12 {
		t.Errorf("TotalBytes = %d, want 12", plan.TotalBytes)
	}
}

// TestPlanAttachmentCopy_SameRefTwice: the same attachment referenced in
// the body twice AND in a field gets ONE new UUID and is counted once.
// Two rows would mean the rewrite maps the old id to whichever new id won,
// and the dry-run would double the bytes.
func TestPlanAttachmentCopy_SameRefTwice(t *testing.T) {
	f := newPlanFixture(t)
	a := f.attach(t, f.wsA, "shot.png", 900)

	content := imageRef(a.ID) + "\n\nand again " + fileRef(a.ID)
	plan := f.plan(t, f.req(content, map[string]any{"cover": "pad-attachment:" + a.ID}))

	if len(plan.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1 (same blob referenced three times)", len(plan.Rows))
	}
	if len(plan.IDMap) != 1 {
		t.Fatalf("IDMap = %v, want exactly one entry", plan.IDMap)
	}
	if plan.TotalBytes != 900 {
		t.Errorf("TotalBytes = %d, want 900 (counted once)", plan.TotalBytes)
	}
}

// TestPlanAttachmentCopy_EmptyRefsIgnored: a bare prefix with no id
// resolves to nothing to clone and nothing to count — it is not a
// reference, so it is not an unresolvable reference either.
func TestPlanAttachmentCopy_EmptyRefsIgnored(t *testing.T) {
	f := newPlanFixture(t)

	plan := f.plan(t, f.req("see pad-attachment: and pad-attachment:)", map[string]any{
		"note": "pad-attachment:",
	}))

	if len(plan.Rows) != 0 || len(plan.UnresolvableRefs) != 0 {
		t.Fatalf("rows=%v unresolvable=%v, want both empty", sourceIDs(plan), plan.UnresolvableRefs)
	}
}

// TestPlanAttachmentCopy_JunkSuffixIsUnresolvable pins the deliberate
// choice not to narrow the id capture to the canonical UUID shape.
//
// `pad-attachment:<uuid>x` captures the whole token, resolves to nothing,
// and is counted. It is NOT partially matched back to <uuid> and cloned.
// The alternative — matching only 36-char RFC4122 ids — would make any
// non-UUID attachment id silently un-copyable, trading a real failure for
// a cosmetic one: this body was already broken in workspace A (nothing
// resolves `<uuid>x` there either), and it stays exactly as broken in B.
func TestPlanAttachmentCopy_JunkSuffixIsUnresolvable(t *testing.T) {
	f := newPlanFixture(t)
	a := f.attach(t, f.wsA, "shot.png", 77)

	plan := f.plan(t, f.req("![x](pad-attachment:"+a.ID+"x)", nil))

	if len(plan.Rows) != 0 {
		t.Fatalf("rows = %v, want none — a junk-suffixed id must not resolve to its prefix", sourceIDs(plan))
	}
	if len(plan.UnresolvableRefs) != 1 || plan.UnresolvableRefs[0] != a.ID+"x" {
		t.Errorf("UnresolvableRefs = %v, want [%sx]", plan.UnresolvableRefs, a.ID)
	}
}

// TestPlanAttachmentCopy_NonUUIDIdResolves is the other half of that
// choice: an attachment whose id is not RFC4122-shaped is still
// enumerated and still cloned. A canonical-UUID-only regex would drop it.
func TestPlanAttachmentCopy_NonUUIDIdResolves(t *testing.T) {
	f := newPlanFixture(t)
	a := &models.Attachment{
		ID:          "legacy_img-42",
		WorkspaceID: f.wsA.ID,
		UploadedBy:  "original-uploader",
		StorageKey:  "fs:" + newID(),
		ContentHash: newID(),
		MimeType:    "image/png",
		SizeBytes:   17,
		Filename:    "legacy.png",
	}
	if err := f.s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}

	plan := f.plan(t, f.req(imageRef(a.ID), nil))

	assertSourceSet(t, plan, a.ID)
}

// TestPlanAttachmentCopy_RefsInCodeFencesAreCloned pins a deliberate
// choice: enumeration matches the rewriter's reach (a plain ReplaceAll
// over "pad-attachment:<old>"), which does not respect code fences. If the
// planner skipped fenced references the rewrite would leave workspace A's
// UUID in the copied body, and that UUID 403s on download from B.
func TestPlanAttachmentCopy_RefsInCodeFencesAreCloned(t *testing.T) {
	f := newPlanFixture(t)
	a := f.attach(t, f.wsA, "shot.png", 42)

	plan := f.plan(t, f.req("```\n"+imageRef(a.ID)+"\n```\n", nil))

	assertSourceSet(t, plan, a.ID)
}

// --- the ordering bug (DR-11) ----------------------------------------------

// TestPlanAttachmentCopy_DroppedFieldNotCloned is the ordering bug DR-11
// calls out, driven through the real MigrateFields.
//
// The source item has a `screenshot` field the destination collection does
// not have, so MigrateFields drops it. The planner is handed the FINAL
// fields, so the attachment referenced only by that dropped field must not
// be cloned: cloning it would put a blob in workspace B with nothing
// referencing it — invisible, and un-GC-able because item_id is set — and
// would overstate the dry-run's byte total.
func TestPlanAttachmentCopy_DroppedFieldNotCloned(t *testing.T) {
	f := newPlanFixture(t)
	kept := f.attach(t, f.wsA, "kept.png", 100)
	dropped := f.attach(t, f.wsA, "dropped.png", 999999)

	sourceSchema := []models.FieldDef{
		{Key: "cover", Type: "text", Label: "Cover"},
		{Key: "screenshot", Type: "text", Label: "Screenshot"},
	}
	targetSchema := []models.FieldDef{
		{Key: "cover", Type: "text", Label: "Cover"},
	}
	rawFields := map[string]any{
		"cover":      "pad-attachment:" + kept.ID,
		"screenshot": "pad-attachment:" + dropped.ID,
	}

	migrated := items.MigrateFields(rawFields, sourceSchema, targetSchema, items.SameWorkspace)
	if len(migrated.Dropped) != 1 || migrated.Dropped[0] != "screenshot" {
		t.Fatalf("precondition: MigrateFields dropped %v, want [screenshot]", migrated.Dropped)
	}

	plan := f.plan(t, f.req("body with no refs", migrated.Fields))

	assertSourceSet(t, plan, kept.ID)
	if _, ok := plan.IDMap[dropped.ID]; ok {
		t.Errorf("dropped-field attachment %s was cloned — the planner re-read raw fields", dropped.ID)
	}
	if plan.TotalBytes != 100 {
		t.Errorf("TotalBytes = %d, want 100 (the dropped blob must not inflate the dry-run)", plan.TotalBytes)
	}
	if len(plan.UnresolvableRefs) != 0 {
		t.Errorf("UnresolvableRefs = %v — a dropped field is not an unresolvable reference, it is no reference at all", plan.UnresolvableRefs)
	}
}

// --- DR-11a: the confused deputy -------------------------------------------

// TestPlanAttachmentCopy_ForeignRefNotCloned is the security case. The
// user writes a reference to an attachment owned by a THIRD workspace into
// an item they control, then copies it. Resolution is scoped to
// workspace A, so the foreign row resolves to nothing: not cloned, no
// bytes, literal text preserved (no IDMap entry), counted as unresolvable.
//
// Cloning it would put another workspace's blob into a workspace the user
// controls, bypassing the download handler's workspace check
// (handleGetAttachment, handlers_attachments.go) entirely.
func TestPlanAttachmentCopy_ForeignRefNotCloned(t *testing.T) {
	f := newPlanFixture(t)
	mine := f.attach(t, f.wsA, "mine.png", 11)
	theirs := f.attach(t, f.wsC, "theirs.png", 5_000_000)

	plan := f.plan(t, f.req(imageRef(mine.ID)+"\n"+imageRef(theirs.ID), nil))

	assertSourceSet(t, plan, mine.ID)
	if _, ok := plan.IDMap[theirs.ID]; ok {
		t.Fatalf("foreign attachment %s was cloned — DR-11a scope is missing", theirs.ID)
	}
	if plan.TotalBytes != 11 {
		t.Errorf("TotalBytes = %d, want 11", plan.TotalBytes)
	}
	if len(plan.UnresolvableRefs) != 1 || plan.UnresolvableRefs[0] != theirs.ID {
		t.Errorf("UnresolvableRefs = %v, want [%s]", plan.UnresolvableRefs, theirs.ID)
	}
}

// TestPlanAttachmentCopy_SoftDeletedRefNotCloned: a tombstoned attachment
// is out of scope exactly like a foreign one. Cloning it would resurrect
// bytes the workspace already deleted.
func TestPlanAttachmentCopy_SoftDeletedRefNotCloned(t *testing.T) {
	f := newPlanFixture(t)
	gone := f.attach(t, f.wsA, "gone.png", 64)
	if err := f.s.SoftDeleteAttachment(gone.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}

	plan := f.plan(t, f.req(imageRef(gone.ID), nil))

	if len(plan.Rows) != 0 {
		t.Fatalf("rows = %v, want none", sourceIDs(plan))
	}
	if plan.TotalBytes != 0 {
		t.Errorf("TotalBytes = %d, want 0", plan.TotalBytes)
	}
	if len(plan.UnresolvableRefs) != 1 || plan.UnresolvableRefs[0] != gone.ID {
		t.Errorf("UnresolvableRefs = %v, want [%s]", plan.UnresolvableRefs, gone.ID)
	}
}

// TestPlanAttachmentCopy_DanglingRefNotFatal: a reference matching nothing
// at all is reported, never cloned, and never blocks the copy — a stale
// reference is a pre-existing condition. The resolvable siblings still
// plan normally.
func TestPlanAttachmentCopy_DanglingRefNotFatal(t *testing.T) {
	f := newPlanFixture(t)
	real := f.attach(t, f.wsA, "real.png", 8)
	ghost := newID()

	plan := f.plan(t, f.req(imageRef(ghost)+"\n"+imageRef(real.ID), nil))

	assertSourceSet(t, plan, real.ID)
	if len(plan.UnresolvableRefs) != 1 || plan.UnresolvableRefs[0] != ghost {
		t.Errorf("UnresolvableRefs = %v, want [%s]", plan.UnresolvableRefs, ghost)
	}
}

// --- variants ---------------------------------------------------------------

// TestPlanAttachmentCopy_VariantsFollowParent: one referenced original
// with two thumbnails produces three rows, the thumbnails' parent_id
// remapped to the NEW original's id, and the original emitted first so a
// caller inserting in order never writes a parent_id ahead of its parent.
func TestPlanAttachmentCopy_VariantsFollowParent(t *testing.T) {
	f := newPlanFixture(t)
	orig := f.attach(t, f.wsA, "shot.png", 1000)
	sm := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbSm, 10)
	md := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbMd, 20)

	plan := f.plan(t, f.req(imageRef(orig.ID), nil))

	assertSourceSet(t, plan, orig.ID, sm.ID, md.ID)
	if plan.Rows[0].SourceID != orig.ID {
		t.Fatalf("Rows[0].SourceID = %s, want the original %s", plan.Rows[0].SourceID, orig.ID)
	}
	newOriginalID := plan.Rows[0].Attachment.ID
	for _, row := range plan.Rows[1:] {
		if row.Attachment.ParentID == nil {
			t.Fatalf("variant row %s lost its parent_id", row.SourceID)
		}
		if *row.Attachment.ParentID != newOriginalID {
			t.Errorf("variant %s parent_id = %s, want the NEW original %s",
				row.SourceID, *row.Attachment.ParentID, newOriginalID)
		}
	}
	if plan.TotalBytes != 1030 {
		t.Errorf("TotalBytes = %d, want 1030 (original + both thumbs)", plan.TotalBytes)
	}
	// The map covers derived rows too, so nothing referencing a thumbnail
	// by id survives the rewrite pointing at workspace A.
	if plan.IDMap[sm.ID] == "" || plan.IDMap[md.ID] == "" {
		t.Errorf("IDMap = %v, want entries for both variants", plan.IDMap)
	}
}

// TestPlanAttachmentCopy_ForeignVariantNotFollowed is the one-level-down
// escalation: a variant row in a THIRD workspace whose parent_id points at
// workspace A's original must not be dragged in by the variant traversal.
// An unscoped `WHERE parent_id IN (...)` would clone it.
func TestPlanAttachmentCopy_ForeignVariantNotFollowed(t *testing.T) {
	f := newPlanFixture(t)
	orig := f.attach(t, f.wsA, "shot.png", 100)
	ours := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbSm, 5)
	// Same parent_id, different workspace — user-controllable in the
	// sense that any workspace can hold a row with an arbitrary parent_id.
	theirs := f.variant(t, f.wsC, orig, models.AttachmentVariantThumbMd, 9_000)

	plan := f.plan(t, f.req(imageRef(orig.ID), nil))

	assertSourceSet(t, plan, orig.ID, ours.ID)
	if _, ok := plan.IDMap[theirs.ID]; ok {
		t.Fatalf("foreign variant %s was cloned — the variant query lost the DR-11a scope", theirs.ID)
	}
	if plan.TotalBytes != 105 {
		t.Errorf("TotalBytes = %d, want 105", plan.TotalBytes)
	}
}

// TestPlanAttachmentCopy_SoftDeletedVariantNotFollowed: the variant
// traversal carries the deleted_at half of the scope too.
func TestPlanAttachmentCopy_SoftDeletedVariantNotFollowed(t *testing.T) {
	f := newPlanFixture(t)
	orig := f.attach(t, f.wsA, "shot.png", 100)
	dead := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbSm, 7)
	if err := f.s.SoftDeleteAttachment(dead.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}

	plan := f.plan(t, f.req(imageRef(orig.ID), nil))

	assertSourceSet(t, plan, orig.ID)
	if plan.TotalBytes != 100 {
		t.Errorf("TotalBytes = %d, want 100", plan.TotalBytes)
	}
}

// TestPlanAttachmentCopy_RefToVariantPullsInParent: content is
// user-controlled, so a reference can name a thumbnail directly. Cloning
// it alone would emit a row whose parent_id still points into workspace A,
// so the in-scope parent is pulled in as the clone root and the whole
// variant set comes with it.
func TestPlanAttachmentCopy_RefToVariantPullsInParent(t *testing.T) {
	f := newPlanFixture(t)
	orig := f.attach(t, f.wsA, "shot.png", 100)
	sm := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbSm, 5)

	plan := f.plan(t, f.req(imageRef(sm.ID), nil))

	assertSourceSet(t, plan, orig.ID, sm.ID)
	if plan.Rows[0].SourceID != orig.ID {
		t.Fatalf("Rows[0].SourceID = %s, want the parent %s first", plan.Rows[0].SourceID, orig.ID)
	}
	if got := plan.Rows[1].Attachment.ParentID; got == nil || *got != plan.Rows[0].Attachment.ID {
		t.Errorf("variant parent_id = %v, want the new original %s", got, plan.Rows[0].Attachment.ID)
	}
	if len(plan.UnresolvableRefs) != 0 {
		t.Errorf("UnresolvableRefs = %v, want none", plan.UnresolvableRefs)
	}
}

// TestPlanAttachmentCopy_RefToVariantWithForeignParent: the same
// escalation from the other direction. The referenced thumbnail lives in
// workspace A but its parent lives elsewhere, so following the parent
// would clone a foreign original. The reference is unresolvable instead.
func TestPlanAttachmentCopy_RefToVariantWithForeignParent(t *testing.T) {
	f := newPlanFixture(t)
	foreignOrig := f.attach(t, f.wsC, "theirs.png", 4_000)
	localThumb := f.variant(t, f.wsA, foreignOrig, models.AttachmentVariantThumbSm, 6)

	plan := f.plan(t, f.req(imageRef(localThumb.ID), nil))

	if len(plan.Rows) != 0 {
		t.Fatalf("rows = %v, want none", sourceIDs(plan))
	}
	if _, ok := plan.IDMap[foreignOrig.ID]; ok {
		t.Fatalf("foreign parent %s was cloned via its in-workspace variant", foreignOrig.ID)
	}
	if plan.TotalBytes != 0 {
		t.Errorf("TotalBytes = %d, want 0", plan.TotalBytes)
	}
	if len(plan.UnresolvableRefs) != 1 || plan.UnresolvableRefs[0] != localThumb.ID {
		t.Errorf("UnresolvableRefs = %v, want [%s]", plan.UnresolvableRefs, localThumb.ID)
	}
}

// --- row shape --------------------------------------------------------------

// TestPlanAttachmentCopy_RowShape pins every column the caller inserts:
// fresh id, destination workspace, destination item from the outset (never
// transiently NULL), uploaded_by = the copying actor and NOT the source
// uploader (who may not be a member of B at all), and the
// content-addressed columns carried over verbatim so a same-instance copy
// is a row copy rather than a byte copy.
func TestPlanAttachmentCopy_RowShape(t *testing.T) {
	f := newPlanFixture(t)
	src := f.attach(t, f.wsA, "shot.png", 4096)

	plan := f.plan(t, f.req(imageRef(src.ID), nil))
	got := plan.Rows[0].Attachment

	if got.ID == src.ID || got.ID == "" {
		t.Errorf("id = %q, want a fresh non-empty UUID", got.ID)
	}
	if got.WorkspaceID != f.wsB.ID {
		t.Errorf("workspace_id = %s, want destination %s", got.WorkspaceID, f.wsB.ID)
	}
	if got.ItemID == nil || *got.ItemID != "dest-item-id" {
		t.Errorf("item_id = %v, want dest-item-id (never transiently NULL)", got.ItemID)
	}
	if got.UploadedBy != f.actor {
		t.Errorf("uploaded_by = %q, want the copying actor %q, not the source uploader %q",
			got.UploadedBy, f.actor, src.UploadedBy)
	}
	if got.StorageKey != src.StorageKey || got.ContentHash != src.ContentHash {
		t.Errorf("storage_key/content_hash = %q/%q, want them carried over (%q/%q)",
			got.StorageKey, got.ContentHash, src.StorageKey, src.ContentHash)
	}
	if got.MimeType != src.MimeType || got.SizeBytes != src.SizeBytes || got.Filename != src.Filename {
		t.Errorf("mime/size/filename not carried over: %+v", got)
	}
	if got.Width == nil || *got.Width != 800 || got.Height == nil || *got.Height != 600 {
		t.Errorf("width/height not carried over: %v/%v", got.Width, got.Height)
	}
	if got.ParentID != nil {
		t.Errorf("parent_id = %v, want nil for an original", got.ParentID)
	}
	if got.DeletedAt != nil {
		t.Errorf("deleted_at = %v, want nil", got.DeletedAt)
	}
	if !got.CreatedAt.IsZero() {
		t.Errorf("created_at = %v, want zero so CreateAttachment stamps now()", got.CreatedAt)
	}
	if plan.Rows[0].NeedsByteTransfer || plan.CrossBackend {
		t.Errorf("same-backend copy flagged as needing a byte transfer")
	}
}

// TestPlanAttachmentCopy_DryRunAllowsEmptyTargetItem: a dry-run has no
// destination item yet and writes nothing, so an empty TargetItemID is
// legal there and simply leaves item_id nil.
func TestPlanAttachmentCopy_DryRunAllowsEmptyTargetItem(t *testing.T) {
	f := newPlanFixture(t)
	src := f.attach(t, f.wsA, "shot.png", 3)

	req := f.req(imageRef(src.ID), nil)
	req.TargetItemID = ""
	req.DryRun = true
	plan := f.plan(t, req)

	if plan.Rows[0].Attachment.ItemID != nil {
		t.Errorf("item_id = %v, want nil for a dry-run plan", plan.Rows[0].Attachment.ItemID)
	}
	if plan.TotalBytes != 3 {
		t.Errorf("TotalBytes = %d, want 3 — a dry-run still reports the real total", plan.TotalBytes)
	}
}

// TestPlanAttachmentCopy_InsertablePlanRequiresTargetItem: without the
// DryRun opt-in, an empty TargetItemID is an error rather than a plan full
// of item_id=NULL rows. Inserting those would permanently orphan the
// blobs — referenced by the copied body, so never GC'd, and invisible
// through every item-scoped surface.
func TestPlanAttachmentCopy_InsertablePlanRequiresTargetItem(t *testing.T) {
	f := newPlanFixture(t)
	src := f.attach(t, f.wsA, "shot.png", 3)

	req := f.req(imageRef(src.ID), nil)
	req.TargetItemID = ""
	_, err := f.s.PlanAttachmentCopy(req)
	if err == nil || !strings.Contains(err.Error(), "target_item_id") {
		t.Fatalf("err = %v, want one naming target_item_id", err)
	}
}

// --- cross-backend ----------------------------------------------------------

// TestPlanAttachmentCopy_CrossBackendDetected: when the destination writes
// to a different backend, the shared-storage_key shortcut does not hold.
// The plan must say so per row rather than emit a key the target backend
// cannot resolve.
func TestPlanAttachmentCopy_CrossBackendDetected(t *testing.T) {
	f := newPlanFixture(t)
	src := f.attachWith(t, f.wsA, "shot.png", 12, "fs:deadbeef", "deadbeef")

	req := f.req(imageRef(src.ID), nil)
	req.TargetBackend = "s3"
	plan := f.plan(t, req)

	if !plan.CrossBackend || !plan.Rows[0].NeedsByteTransfer {
		t.Fatalf("cross-backend not detected: CrossBackend=%v row=%+v", plan.CrossBackend, plan.Rows[0])
	}
	// The emitted row must NOT carry a key the s3 destination cannot
	// resolve. An "fs:" key would insert cleanly and fail at download.
	if plan.Rows[0].Attachment.StorageKey != "" {
		t.Errorf("storage_key = %q, want empty — the caller fills it from Put after transferring bytes",
			plan.Rows[0].Attachment.StorageKey)
	}
	if plan.Rows[0].SourceStorageKey != "fs:deadbeef" {
		t.Errorf("SourceStorageKey = %q, want the source key so the caller can Get the bytes",
			plan.Rows[0].SourceStorageKey)
	}
}

// TestPlanAttachmentCopy_CrossBackendRowUninsertable closes the loop on
// the sentinel: the blank StorageKey is not merely a convention the
// orchestration is asked to honour. CreateAttachment refuses it, so an
// orchestration that forgets the Get/Put step fails loudly at insert
// rather than quietly creating an attachment that 404s on download.
func TestPlanAttachmentCopy_CrossBackendRowUninsertable(t *testing.T) {
	f := newPlanFixture(t)
	src := f.attachWith(t, f.wsA, "shot.png", 12, "fs:deadbeef", "deadbeef")

	req := f.req(imageRef(src.ID), nil)
	req.TargetBackend = "s3"
	plan := f.plan(t, req)

	row := plan.Rows[0].Attachment
	if err := f.s.CreateAttachment(&row); err == nil {
		t.Fatal("CreateAttachment accepted a row with no storage_key")
	} else if !strings.Contains(err.Error(), "storage_key") {
		t.Errorf("err = %v, want one naming storage_key", err)
	}

	// After the caller transfers the bytes, the same row inserts fine.
	row.StorageKey = "s3:deadbeef"
	row.ItemID = nil // no destination item exists in this unit fixture
	if err := f.s.CreateAttachment(&row); err != nil {
		t.Fatalf("CreateAttachment after byte transfer: %v", err)
	}
}

// TestPlanAttachmentCopy_SameBackendNoTransfer: naming the backend
// explicitly, when it matches, must NOT trigger a pointless byte copy.
func TestPlanAttachmentCopy_SameBackendNoTransfer(t *testing.T) {
	f := newPlanFixture(t)
	src := f.attachWith(t, f.wsA, "shot.png", 12, "fs:deadbeef", "deadbeef")

	req := f.req(imageRef(src.ID), nil)
	req.TargetBackend = "fs"
	plan := f.plan(t, req)

	if plan.CrossBackend || plan.Rows[0].NeedsByteTransfer {
		t.Errorf("same backend flagged as cross-backend")
	}
	if plan.Rows[0].Attachment.StorageKey != "fs:deadbeef" {
		t.Errorf("storage_key = %q, want the source key — a same-backend copy is a row copy",
			plan.Rows[0].Attachment.StorageKey)
	}
	if plan.Rows[0].SourceStorageKey != "fs:deadbeef" {
		t.Errorf("SourceStorageKey = %q, want it populated on every row", plan.Rows[0].SourceStorageKey)
	}
}

// TestPlanAttachmentCopy_PrefixlessKeyNeedsTransfer: a storage_key with no
// backend prefix cannot be routed by the registry, so it is treated as
// needing a real transfer rather than assumed resolvable.
func TestPlanAttachmentCopy_PrefixlessKeyNeedsTransfer(t *testing.T) {
	f := newPlanFixture(t)
	src := f.attachWith(t, f.wsA, "shot.png", 12, "legacy-key-no-prefix", "hash1")

	req := f.req(imageRef(src.ID), nil)
	req.TargetBackend = "fs"
	plan := f.plan(t, req)

	if !plan.CrossBackend || !plan.Rows[0].NeedsByteTransfer {
		t.Errorf("prefixless storage_key must be flagged for byte transfer")
	}
}

// --- byte accounting --------------------------------------------------------

// TestPlanAttachmentCopy_DedupedBlobsCountedPerRow: two distinct
// attachment rows sharing a content_hash count twice, matching what
// WorkspaceStorageUsage's SUM(size_bytes) reports after the copy
// (TestStorageUsage_TracksUploads asserts that double-count is intentional).
// Per DR-16 storage is reported, not enforced — the dry-run's number must
// agree with the storage page, not with a hash-deduped fiction.
func TestPlanAttachmentCopy_DedupedBlobsCountedPerRow(t *testing.T) {
	f := newPlanFixture(t)
	a := f.attachWith(t, f.wsA, "one.png", 500, "fs:samehash", "samehash")
	b := f.attachWith(t, f.wsA, "two.png", 500, "fs:samehash", "samehash")

	plan := f.plan(t, f.req(imageRef(a.ID)+imageRef(b.ID), nil))

	if len(plan.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2 distinct rows", len(plan.Rows))
	}
	if plan.TotalBytes != 1000 {
		t.Errorf("TotalBytes = %d, want 1000 (per row, matching SUM(size_bytes))", plan.TotalBytes)
	}
}

// TestPlanAttachmentCopy_DryRunTotalsMatchTheRealCopy is the acceptance
// criterion that the byte total "matches what the dry-run will display".
// The dry-run and the copy that follows it call the same function with the
// same inputs, so the guarantee is structural — this pins it against a
// future change that special-cases DryRun and lets the two drift.
func TestPlanAttachmentCopy_DryRunTotalsMatchTheRealCopy(t *testing.T) {
	f := newPlanFixture(t)
	orig := f.attach(t, f.wsA, "shot.png", 1000)
	f.variant(t, f.wsA, orig, models.AttachmentVariantThumbSm, 10)
	f.variant(t, f.wsA, orig, models.AttachmentVariantThumbMd, 20)
	extra := f.attach(t, f.wsA, "doc.pdf", 300)
	ghost := newID()

	content := imageRef(orig.ID) + "\n" + fileRef(extra.ID) + "\n" + imageRef(ghost)
	fields := map[string]any{"cover": "pad-attachment:" + orig.ID}

	preview := f.req(content, fields)
	preview.TargetItemID = ""
	preview.DryRun = true
	dry := f.plan(t, preview)

	real := f.plan(t, f.req(content, fields))

	if dry.TotalBytes != real.TotalBytes {
		t.Errorf("dry-run TotalBytes = %d, copy = %d", dry.TotalBytes, real.TotalBytes)
	}
	if dry.TotalBytes != 1330 {
		t.Errorf("TotalBytes = %d, want 1330 (original + two thumbs + the pdf, each once)", dry.TotalBytes)
	}
	if strings.Join(sourceIDs(dry), ",") != strings.Join(sourceIDs(real), ",") {
		t.Errorf("planned rows differ: dry-run %v, copy %v", sourceIDs(dry), sourceIDs(real))
	}
	if strings.Join(dry.UnresolvableRefs, ",") != strings.Join(real.UnresolvableRefs, ",") {
		t.Errorf("unresolvable refs differ: dry-run %v, copy %v", dry.UnresolvableRefs, real.UnresolvableRefs)
	}
	if len(dry.UnresolvableRefs) != 1 {
		t.Errorf("UnresolvableRefs = %v, want the one dangling ref", dry.UnresolvableRefs)
	}

	// The one thing that legitimately differs: the dry-run's rows are not
	// attached to a destination item, because there is not one yet.
	if dry.Rows[0].Attachment.ItemID != nil || real.Rows[0].Attachment.ItemID == nil {
		t.Errorf("item_id: dry-run %v (want nil), copy %v (want set)",
			dry.Rows[0].Attachment.ItemID, real.Rows[0].Attachment.ItemID)
	}
}

// --- determinism + validation ----------------------------------------------

// TestPlanAttachmentCopy_DeterministicOrder: references are planned in
// first-appearance order (content before fields), so a dry-run and the
// subsequent copy list the same rows in the same order.
func TestPlanAttachmentCopy_DeterministicOrder(t *testing.T) {
	f := newPlanFixture(t)
	first := f.attach(t, f.wsA, "first.png", 1)
	second := f.attach(t, f.wsA, "second.png", 2)
	third := f.attach(t, f.wsA, "third.png", 3)

	req := f.req(imageRef(second.ID)+imageRef(first.ID), map[string]any{"cover": "pad-attachment:" + third.ID})
	for i := 0; i < 5; i++ {
		plan := f.plan(t, req)
		got := sourceIDs(plan)
		want := []string{second.ID, first.ID, third.ID}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("iteration %d: order = %v, want %v", i, got, want)
		}
	}
}

// TestPlanAttachmentCopy_DoesNotMutateCallerFields: the planner reads the
// caller's final fields, it does not own them. The orchestration writes
// that same map to the destination item after planning, so a mutation
// here would corrupt the copy.
func TestPlanAttachmentCopy_DoesNotMutateCallerFields(t *testing.T) {
	f := newPlanFixture(t)
	a := f.attach(t, f.wsA, "shot.png", 5)

	fields := map[string]any{
		"cover":  "pad-attachment:" + a.ID,
		"status": "open",
		"tags":   []any{"one", "two"},
	}
	before, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	f.plan(t, f.req("body", fields))

	after, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("planner mutated caller fields:\n before %s\n after  %s", before, after)
	}
}

// TestPlanAttachmentCopy_NoRefs: nothing referenced, nothing planned, and
// a non-nil map so callers can index it unconditionally.
func TestPlanAttachmentCopy_NoRefs(t *testing.T) {
	f := newPlanFixture(t)

	plan := f.plan(t, f.req("just prose", map[string]any{"status": "open"}))

	if len(plan.Rows) != 0 || plan.TotalBytes != 0 || len(plan.UnresolvableRefs) != 0 {
		t.Errorf("empty plan expected, got %+v", plan)
	}
	if plan.IDMap == nil {
		t.Error("IDMap is nil; callers must be able to index it unconditionally")
	}
}

// TestPlanAttachmentCopy_RequiredInputs: the three identifiers that make
// the plan safe are mandatory. An empty SourceWorkspaceID in particular
// would turn the DR-11a scope into "any workspace".
func TestPlanAttachmentCopy_RequiredInputs(t *testing.T) {
	f := newPlanFixture(t)

	for _, tc := range []struct {
		name   string
		mutate func(*AttachmentCopyRequest)
		want   string
	}{
		{"no source workspace", func(r *AttachmentCopyRequest) { r.SourceWorkspaceID = "" }, "source_workspace_id"},
		{"no target workspace", func(r *AttachmentCopyRequest) { r.TargetWorkspaceID = "" }, "target_workspace_id"},
		{"no actor", func(r *AttachmentCopyRequest) { r.UploadedBy = "" }, "uploaded_by"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := f.req("body", nil)
			tc.mutate(&req)
			_, err := f.s.PlanAttachmentCopy(req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %s", err, tc.want)
			}
		})
	}
}

// TestChunkStrings covers the split at and around the boundary. The
// integration test below cannot pin these cases cheaply (each id costs a
// row), and an off-by-one here silently drops a whole chunk of references
// — which the planner would report as "not referenced" rather than as an
// error.
func TestChunkStrings(t *testing.T) {
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("id-%d", i)
		}
		return out
	}
	sizes := func(chunks [][]string) []int {
		out := make([]int, len(chunks))
		for i, c := range chunks {
			out[i] = len(c)
		}
		return out
	}

	for _, tc := range []struct {
		n    int
		size int
		want []int
	}{
		{0, 400, nil},
		{1, 400, []int{1}},
		{399, 400, []int{399}},
		{400, 400, []int{400}},
		{401, 400, []int{400, 1}},
		{800, 400, []int{400, 400}},
		{801, 400, []int{400, 400, 1}},
		{5, 2, []int{2, 2, 1}},
	} {
		t.Run(fmt.Sprintf("n=%d/size=%d", tc.n, tc.size), func(t *testing.T) {
			in := ids(tc.n)
			chunks := chunkStrings(in, tc.size)
			if fmt.Sprint(sizes(chunks)) != fmt.Sprint(tc.want) {
				t.Fatalf("chunk sizes = %v, want %v", sizes(chunks), tc.want)
			}
			var flat []string
			for _, c := range chunks {
				flat = append(flat, c...)
			}
			if strings.Join(flat, ",") != strings.Join(in, ",") {
				t.Errorf("chunks do not reassemble to the input")
			}
		})
	}
}

// TestPlanAttachmentCopy_ManyRefsChunked drives real references through
// the multi-chunk path end to end: every reference must still resolve,
// exactly once, with the bytes counted once. It complements TestChunkStrings
// — that one pins the split arithmetic, this one pins that a plan built
// from more than one query is complete.
func TestPlanAttachmentCopy_ManyRefsChunked(t *testing.T) {
	if testing.Short() {
		t.Skip("creates attachmentPlanChunk+1 rows")
	}
	f := newPlanFixture(t)

	var body strings.Builder
	want := make([]string, 0, attachmentPlanChunk+1)
	for i := 0; i <= attachmentPlanChunk; i++ {
		a := f.attach(t, f.wsA, fmt.Sprintf("shot-%d.png", i), 1)
		want = append(want, a.ID)
		body.WriteString(imageRef(a.ID))
	}

	plan := f.plan(t, f.req(body.String(), nil))

	assertSourceSet(t, plan, want...)
	if plan.TotalBytes != int64(len(want)) {
		t.Errorf("TotalBytes = %d, want %d", plan.TotalBytes, len(want))
	}
}

// --- the caller's authorization hook (TASK-2408) ---------------------------
//
// DR-11a's scope is the workspace; the workspace is not the caller. The
// planner therefore consults an AttachmentAuthorizer supplied by the
// server, and a denial must be INDISTINGUISHABLE from a row that was never
// there — same bucket, same counts — because the preflight publishes those
// counts to anyone who can edit the source item.

// TestPlanAttachmentCopy_DeniedRefIsExactlyDangling is the anti-oracle
// claim at the planner's own level: the plan produced for a live-but-denied
// reference equals, field for field, the plan produced for a UUID that
// names nothing at all.
func TestPlanAttachmentCopy_DeniedRefIsExactlyDangling(t *testing.T) {
	f := newPlanFixture(t)
	secret := f.attach(t, f.wsA, "secret.png", 4096)
	f.variant(t, f.wsA, secret, models.AttachmentVariantThumbMd, 20)

	denied := f.plan(t, withAuthorizer(f.req(imageRef(secret.ID), nil), denyAll))

	missing := newID()
	dangling := f.plan(t, f.req(imageRef(missing), nil))

	if len(denied.Rows) != 0 || denied.TotalBytes != 0 {
		t.Fatalf("a denied reference was planned: %d rows, %d bytes", len(denied.Rows), denied.TotalBytes)
	}
	if len(denied.IDMap) != 0 {
		t.Errorf("IDMap = %v, want empty — a denied reference must not be rewritten", denied.IDMap)
	}
	if len(denied.UnresolvableRefs) != 1 || denied.UnresolvableRefs[0] != secret.ID {
		t.Errorf("UnresolvableRefs = %v, want exactly the denied ref", denied.UnresolvableRefs)
	}
	if len(dangling.UnresolvableRefs) != len(denied.UnresolvableRefs) ||
		len(dangling.Rows) != len(denied.Rows) ||
		dangling.TotalBytes != denied.TotalBytes ||
		dangling.CrossBackend != denied.CrossBackend {
		t.Errorf("a denied reference is distinguishable from a dangling one:\n denied:   %+v\n dangling: %+v",
			denied, dangling)
	}
}

// TestPlanAttachmentCopy_DeniedParentSinksTheVariantRef is the one-level-down
// case, mirroring TestPlanAttachmentCopy_RefToVariantWithForeignParent: a
// reference naming a VARIANT whose original the caller may not see cannot
// smuggle that original in as its clone root.
func TestPlanAttachmentCopy_DeniedParentSinksTheVariantRef(t *testing.T) {
	f := newPlanFixture(t)
	orig := f.attach(t, f.wsA, "shot.png", 1000)
	thumb := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbSm, 10)

	// Everything is visible EXCEPT the original.
	req := withAuthorizer(f.req(imageRef(thumb.ID), nil), func(_ Queryer, a models.Attachment) (bool, error) {
		return a.ID != orig.ID, nil
	})
	plan := f.plan(t, req)

	if len(plan.Rows) != 0 {
		t.Fatalf("planned %v; a variant whose parent is denied must clone nothing", sourceIDs(plan))
	}
	if len(plan.UnresolvableRefs) != 1 || plan.UnresolvableRefs[0] != thumb.ID {
		t.Errorf("UnresolvableRefs = %v, want the variant reference", plan.UnresolvableRefs)
	}
}

// TestPlanAttachmentCopy_DeniedVariantDropsOnlyItself is the other
// direction, and the reason variants are authorized individually rather
// than inherited from an authorized root: a derived row that (through the
// malformed item_id data PLAN-2397 repairs) belongs somewhere the caller
// cannot see drops out on its own, without taking the legitimate original
// with it.
func TestPlanAttachmentCopy_DeniedVariantDropsOnlyItself(t *testing.T) {
	f := newPlanFixture(t)
	orig := f.attach(t, f.wsA, "shot.png", 1000)
	sm := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbSm, 10)
	md := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbMd, 20)

	req := withAuthorizer(f.req(imageRef(orig.ID), nil), func(_ Queryer, a models.Attachment) (bool, error) {
		return a.ID != md.ID, nil
	})
	plan := f.plan(t, req)

	assertSourceSet(t, plan, orig.ID, sm.ID)
	if plan.TotalBytes != 1010 {
		t.Errorf("TotalBytes = %d, want 1010 — the denied variant's bytes must not be counted", plan.TotalBytes)
	}
	if len(plan.UnresolvableRefs) != 0 {
		t.Errorf("UnresolvableRefs = %v, want none — the REFERENCE resolved fine", plan.UnresolvableRefs)
	}
}

// TestPlanAttachmentCopy_AuthorizerErrorIsFatal: a failed membership lookup
// must not read as "you may not see this". Silently denying would turn a
// transient database error into a copy that quietly drops the user's
// images.
func TestPlanAttachmentCopy_AuthorizerErrorIsFatal(t *testing.T) {
	f := newPlanFixture(t)
	a := f.attach(t, f.wsA, "shot.png", 10)

	req := withAuthorizer(f.req(imageRef(a.ID), nil), func(Queryer, models.Attachment) (bool, error) {
		return false, fmt.Errorf("membership lookup exploded")
	})
	if _, err := f.s.PlanAttachmentCopy(req); err == nil {
		t.Fatal("PlanAttachmentCopy swallowed the authorizer's error")
	}
}

// TestPlanAttachmentCopy_AuthorizerSeesEveryClonedRow pins the coverage
// claim itself: every row the plan emits was offered to the authorizer.
// The gate is only worth what it is asked about.
func TestPlanAttachmentCopy_AuthorizerSeesEveryClonedRow(t *testing.T) {
	f := newPlanFixture(t)
	orig := f.attach(t, f.wsA, "shot.png", 1000)
	sm := f.variant(t, f.wsA, orig, models.AttachmentVariantThumbSm, 10)
	other := f.attach(t, f.wsA, "spec.pdf", 50)

	seen := map[string]bool{}
	req := withAuthorizer(f.req(imageRef(sm.ID)+fileRef(other.ID), nil), func(_ Queryer, a models.Attachment) (bool, error) {
		seen[a.ID] = true
		return true, nil
	})
	plan := f.plan(t, req)

	for _, row := range plan.Rows {
		if !seen[row.SourceID] {
			t.Errorf("row %s was cloned without ever being authorized", row.SourceID)
		}
	}
	// Including the PARENT pulled in by the variant reference, which is not
	// itself a reference the caller wrote.
	if !seen[orig.ID] {
		t.Error("the variant's parent was adopted as a clone root without being authorized")
	}
}

func withAuthorizer(req AttachmentCopyRequest, auth AttachmentAuthorizer) AttachmentCopyRequest {
	req.Authorize = auth
	return req
}

func denyAll(Queryer, models.Attachment) (bool, error) { return false, nil }
