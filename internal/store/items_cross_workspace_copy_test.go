package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for CopyItemAcrossWorkspaces — PLAN-2357 / TASK-2363.
//
// The concurrency and lock-ordering tests live at the bottom and are
// Postgres-only by construction (see requirePostgresForConcurrency).

// copyFixture is workspace A (source), workspace B (destination), and a third
// workspace C used for the confused-deputy negative cases.
type copyFixture struct {
	s     *Store
	wsA   *models.Workspace
	wsB   *models.Workspace
	wsC   *models.Workspace
	colA  *models.Collection
	colB  *models.Collection
	colC  *models.Collection
	actor string
}

func newCopyFixture(t *testing.T) copyFixture {
	t.Helper()
	s := testStore(t)
	f := copyFixture{s: s, actor: "actor-user"}
	f.wsA = createTestWorkspace(t, s, "Copy Source")
	f.wsB = createTestWorkspace(t, s, "Copy Dest")
	f.wsC = createTestWorkspace(t, s, "Copy Third Party")
	f.colA = createTestCollection(t, s, f.wsA.ID, "Tasks A")
	f.colB = createTestCollection(t, s, f.wsB.ID, "Tasks B")
	f.colC = createTestCollection(t, s, f.wsC.ID, "Tasks C")
	return f
}

func (f copyFixture) req() CrossWorkspaceCopyRequest {
	return CrossWorkspaceCopyRequest{
		TargetWorkspaceID:  f.wsB.ID,
		TargetCollectionID: f.colB.ID,
		Actor:              f.actor,
	}
}

func (f copyFixture) copy(t *testing.T, req CrossWorkspaceCopyRequest) *CrossWorkspaceCopyResult {
	t.Helper()
	res, err := f.s.CopyItemAcrossWorkspaces(req)
	if err != nil {
		t.Fatalf("CopyItemAcrossWorkspaces: %v", err)
	}
	return res
}

// attachIn creates an original attachment in the given workspace.
func (f copyFixture) attachIn(t *testing.T, workspaceID, filename string, size int64) *models.Attachment {
	t.Helper()
	w, h := 640, 480
	a := &models.Attachment{
		WorkspaceID: workspaceID,
		UploadedBy:  "source-uploader",
		StorageKey:  "fs:" + newID(),
		ContentHash: newID(),
		MimeType:    "image/png",
		SizeBytes:   size,
		Filename:    filename,
		Width:       &w,
		Height:      &h,
	}
	if err := f.s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment(%s): %v", filename, err)
	}
	return a
}

func (f copyFixture) variantOf(t *testing.T, workspaceID string, parent *models.Attachment, kind string, size int64) *models.Attachment {
	t.Helper()
	pid, v := parent.ID, kind
	a := &models.Attachment{
		WorkspaceID: workspaceID,
		UploadedBy:  "source-uploader",
		StorageKey:  "fs:" + newID(),
		ContentHash: newID(),
		MimeType:    "image/webp",
		SizeBytes:   size,
		Filename:    kind + "-" + parent.Filename,
		ParentID:    &pid,
		Variant:     &v,
	}
	if err := f.s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment(variant): %v", err)
	}
	return a
}

func countItemsIn(t *testing.T, s *Store, workspaceID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`), workspaceID).Scan(&n); err != nil {
		t.Fatalf("count items in %s: %v", workspaceID, err)
	}
	return n
}

func countAttachmentsIn(t *testing.T, s *Store, workspaceID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM attachments WHERE workspace_id = ?`), workspaceID).Scan(&n); err != nil {
		t.Fatalf("count attachments in %s: %v", workspaceID, err)
	}
	return n
}

// attachmentsIn returns every attachment row in a workspace (including
// soft-deleted ones, of which the copy path creates none).
func attachmentsIn(t *testing.T, s *Store, workspaceID string) []models.Attachment {
	t.Helper()
	var out []models.Attachment
	if err := s.scanAttachmentsInto(
		`SELECT `+attachmentColumns+` FROM attachments WHERE workspace_id = ? ORDER BY created_at, id`,
		[]any{workspaceID},
		func(a models.Attachment) { out = append(out, a) },
	); err != nil {
		t.Fatalf("list attachments in %s: %v", workspaceID, err)
	}
	return out
}

func countMoveRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_workspace_moves`)).Scan(&n); err != nil {
		t.Fatalf("count item_workspace_moves: %v", err)
	}
	return n
}

func maxSeq(t *testing.T, s *Store, workspaceID string) int64 {
	t.Helper()
	seq, err := s.MaxItemSeq(workspaceID)
	if err != nil {
		t.Fatalf("MaxItemSeq(%s): %v", workspaceID, err)
	}
	return seq
}

// --- Happy path -------------------------------------------------------------

func TestCopyItemAcrossWorkspaces_PlainCopyLandsInDestination(t *testing.T) {
	f := newCopyFixture(t)
	parent := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Parent", "")
	src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:    "Ship the thing",
		Content:  "body with [[Parent]] link",
		Fields:   `{"status":"done"}`,
		Tags:     `["alpha","beta"]`,
		ParentID: &parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	req := f.req()
	req.SourceItemID = src.ID
	res := f.copy(t, req)

	if res.Item.WorkspaceID != f.wsB.ID {
		t.Errorf("copy landed in %s, want %s", res.Item.WorkspaceID, f.wsB.ID)
	}
	if res.Item.CollectionID != f.colB.ID {
		t.Errorf("copy collection = %s, want %s", res.Item.CollectionID, f.colB.ID)
	}
	if res.Item.Title != src.Title {
		t.Errorf("title = %q, want %q", res.Item.Title, src.Title)
	}
	if res.Item.Content != src.Content {
		t.Errorf("content = %q, want %q", res.Item.Content, src.Content)
	}
	// DR-17: tags carry. Compared semantically because Postgres stores tags as
	// jsonb and hands the array back re-serialized with spaces, while SQLite
	// round-trips the literal TEXT.
	var gotTags, wantTags []string
	if err := json.Unmarshal([]byte(res.Item.Tags), &gotTags); err != nil {
		t.Fatalf("decode copied tags %q: %v", res.Item.Tags, err)
	}
	if err := json.Unmarshal([]byte(src.Tags), &wantTags); err != nil {
		t.Fatalf("decode source tags %q: %v", src.Tags, err)
	}
	if strings.Join(gotTags, ",") != strings.Join(wantTags, ",") {
		t.Errorf("tags = %v, want the source's tags %v", gotTags, wantTags)
	}
	if len(gotTags) != 2 {
		t.Errorf("tags = %v, want two entries", gotTags)
	}
	// DR-17: the copy is unparented.
	if res.Item.ParentID != nil {
		t.Errorf("copy has parent %v, want nil (DR-17 scrubs ParentID)", *res.Item.ParentID)
	}
	links, err := f.s.GetItemLinks(res.Item.ID)
	if err != nil {
		t.Fatalf("GetItemLinks: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("copy has %d links, want 0 (DR-17: item_links do not carry)", len(links))
	}
	// The source is untouched.
	stillThere, err := f.s.GetItem(src.ID)
	if err != nil || stillThere == nil {
		t.Fatalf("source item gone after a plain copy: %v", err)
	}
	if stillThere.Seq != src.Seq {
		t.Errorf("plain copy bumped the source's seq: %d -> %d", src.Seq, stillThere.Seq)
	}
	// Provenance: a copy, not a move.
	if res.Move == nil {
		t.Fatal("no provenance row recorded")
	}
	if res.Move.ArchivedSource {
		t.Error("provenance says archived_source=true for a plain copy")
	}
	if res.Move.SourceSeq != nil {
		t.Errorf("plain copy recorded source_seq=%d, want nil", *res.Move.SourceSeq)
	}
	back, err := f.s.GetItemWorkspaceMoveByTarget(res.Item.ID)
	if err != nil || back == nil {
		t.Fatalf("back-pointer lookup failed: %v", err)
	}
	if back.SourceItemID != src.ID {
		t.Errorf("back-pointer source = %s, want %s", back.SourceItemID, src.ID)
	}
}

// DR-9a parity: the copy is a REAL create, not a bare items INSERT. Version
// row, wiki-link index, status transition, slug, item_number and seq must all
// exist in the destination.
func TestCopyItemAcrossWorkspaces_CreationParity(t *testing.T) {
	f := newCopyFixture(t)
	// A destination item the copied body's [[...]] link resolves to.
	createTestItem(t, f.s, f.wsB.ID, f.colB.ID, "Target Doc", "")
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Parity", "see [[Target Doc]]")

	req := f.req()
	req.SourceItemID = src.ID
	res := f.copy(t, req)

	if res.Item.Slug == "" {
		t.Error("copy has no slug")
	}
	if res.Item.ItemNumber == nil || *res.Item.ItemNumber == 0 {
		t.Error("copy has no item_number")
	}
	if res.Item.Seq == 0 {
		t.Error("copy has no seq")
	}

	for _, probe := range []struct {
		name  string
		query string
	}{
		{"item_versions", `SELECT COUNT(*) FROM item_versions WHERE item_id = ?`},
		{"item_wiki_links", `SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ?`},
		{"status_transitions", `SELECT COUNT(*) FROM status_transitions WHERE item_id = ?`},
	} {
		var n int
		if err := f.s.db.QueryRow(f.s.q(probe.query), res.Item.ID).Scan(&n); err != nil {
			t.Fatalf("%s probe: %v", probe.name, err)
		}
		if n == 0 {
			t.Errorf("%s: 0 rows for the copied item, want >=1 (DR-9a creation parity)", probe.name)
		}
	}
}

// --- Seq (DR-14) ------------------------------------------------------------

// A plain copy must not advance workspace A's cursor AT ALL — A's watchers
// have nothing to see, and a spurious bump would make them re-fetch an
// unchanged item.
func TestCopyItemAcrossWorkspaces_PlainCopyDoesNotAdvanceSourceSeq(t *testing.T) {
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Untouched", "body")

	cursorA := maxSeq(t, f.s, f.wsA.ID)
	cursorB := maxSeq(t, f.s, f.wsB.ID)

	req := f.req()
	req.SourceItemID = src.ID
	res := f.copy(t, req)

	if got := maxSeq(t, f.s, f.wsA.ID); got != cursorA {
		t.Errorf("plain copy advanced workspace A's seq %d -> %d, want unchanged", cursorA, got)
	}
	changesA, err := f.s.ListItemsChangesSince(f.wsA.ID, ItemChangesParams{Since: cursorA})
	if err != nil {
		t.Fatalf("ListItemsChangesSince(A): %v", err)
	}
	if len(changesA) != 0 {
		t.Errorf("plain copy produced %d delta rows in A, want 0", len(changesA))
	}

	changesB, err := f.s.ListItemsChangesSince(f.wsB.ID, ItemChangesParams{Since: cursorB})
	if err != nil {
		t.Fatalf("ListItemsChangesSince(B): %v", err)
	}
	var sawCreate bool
	for _, it := range changesB {
		if it.ID == res.Item.ID && it.DeletedAt == nil {
			sawCreate = true
		}
	}
	if !sawCreate {
		t.Error("workspace B's delta from the pre-copy cursor does not contain the create")
	}
}

// On a MOVE, A must advance so its clients receive the tombstone — otherwise
// they keep rendering a source item that no longer exists.
func TestCopyItemAcrossWorkspaces_MoveEmitsTombstoneInSourceWorkspace(t *testing.T) {
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Moving out", "body")

	cursorA := maxSeq(t, f.s, f.wsA.ID)
	cursorB := maxSeq(t, f.s, f.wsB.ID)

	req := f.req()
	req.SourceItemID = src.ID
	req.ArchiveSource = true
	res := f.copy(t, req)

	if got := maxSeq(t, f.s, f.wsA.ID); got <= cursorA {
		t.Errorf("move did not advance workspace A's seq (%d -> %d)", cursorA, got)
	}
	changesA, err := f.s.ListItemsChangesSince(f.wsA.ID, ItemChangesParams{Since: cursorA})
	if err != nil {
		t.Fatalf("ListItemsChangesSince(A): %v", err)
	}
	var sawTombstone bool
	for _, it := range changesA {
		if it.ID == src.ID && it.DeletedAt != nil {
			sawTombstone = true
		}
	}
	if !sawTombstone {
		t.Error("workspace A's delta from the pre-copy cursor does not contain the tombstone")
	}
	if maxSeq(t, f.s, f.wsB.ID) <= cursorB {
		t.Error("move did not advance workspace B's seq")
	}

	// The source is archived, and the provenance row carries the seq that
	// archive assigned (DR-2a: without it two moves in one second are
	// unorderable).
	if live, _ := f.s.GetItem(src.ID); live != nil {
		t.Error("source is still live after a move")
	}
	if res.SourceSeq == nil {
		t.Fatal("move recorded no source_seq")
	}
	if !res.Move.ArchivedSource {
		t.Error("provenance says archived_source=false for a move")
	}
	archived, err := f.s.GetItemIncludeDeleted(src.ID)
	if err != nil {
		t.Fatalf("GetItemIncludeDeleted: %v", err)
	}
	if archived.Seq != *res.SourceSeq {
		t.Errorf("provenance source_seq=%d but the archived row carries seq=%d", *res.SourceSeq, archived.Seq)
	}
}

// --- Fields (DR-12) ---------------------------------------------------------

// MigrateFields computes result.Errors BEFORE any override exists, so testing
// those stale errors rejects a copy whose override already supplied the
// missing required field. Overrides must be applied first, then validated.
func TestCopyItemAcrossWorkspaces_OverrideSatisfiesRequiredField(t *testing.T) {
	f := newCopyFixture(t)
	// A destination collection with a required field the source does not have.
	dest, err := f.s.CreateCollection(f.wsB.ID, models.CollectionCreate{
		Name:   "Strict",
		Schema: `{"fields":[{"key":"severity","label":"Severity","type":"select","options":["low","high"],"required":true}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Needs severity", "")

	req := f.req()
	req.SourceItemID = src.ID
	req.TargetCollectionID = dest.ID

	// Without the override the copy is refused …
	if _, err := f.s.CopyItemAcrossWorkspaces(req); err == nil {
		t.Fatal("copy succeeded with a required field unset, want a validation error")
	} else {
		var verr *FieldValidationError
		if !errors.As(err, &verr) {
			t.Errorf("error = %v, want a *FieldValidationError", err)
		}
	}

	// … and WITH it, the copy lands. This is the assertion that fails if the
	// stale MigrateFields errors are consulted after overrides merge in.
	req.FieldOverrides = map[string]any{"severity": "high"}
	res := f.copy(t, req)
	var fields map[string]any
	if err := json.Unmarshal([]byte(res.Item.Fields), &fields); err != nil {
		t.Fatalf("decode destination fields: %v", err)
	}
	if fields["severity"] != "high" {
		t.Errorf("severity = %v, want \"high\"", fields["severity"])
	}
}

// The mirror image: an override with a value the destination schema rejects
// must be type-checked. Pre-DR-12 the override was never validated at all.
func TestCopyItemAcrossWorkspaces_InvalidOverrideRejected(t *testing.T) {
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Bad override", "")

	req := f.req()
	req.SourceItemID = src.ID
	req.FieldOverrides = map[string]any{"status": "not-an-option"}

	_, err := f.s.CopyItemAcrossWorkspaces(req)
	if err == nil {
		t.Fatal("copy accepted an override outside the select's options")
	}
	var verr *FieldValidationError
	if !errors.As(err, &verr) {
		t.Errorf("error = %v, want a *FieldValidationError", err)
	}
	if countItemsIn(t, f.s, f.wsB.ID) != 0 {
		t.Error("a rejected copy left an item in the destination")
	}
}

func TestCopyItemAcrossWorkspaces_DroppedFieldsReported(t *testing.T) {
	f := newCopyFixture(t)
	dest, err := f.s.CreateCollection(f.wsB.ID, models.CollectionCreate{
		Name:   "Narrow",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:  "Extra fields",
		Fields: `{"status":"open","nowhere_to_go":"x"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	req := f.req()
	req.SourceItemID = src.ID
	req.TargetCollectionID = dest.ID
	res := f.copy(t, req)

	if len(res.DroppedFields) != 1 || res.DroppedFields[0] != "nowhere_to_go" {
		t.Errorf("DroppedFields = %v, want [nowhere_to_go]", res.DroppedFields)
	}
}

// --- Assignment scrubs (DR-8) -----------------------------------------------

func TestCopyItemAcrossWorkspaces_AssigneeCarriesOnlyForDestinationMembers(t *testing.T) {
	f := newCopyFixture(t)
	member := createTestUser(t, f.s, "member@example.com", "Member", "s3cret")
	stranger := createTestUser(t, f.s, "stranger@example.com", "Stranger", "s3cret")
	for _, u := range []*models.User{member, stranger} {
		if err := f.s.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember(A): %v", err)
		}
	}
	if err := f.s.AddWorkspaceMember(f.wsB.ID, member.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember(B): %v", err)
	}

	role, err := f.s.CreateAgentRole(f.wsA.ID, models.AgentRoleCreate{Name: "Builder"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}

	carried, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:          "Assigned to a B member",
		AssignedUserID: &member.ID,
		AgentRoleID:    &role.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	req := f.req()
	req.SourceItemID = carried.ID
	res := f.copy(t, req)
	if res.Item.AssignedUserID == nil || *res.Item.AssignedUserID != member.ID {
		t.Errorf("assignee did not carry for a destination member: %v", res.Item.AssignedUserID)
	}
	if res.DroppedAssignee {
		t.Error("DroppedAssignee=true for an assignee that carried")
	}
	// Agent role ALWAYS clears — role slugs are workspace-local.
	if res.Item.AgentRoleID != nil {
		t.Errorf("agent role carried (%v), want cleared", *res.Item.AgentRoleID)
	}
	if !res.DroppedAgentRole {
		t.Error("DroppedAgentRole=false but the source had an agent role")
	}

	dropped, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:          "Assigned to a non-member",
		AssignedUserID: &stranger.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	req2 := f.req()
	req2.SourceItemID = dropped.ID
	res2 := f.copy(t, req2)
	if res2.Item.AssignedUserID != nil {
		t.Errorf("assignee carried for a non-member of B: %v", *res2.Item.AssignedUserID)
	}
	if !res2.DroppedAssignee {
		t.Error("DroppedAssignee=false for an assignee that was cleared")
	}
}

// --- Attachments (DR-11 / DR-11a) -------------------------------------------

func TestCopyItemAcrossWorkspaces_AttachmentsClonedAndRefsRewritten(t *testing.T) {
	f := newCopyFixture(t)
	orig := f.attachIn(t, f.wsA.ID, "diagram.png", 4096)
	thumb := f.variantOf(t, f.wsA.ID, orig, "thumb-md", 512)

	// Both collections declare `note` so a field-borne reference actually
	// SURVIVES migration — with the default fixture schemas `note` is an
	// unknown key that MigrateFields drops, and the fields rewrite would go
	// untested.
	withNote := `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"},{"key":"note","label":"Note","type":"text"}]}`
	srcColl, err := f.s.CreateCollection(f.wsA.ID, models.CollectionCreate{Name: "Noted A", Schema: withNote})
	if err != nil {
		t.Fatalf("CreateCollection(A): %v", err)
	}
	dstColl, err := f.s.CreateCollection(f.wsB.ID, models.CollectionCreate{Name: "Noted B", Schema: withNote})
	if err != nil {
		t.Fatalf("CreateCollection(B): %v", err)
	}

	// A reference in the body, one inside a fenced code block (the rewriter
	// is a plain ReplaceAll, so a fenced ref gets rewritten either way and
	// must therefore be cloned either way), and one in the fields.
	content := fmt.Sprintf("![d](pad-attachment:%s)\n\n```\nsee pad-attachment:%s\n```\n", orig.ID, thumb.ID)
	src, err := f.s.CreateItem(f.wsA.ID, srcColl.ID, models.ItemCreate{
		Title:   "Has attachments",
		Content: content,
		Fields:  fmt.Sprintf(`{"status":"open","note":"pad-attachment:%s"}`, orig.ID),
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	req := f.req()
	req.SourceItemID = src.ID
	req.TargetCollectionID = dstColl.ID
	res := f.copy(t, req)

	// The field-borne reference survived migration — otherwise the assertions
	// below about the fields rewrite would be vacuous.
	if !strings.Contains(res.Item.Fields, "pad-attachment:") {
		t.Fatal("the destination fields carry no attachment reference; the fields-rewrite assertions would be vacuous")
	}

	if res.AttachmentsCopied != 2 {
		t.Errorf("AttachmentsCopied = %d, want 2 (original + variant)", res.AttachmentsCopied)
	}
	if res.BytesCopied != 4096+512 {
		t.Errorf("BytesCopied = %d, want %d", res.BytesCopied, 4096+512)
	}

	// No workspace-A UUID survives the rewrite, anywhere — body or fields,
	// fenced or not. A surviving ref renders broken AND 403s on download.
	for _, oldID := range []string{orig.ID, thumb.ID} {
		if strings.Contains(res.Item.Content, oldID) {
			t.Errorf("copied content still references workspace-A attachment %s", oldID)
		}
		if strings.Contains(res.Item.Fields, oldID) {
			t.Errorf("copied fields still reference workspace-A attachment %s", oldID)
		}
	}

	rows := attachmentsIn(t, f.s, f.wsB.ID)
	if len(rows) != 2 {
		t.Fatalf("workspace B has %d attachment rows, want 2", len(rows))
	}
	var newOriginal *models.Attachment
	for i := range rows {
		a := rows[i]
		// DR-11: item_id is set from the outset, never transiently NULL.
		if a.ItemID == nil || *a.ItemID != res.Item.ID {
			t.Errorf("clone %s has item_id %v, want %s", a.ID, a.ItemID, res.Item.ID)
		}
		// DR-11: uploaded_by is the ACTOR, not the source uploader.
		if a.UploadedBy != f.actor {
			t.Errorf("clone %s uploaded_by = %q, want the actor %q", a.ID, a.UploadedBy, f.actor)
		}
		if a.CreatedAt.IsZero() {
			t.Errorf("clone %s has a zero created_at", a.ID)
		}
		if a.WorkspaceID != f.wsB.ID {
			t.Errorf("clone %s landed in %s", a.ID, a.WorkspaceID)
		}
		if a.ParentID == nil {
			cp := a
			newOriginal = &cp
		}
	}
	if newOriginal == nil {
		t.Fatal("no cloned original in workspace B")
	}
	for i := range rows {
		a := rows[i]
		if a.ParentID == nil {
			continue
		}
		// The variant's parent_id is remapped to the NEW original, not
		// left pointing into workspace A.
		if *a.ParentID != newOriginal.ID {
			t.Errorf("cloned variant parent_id = %s, want the new original %s", *a.ParentID, newOriginal.ID)
		}
	}

	// The rewritten refs actually resolve in B.
	for _, id := range []string{newOriginal.ID} {
		if !strings.Contains(res.Item.Content, "pad-attachment:"+id) {
			t.Errorf("copied content does not reference the new original %s", id)
		}
	}

	// Workspace A keeps its rows, untouched.
	if countAttachmentsIn(t, f.s, f.wsA.ID) != 2 {
		t.Error("the copy disturbed workspace A's attachment rows")
	}
}

// DR-11a: refs that resolve to nothing under `workspace_id = A AND deleted_at
// IS NULL` are never cloned, the literal text survives, and the copy is not
// blocked. The foreign-workspace case is the confused-deputy hole.
func TestCopyItemAcrossWorkspaces_UnresolvableRefsAreNeverCloned(t *testing.T) {
	f := newCopyFixture(t)
	foreign := f.attachIn(t, f.wsC.ID, "someone-elses.png", 9999)
	dangling := newID()

	content := fmt.Sprintf("![a](pad-attachment:%s) ![b](pad-attachment:%s)", foreign.ID, dangling)
	src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:   "Bad refs",
		Content: content,
		Fields:  `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	req := f.req()
	req.SourceItemID = src.ID
	res := f.copy(t, req)

	if res.AttachmentsCopied != 0 {
		t.Errorf("cloned %d attachments from unresolvable refs, want 0", res.AttachmentsCopied)
	}
	if countAttachmentsIn(t, f.s, f.wsB.ID) != 0 {
		t.Error("a foreign-workspace attachment was cloned into the destination")
	}
	if len(res.UnresolvableRefs) != 2 {
		t.Errorf("UnresolvableRefs = %v, want both refs", res.UnresolvableRefs)
	}
	// The copy renders exactly as broken as the source did.
	if res.Item.Content != content {
		t.Errorf("content = %q, want the source text preserved verbatim", res.Item.Content)
	}
}

// v1 refuses a copy whose bytes live in a backend the destination does not
// write to, rather than silently inserting a row that 404s on download.
func TestCopyItemAcrossWorkspaces_CrossBackendRefused(t *testing.T) {
	f := newCopyFixture(t)
	orig := f.attachIn(t, f.wsA.ID, "on-disk.png", 100) // storage_key is "fs:…"
	src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:   "Cross backend",
		Content: fmt.Sprintf("![x](pad-attachment:%s)", orig.ID),
		Fields:  `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	req := f.req()
	req.SourceItemID = src.ID
	req.TargetBackend = "s3"

	_, err = f.s.CopyItemAcrossWorkspaces(req)
	if !errors.Is(err, ErrCopyCrossBackendAttachments) {
		t.Fatalf("error = %v, want ErrCopyCrossBackendAttachments", err)
	}
	if countItemsIn(t, f.s, f.wsB.ID) != 0 || countAttachmentsIn(t, f.s, f.wsB.ID) != 0 {
		t.Error("a refused cross-backend copy left rows in the destination")
	}
}

// --- Rollback ---------------------------------------------------------------

// A failure at ANY stage must leave nothing behind in either workspace: no
// item, no attachment rows, no provenance row, and neither seq advanced.
func TestCopyItemAcrossWorkspaces_RollbackIsCompleteAtEveryStage(t *testing.T) {
	stages := []string{copyStageCreateItem, copyStageAttachments, copyStageArchive, copyStageProvenance}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			f := newCopyFixture(t)
			orig := f.attachIn(t, f.wsA.ID, "shot.png", 128)
			f.variantOf(t, f.wsA.ID, orig, "thumb-sm", 32)
			src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
				Title:   "Rolls back",
				Content: fmt.Sprintf("![x](pad-attachment:%s)", orig.ID),
				Fields:  `{"status":"open"}`,
			})
			if err != nil {
				t.Fatalf("CreateItem: %v", err)
			}

			itemsB := countItemsIn(t, f.s, f.wsB.ID)
			attachB := countAttachmentsIn(t, f.s, f.wsB.ID)
			moves := countMoveRows(t, f.s)
			cursorA := maxSeq(t, f.s, f.wsA.ID)
			cursorB := maxSeq(t, f.s, f.wsB.ID)

			req := f.req()
			req.SourceItemID = src.ID
			req.ArchiveSource = true
			req.failAfterStage = stage

			if _, err := f.s.CopyItemAcrossWorkspaces(req); err == nil {
				t.Fatalf("copy succeeded despite an injected failure after %q", stage)
			}

			if got := countItemsIn(t, f.s, f.wsB.ID); got != itemsB {
				t.Errorf("destination item count %d -> %d after rollback", itemsB, got)
			}
			if got := countAttachmentsIn(t, f.s, f.wsB.ID); got != attachB {
				t.Errorf("destination attachment count %d -> %d after rollback", attachB, got)
			}
			if got := countMoveRows(t, f.s); got != moves {
				t.Errorf("provenance row count %d -> %d after rollback", moves, got)
			}
			if got := maxSeq(t, f.s, f.wsA.ID); got != cursorA {
				t.Errorf("workspace A's seq advanced %d -> %d after rollback", cursorA, got)
			}
			if got := maxSeq(t, f.s, f.wsB.ID); got != cursorB {
				t.Errorf("workspace B's seq advanced %d -> %d after rollback", cursorB, got)
			}
			if live, _ := f.s.GetItem(src.ID); live == nil {
				t.Error("the source was archived despite the rollback")
			}
		})
	}
}

// A cloned attachment whose storage_key is blank must abort the whole copy —
// CreateAttachmentTx refuses it, and that refusal has to unwind the item that
// was already inserted. (A row with no key is a live attachment the registry
// cannot resolve; it fails at download time with nothing to point at.)
func TestCopyItemAcrossWorkspaces_AttachmentInsertFailureRollsBackTheItem(t *testing.T) {
	f := newCopyFixture(t)
	// Insert an attachment with an empty storage_key directly — CreateAttachment
	// refuses it, which is precisely the guard being exercised downstream.
	id := newID()
	if _, err := f.s.db.Exec(f.s.q(`
		INSERT INTO attachments (`+attachmentColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, f.wsA.ID, nil, "uploader", "", newID(), "image/png", 10, "keyless.png",
		nil, nil, nil, nil, now(), nil); err != nil {
		t.Fatalf("insert keyless attachment: %v", err)
	}

	src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:   "Keyless attachment",
		Content: fmt.Sprintf("![x](pad-attachment:%s)", id),
		Fields:  `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	req := f.req()
	req.SourceItemID = src.ID
	if _, err := f.s.CopyItemAcrossWorkspaces(req); err == nil {
		t.Fatal("copy succeeded with a keyless source attachment")
	}
	if countItemsIn(t, f.s, f.wsB.ID) != 0 {
		t.Error("the destination item survived a failed attachment insert")
	}
	if countAttachmentsIn(t, f.s, f.wsB.ID) != 0 {
		t.Error("attachment rows survived a failed attachment insert")
	}
	if countMoveRows(t, f.s) != 0 {
		t.Error("a provenance row survived a failed attachment insert")
	}
}

// --- Scope refusals ---------------------------------------------------------

func TestCopyItemAcrossWorkspaces_TargetCollectionMustBelongToTargetWorkspace(t *testing.T) {
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Scoped", "")

	req := f.req()
	req.SourceItemID = src.ID
	req.TargetCollectionID = f.colC.ID // lives in workspace C, not B

	if _, err := f.s.CopyItemAcrossWorkspaces(req); err == nil {
		t.Fatal("copy accepted a collection from another workspace")
	}
	if countItemsIn(t, f.s, f.wsB.ID) != 0 || countItemsIn(t, f.s, f.wsC.ID) != 0 {
		t.Error("a cross-workspace collection reference produced an item")
	}
}

func TestCopyItemAcrossWorkspaces_ArchivedSourceIsNotCopyable(t *testing.T) {
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Already gone", "")
	if err := f.s.DeleteItem(src.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	req := f.req()
	req.SourceItemID = src.ID
	if _, err := f.s.CopyItemAcrossWorkspaces(req); err == nil {
		t.Fatal("copy accepted an archived source")
	}
}

// --- Quota (DR-16) ----------------------------------------------------------

// quotaFixture wires a free-tier owner with an items_per_workspace override so
// the cap is reachable in a test.
func newQuotaFixture(t *testing.T, limit int) copyFixture {
	t.Helper()
	s := testStore(t)
	owner := createTestUser(t, s, "quota-owner@example.com", "Owner", "s3cret")
	if err := s.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	if err := s.SetUserPlanOverrides(owner.ID, fmt.Sprintf(`{"items_per_workspace": %d}`, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	wsA, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Quota Source", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace(A): %v", err)
	}
	wsB, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Quota Dest", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace(B): %v", err)
	}
	return copyFixture{
		s:     s,
		wsA:   wsA,
		wsB:   wsB,
		colA:  createTestCollection(t, s, wsA.ID, "Tasks A"),
		colB:  createTestCollection(t, s, wsB.ID, "Tasks B"),
		actor: owner.ID,
	}
}

func TestCopyItemAcrossWorkspaces_ItemQuota(t *testing.T) {
	// The destination starts empty and the cap is 2: the first copy is
	// "exactly at limit minus one" and lands; the second fills the cap and
	// lands; the third is one-over and is refused.
	f := newQuotaFixture(t, 2)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Copy me", "")

	for i := 0; i < 2; i++ {
		req := f.req()
		req.SourceItemID = src.ID
		req.EnforceItemLimit = true
		if _, err := f.s.CopyItemAcrossWorkspaces(req); err != nil {
			t.Fatalf("copy %d under the cap failed: %v", i, err)
		}
	}

	req := f.req()
	req.SourceItemID = src.ID
	req.EnforceItemLimit = true
	_, err := f.s.CopyItemAcrossWorkspaces(req)
	if err == nil {
		t.Fatal("copy past the item cap succeeded")
	}
	var limitErr *ItemLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error = %v, want an *ItemLimitError", err)
	}
	if limitErr.Result.Current != 2 || limitErr.Result.Limit != 2 {
		t.Errorf("limit result = %+v, want current=2 limit=2", limitErr.Result)
	}
	if countItemsIn(t, f.s, f.wsB.ID) != 2 {
		t.Errorf("destination has %d items, want 2 (the rejected copy must not land)", countItemsIn(t, f.s, f.wsB.ID))
	}

	// EnforceItemLimit=false is the self-hosted shape: the same copy lands.
	req.EnforceItemLimit = false
	if _, err := f.s.CopyItemAcrossWorkspaces(req); err != nil {
		t.Fatalf("unenforced copy failed: %v", err)
	}
}

// TestCheckLimitTx_SeesUncommittedRowsInTheTransaction is the direct proof
// that the DR-16 quota count runs on the caller's transaction.
//
// The concurrent-copy test below cannot prove this on its own: the destination
// workspace's advisory lock already serializes the two copies, so a pool-based
// CheckLimit would produce the same one-success/one-rejection outcome. This
// one distinguishes them unambiguously — an uncommitted insert is visible to
// CheckLimitTx and invisible to CheckLimit, so swapping the call in the copy
// path back to the pool form makes this test fail.
func TestCheckLimitTx_SeesUncommittedRowsInTheTransaction(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "tx-count@example.com", "Owner", "s3cret")
	if err := s.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	if err := s.SetUserPlanOverrides(owner.ID, `{"items_per_workspace": 1}`); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Tx Count", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col := createTestCollection(t, s, ws.ID, "Tasks")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // test cleanup

	if _, err := s.createItemTx(tx, ws.ID, col.ID, models.ItemCreate{
		Title:  "Uncommitted",
		Fields: `{"status":"open"}`,
	}); err != nil {
		t.Fatalf("createItemTx: %v", err)
	}

	inTx, err := s.CheckLimitTx(tx, ws.ID, "items_per_workspace")
	if err != nil {
		t.Fatalf("CheckLimitTx: %v", err)
	}
	if inTx.Current != 1 {
		t.Errorf("CheckLimitTx Current=%d, want 1 (the uncommitted insert)", inTx.Current)
	}
	if inTx.Allowed {
		t.Error("CheckLimitTx Allowed=true at the cap; the uncommitted row was not counted")
	}
}

// --- Lock-key ordering ------------------------------------------------------

// The pure half of the DR-9 lock contract: ascending order, duplicates
// collapsed. Colliding keys (two workspaces hashing to one value) are ONE lock,
// not two.
func TestSortedDedupedLockKeys(t *testing.T) {
	cases := []struct {
		name string
		in   []int64
		want []int64
	}{
		{"empty", nil, nil},
		{"single", []int64{7}, []int64{7}},
		{"already ordered", []int64{-5, 3}, []int64{-5, 3}},
		{"reversed", []int64{3, -5}, []int64{-5, 3}},
		// hashtext returns a signed int4, so negative keys are ordinary and
		// must sort numerically, not by string.
		{"negatives sort numerically", []int64{-2147483648, 2147483647, 0}, []int64{-2147483648, 0, 2147483647}},
		{"collision collapses", []int64{42, 42}, []int64{42}},
		{"collision among others", []int64{9, 42, 42, 1}, []int64{1, 9, 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sortedDedupedLockKeys(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// --- Collections that vanish under the lock (TASK-2365) ---------------------

// TestCopyAcrossWorkspaces_CollectionMissingSentinels pins the DETECTION half
// of the pre-write rejection the HTTP layer maps to a 404.
//
// Both are reachable through the same narrow window: the caller is authorized
// against a live collection, and it is soft-deleted before this transaction
// re-reads it under the locks. They must come back as the exported sentinels
// rather than anonymous errors, because the HTTP layer's fallback is DR-13's
// "the copy may or may not have landed" 500 — a message that would send the
// user hunting for an item nothing ever tried to create.
//
// A foreign target collection takes the SAME sentinel as an absent one:
// getCollectionInWorkspaceTx is workspace-scoped, and that scope is the
// security boundary which makes "a collection in someone else's workspace" a
// not-found rather than a cross-workspace write.
func TestCopyAcrossWorkspaces_CollectionMissingSentinels(t *testing.T) {
	t.Run("source collection soft-deleted under the lock", func(t *testing.T) {
		f := newCopyFixture(t)
		src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Orphaned", "body")
		if err := f.s.DeleteCollection(f.colA.ID, ""); err != nil {
			t.Fatalf("DeleteCollection: %v", err)
		}
		req := f.req()
		req.SourceItemID = src.ID
		_, err := f.s.CopyItemAcrossWorkspaces(req)
		if !errors.Is(err, ErrCopySourceCollectionMissing) {
			t.Fatalf("err = %v, want ErrCopySourceCollectionMissing", err)
		}
	})

	t.Run("target collection soft-deleted under the lock", func(t *testing.T) {
		f := newCopyFixture(t)
		src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Homeless", "body")
		if err := f.s.DeleteCollection(f.colB.ID, ""); err != nil {
			t.Fatalf("DeleteCollection: %v", err)
		}
		req := f.req()
		req.SourceItemID = src.ID
		_, err := f.s.CopyItemAcrossWorkspaces(req)
		if !errors.Is(err, ErrCopyTargetCollectionMissing) {
			t.Fatalf("err = %v, want ErrCopyTargetCollectionMissing", err)
		}
	})

	t.Run("target collection in another workspace", func(t *testing.T) {
		f := newCopyFixture(t)
		src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Confused Deputy", "body")
		req := f.req()
		req.SourceItemID = src.ID
		req.TargetCollectionID = f.colC.ID // live, but in workspace C
		_, err := f.s.CopyItemAcrossWorkspaces(req)
		if !errors.Is(err, ErrCopyTargetCollectionMissing) {
			t.Fatalf("err = %v, want ErrCopyTargetCollectionMissing", err)
		}
	})

	// All of them are caller-facing rejections, so the copy must NOT log them
	// as incidents alongside genuine deadlocks and DB failures.
	t.Run("classified as expected rejections", func(t *testing.T) {
		for _, err := range []error{ErrCopySourceCollectionMissing, ErrCopyTargetCollectionMissing} {
			if !isExpectedCopyRejection(err) {
				t.Errorf("%v is logged as an incident; it is a 404 the caller renders", err)
			}
		}
	})
}

// TestCopyAcrossWorkspaces_BadRequestBeatsQuota pins the ORDER of the two
// refusals, which is a DR-6 agreement question rather than a preference.
//
// The preflight has no quota check at all — it documents the destination
// workspace's item quota as one of the things `valid` deliberately does not
// evaluate — so if the copy tested the quota first, a malformed request into a
// full cloud workspace would come back 403 plan_limit_exceeded from the copy
// and 400 malformed_override from its own preview (Codex round 18). A client
// told "you are out of room" cannot fix an override it was never told about.
//
// Moving field validation ahead of the quota costs nothing and weakens
// nothing: it is pure computation over rows already read, and the quota still
// runs inside the transaction before any insert, which is all DR-16 asks.
func TestCopyAcrossWorkspaces_BadRequestBeatsQuota(t *testing.T) {
	// A cap of 1, already consumed, so the destination is genuinely full.
	f := newQuotaFixture(t, 1)
	if _, err := f.s.CreateItem(f.wsB.ID, f.colB.ID, models.ItemCreate{Title: "Occupant"}); err != nil {
		t.Fatalf("CreateItem(occupant): %v", err)
	}
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Doomed", "body")

	// Control: with a well-formed request the quota IS what refuses.
	quotaReq := f.req()
	quotaReq.SourceItemID = src.ID
	quotaReq.EnforceItemLimit = true
	if _, err := f.s.CopyItemAcrossWorkspaces(quotaReq); err == nil {
		t.Fatal("fixture precondition: the destination is not actually full")
	} else {
		var limitErr *ItemLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("control: err = %v, want *ItemLimitError", err)
		}
	}

	// The real assertion: the same full destination, plus an undeclared
	// override, must report the OVERRIDE.
	badReq := f.req()
	badReq.SourceItemID = src.ID
	badReq.EnforceItemLimit = true
	badReq.FieldOverrides = map[string]any{"not_a_field": "x"}

	_, err := f.s.CopyItemAcrossWorkspaces(badReq)
	var undeclared *UndeclaredOverrideError
	if !errors.As(err, &undeclared) {
		t.Fatalf("err = %v (%T), want *UndeclaredOverrideError — a bad request must be reported as a "+
			"bad request whether or not the destination happens to be full", err, err)
	}
	var limitErr *ItemLimitError
	if errors.As(err, &limitErr) {
		t.Error("the quota refusal shadowed the malformed override")
	}
}

// --- The attachment-ref rewrite (TASK-2365) ---------------------------------

// TestRemapAttachmentRefsTokenizesLikeThePlanner pins the property that makes
// items_cross_workspace_copy.go's claim true — that the rewrite "covers
// precisely the reference set the plan cloned".
//
// The rewrite and the planner must AGREE ON WHERE A REFERENCE ENDS. The
// planner's regex is greedy, so `pad-attachment:<uuid>x` is ONE id, `<uuid>x`,
// which resolves to nothing and is deliberately left alone (DR-11a: an
// unresolvable ref keeps its literal text "so the copy renders exactly as
// broken as the source did"). A naive strings.ReplaceAll over
// "pad-attachment:"+old rewrote the `<uuid>` prefix inside it anyway (Codex
// round 26), producing text matching neither the plan nor the user's input.
func TestRemapAttachmentRefsTokenizesLikeThePlanner(t *testing.T) {
	const (
		oldID = "11111111-2222-4333-8444-555555555555"
		newID = "99999999-8888-4777-8666-555555555555"
	)
	idMap := map[string]string{oldID: newID}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"a plain reference is rewritten",
			"![a](pad-attachment:" + oldID + ")",
			"![a](pad-attachment:" + newID + ")",
		},
		{
			"a JSON-encoded reference is rewritten",
			`{"cover":"pad-attachment:` + oldID + `"}`,
			`{"cover":"pad-attachment:` + newID + `"}`,
		},
		{
			// The one this test exists for.
			"a LONGER id that merely starts with a mapped one is left alone",
			"pad-attachment:" + oldID + "x",
			"pad-attachment:" + oldID + "x",
		},
		{
			"both forms in one body are handled independently",
			"ok pad-attachment:" + oldID + " broken pad-attachment:" + oldID + "x",
			"ok pad-attachment:" + newID + " broken pad-attachment:" + oldID + "x",
		},
		{
			"a bare id outside the prefix is never touched",
			"the id is " + oldID + " on its own",
			"the id is " + oldID + " on its own",
		},
		{
			"an unmapped reference survives verbatim",
			"pad-attachment:00000000-0000-4000-8000-000000000000",
			"pad-attachment:00000000-0000-4000-8000-000000000000",
		},
		{"no references at all", "just prose", "just prose"},
		{"empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remapAttachmentRefs(tc.in, idMap); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}

	// An empty map is a no-op even over text full of references.
	busy := "pad-attachment:" + oldID
	if got := remapAttachmentRefs(busy, nil); got != busy {
		t.Fatalf("nil map rewrote %q to %q", busy, got)
	}
}

// --- The PreCheck hook (TASK-2365) ------------------------------------------

// TestCopyAcrossWorkspaces_PreCheckRefusalIsWrapped pins two guarantees the
// hook's callers depend on and cannot verify themselves.
//
// The WRAPPING is the one that matters operationally: a hook refusal is a
// 403/404 the HTTP layer renders, not an incident, and asking every caller to
// remember to wrap it made "forgot to wrap" a silent way to page an operator
// over a routine permission denial (Codex round 10). The caller's own error
// type still comes back out through errors.As.
func TestCopyAcrossWorkspaces_PreCheckRefusalIsWrapped(t *testing.T) {
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Refused", "body")

	sentinel := errors.New("caller's own refusal")
	req := f.req()
	req.SourceItemID = src.ID
	req.PreCheck = func(_ *sql.Tx, _ *models.Item, _ *models.Collection) error { return sentinel }

	_, err := f.s.CopyItemAcrossWorkspaces(req)
	var wrapped *CopyPreCheckError
	if !errors.As(err, &wrapped) {
		t.Fatalf("err = %v (%T), want it wrapped in *CopyPreCheckError", err, err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("the caller's own error is not recoverable through the wrapper: %v", err)
	}
	if !isExpectedCopyRejection(err) {
		t.Error("a hook refusal is logged as an incident; it is a status the caller renders")
	}
	// And nothing was written.
	if items, lErr := f.s.ListItems(f.wsB.ID, models.ItemListParams{}); lErr != nil || len(items) != 0 {
		t.Fatalf("the refused copy left %d item(s) in the destination (err=%v)", len(items), lErr)
	}
}

// TestCopyAcrossWorkspaces_PreCheckGetsCopies — the hook is handed DETACHED
// snapshots, so a hook that mutates (or retains and later mutates) what it was
// given cannot rewrite what the transaction actually copies, nor what the
// post-commit fanout says about it.
//
// Three shapes are exercised, because three different implementations fail at
// different ones:
//
//   - VALUE fields (Title, Content, the collection's Slug) — a plain struct
//     copy already handles these;
//   - a POINTER field written THROUGH (`*source.AssignedUserID = x`) — a plain
//     struct copy aliases it, and it reaches carryAssigneeTx (Codex round 11);
//   - a SLICE field appended to and mutated in place (ImplementationNotes) —
//     hand-enumerated pointer cloning misses these entirely (Codex round 12),
//     and they ride out on the source's move webhook payload.
//
// The last two are the ones that make this test worth having; assert on all
// three so a regression in any implementation strategy is caught.
func TestCopyAcrossWorkspaces_PreCheckGetsCopies(t *testing.T) {
	f := newCopyFixture(t)

	// The assignee must be a member of BOTH workspaces, or DR-8 drops it and
	// the pointer half of this test proves nothing.
	assignee := createTestUser(t, f.s, "assignee@example.com", "Assignee", "pw-assignee")
	intruder := createTestUser(t, f.s, "intruder@example.com", "Intruder", "pw-intruder")
	for _, ws := range []*models.Workspace{f.wsA, f.wsB} {
		for _, u := range []string{assignee.ID, intruder.ID} {
			if err := f.s.AddWorkspaceMember(ws.ID, u, "editor"); err != nil {
				t.Fatalf("AddWorkspaceMember: %v", err)
			}
		}
	}

	// implementation_notes hydrates Item.ImplementationNotes (a SLICE) via
	// hydrateItemComputedMetadata, which getItemTx runs — so the hook is
	// handed a slice header sharing the store's backing array unless the
	// snapshot is a genuine deep copy.
	src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title: "Original Title", Content: "body", AssignedUserID: &assignee.ID,
		Fields: `{"implementation_notes":[{"summary":"original note"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	var heldItem *models.Item
	var heldColl *models.Collection
	req := f.req()
	req.SourceItemID = src.ID
	req.PreCheck = func(_ *sql.Tx, source *models.Item, targetColl *models.Collection) error {
		heldItem, heldColl = source, targetColl
		source.Title = "Hijacked"
		source.Content = "hijacked body"
		targetColl.Slug = "hijacked-slug"
		if source.AssignedUserID == nil {
			t.Fatal("fixture precondition: the hook was handed an unassigned item")
		}
		// THE POINTER CASE: writing THROUGH the pointer, not replacing it.
		*source.AssignedUserID = intruder.ID
		// THE SLICE CASE: mutating the shared backing array in place.
		if len(source.ImplementationNotes) == 0 {
			t.Fatal("fixture precondition: the hook was handed no implementation notes")
		}
		source.ImplementationNotes[0].Summary = "hijacked note"
		return nil
	}

	res := f.copy(t, req)

	if res.Item.Title != "Original Title" {
		t.Errorf("the copy's title is %q; a PreCheck mutation reached the insert", res.Item.Title)
	}
	if res.Item.Content != "body" {
		t.Errorf("the copy's content is %q; a PreCheck mutation reached the insert", res.Item.Content)
	}
	if res.TargetCollection.Slug == "hijacked-slug" {
		t.Error("a PreCheck mutation reached the collection snapshot the caller fans out on")
	}
	if res.Item.AssignedUserID == nil || *res.Item.AssignedUserID != assignee.ID {
		t.Errorf("the copy is assigned to %v, want %s — a PreCheck write THROUGH a pointer field "+
			"reached the insert, so the snapshot is only shallow",
			res.Item.AssignedUserID, assignee.ID)
	}
	// res.Source is what the caller fans out on for the source's move webhook,
	// so a slice mutation that reached it would be published.
	if len(res.Source.ImplementationNotes) == 0 ||
		res.Source.ImplementationNotes[0].Summary != "original note" {
		t.Errorf("the source snapshot's notes are %+v; a PreCheck mutation of a SLICE field "+
			"reached the result the caller fans out on", res.Source.ImplementationNotes)
	}
	if heldItem == res.Source || heldColl == res.TargetCollection {
		t.Error("the hook was handed the canonical pointers, not copies")
	}
	if heldItem.AssignedUserID == res.Source.AssignedUserID {
		t.Error("the hook's item aliases the canonical item's AssignedUserID pointer")
	}
	if len(heldItem.ImplementationNotes) > 0 && len(res.Source.ImplementationNotes) > 0 &&
		&heldItem.ImplementationNotes[0] == &res.Source.ImplementationNotes[0] {
		t.Error("the hook's item shares the canonical item's ImplementationNotes backing array")
	}
}

// TestCopyRollbackErrorClassification pins the distinction between a genuine
// deadlock and SQLite writer contention.
//
// These two must never collapse into one signal. DR-9's lock ordering is meant
// to make 40P01 impossible, so a deadlock in production means the ordering is
// wrong and nothing else will surface it. SQLite's "database is locked" is the
// opposite: an expected saturation mode under burst load against a
// single-writer database with a 30-second busy timeout. Classifying both as
// deadlock=true at ERROR — which this code did until PLAN-2357's final review —
// leaves an operator unable to tell a lock-ordering bug from ordinary load.
func TestCopyRollbackErrorClassification(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		deadlock    bool
		lockTimeout bool
	}{
		{"nil", nil, false, false},
		{"postgres 40P01 by name", errors.New("pq: deadlock detected"), true, false},
		{"postgres 40P01 by code", errors.New("ERROR: something (SQLSTATE 40P01)"), true, false},
		{"sqlite busy timeout", errors.New("database is locked"), false, true},
		{"sqlite busy timeout wrapped", fmt.Errorf("copy item: %w", errors.New("database is locked")), false, true},
		{"unrelated", errors.New("no such column: bogus"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDeadlockError(tc.err); got != tc.deadlock {
				t.Errorf("isDeadlockError = %v, want %v", got, tc.deadlock)
			}
			if got := isLockTimeoutError(tc.err); got != tc.lockTimeout {
				t.Errorf("isLockTimeoutError = %v, want %v", got, tc.lockTimeout)
			}
			// The log site reports at most one of the two, so an operator
			// never sees a line claiming both.
			if isDeadlockError(tc.err) && isLockTimeoutError(tc.err) {
				t.Error("an error classified as BOTH deadlock and lock timeout")
			}
		})
	}
}
