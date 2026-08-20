package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for createItemTx — the tx-taking counterpart of CreateItem
// (PLAN-2357 / DR-9a). Each test below pins one line of the DR-9a parity
// checklist: slug / item_number / seq, content-flush watermarks, the initial
// item_versions row, wiki-link indexing plus broken-title resolution, the
// create-time status_transitions row, CreateItem's defaults, and its
// assignment-scope validation. Plus the transaction-participation test: roll
// the caller's tx back and assert none of the above persisted.

// createViaTx runs createItemTx inside a fresh transaction and commits it,
// which is what a caller that has nothing else to do in the tx looks like.
func createViaTx(t *testing.T, s *Store, workspaceID, collectionID string, input models.ItemCreate) *models.Item {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	item, err := s.createItemTx(tx, workspaceID, collectionID, input)
	if err != nil {
		t.Fatalf("createItemTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return item
}

// createViaTxErr runs createItemTx and rolls back, returning the error.
func createViaTxErr(t *testing.T, s *Store, workspaceID, collectionID string, input models.ItemCreate) error {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	_, err = s.createItemTx(tx, workspaceID, collectionID, input)
	return err
}

func scanCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(query), args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// itemNumber derefs models.Item.ItemNumber (*int), failing the test when the
// column came back NULL — which would itself be a parity break.
func itemNumber(t *testing.T, item *models.Item) int {
	t.Helper()
	if item.ItemNumber == nil {
		t.Fatalf("item %s has a NULL item_number", item.ID)
	}
	return *item.ItemNumber
}

func maxWorkspaceSeq(t *testing.T, s *Store, workspaceID string) int64 {
	t.Helper()
	var seq int64
	if err := s.db.QueryRow(s.q(`SELECT COALESCE(MAX(seq), 0) FROM items WHERE workspace_id = ?`), workspaceID).Scan(&seq); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	return seq
}

// --- Parity: slug allocation, scoped to the destination workspace ---

func TestCreateItemTx_AllocatesWorkspaceScopedSlug(t *testing.T) {
	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Slug Source")
	wsB := createTestWorkspace(t, s, "Slug Dest")
	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Tasks")

	// Same title exists in A — must NOT influence B's allocation.
	createTestItem(t, s, wsA.ID, colA.ID, "Shared Title", "")

	item := createViaTx(t, s, wsB.ID, colB.ID, models.ItemCreate{Title: "Shared Title", Fields: `{"status":"open"}`})
	if item.Slug != "shared-title" {
		t.Fatalf("expected base slug in destination workspace, got %q", item.Slug)
	}
}

func TestCreateItemTx_SlugCollisionResolvesToDistinctSlug(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Slug Collision")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	first := createTestItem(t, s, ws.ID, col.ID, "Collision Title", "")
	second := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Collision Title", Fields: `{"status":"open"}`})
	third := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Collision Title", Fields: `{"status":"open"}`})

	if first.Slug != "collision-title" {
		t.Fatalf("first slug = %q", first.Slug)
	}
	if second.Slug != "collision-title-2" {
		t.Fatalf("second slug = %q, want collision-title-2 (distinct, not an error or overwrite)", second.Slug)
	}
	if third.Slug != "collision-title-3" {
		t.Fatalf("third slug = %q, want collision-title-3", third.Slug)
	}
	if first.ID == second.ID || second.ID == third.ID {
		t.Fatalf("collision overwrote an existing item instead of creating a new one")
	}
	if n := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE workspace_id = ?`, ws.ID); n != 3 {
		t.Fatalf("expected 3 items, got %d", n)
	}
}

func TestCreateItemTx_UntitledFallbackSlug(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Untitled Slug")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// A title that slugifies to "" must fall back to "untitled", same as CreateItem.
	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "!!!", Fields: `{"status":"open"}`})
	if item.Slug != "untitled" {
		t.Fatalf("slug = %q, want untitled", item.Slug)
	}
}

// --- Parity: item_number ---

func TestCreateItemTx_AssignsNextItemNumber(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Item Numbers")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	first := createTestItem(t, s, ws.ID, col.ID, "One", "")
	second := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Two", Fields: `{"status":"open"}`})
	third := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Three", Fields: `{"status":"open"}`})

	if itemNumber(t, second) != itemNumber(t, first)+1 {
		t.Fatalf("item_number = %d, want %d", itemNumber(t, second), itemNumber(t, first)+1)
	}
	if itemNumber(t, third) != itemNumber(t, second)+1 {
		t.Fatalf("item_number = %d, want %d", itemNumber(t, third), itemNumber(t, second)+1)
	}
}

func TestCreateItemTx_ItemNumberIsWorkspaceScoped(t *testing.T) {
	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Numbers A")
	wsB := createTestWorkspace(t, s, "Numbers B")
	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Tasks")

	for i := 0; i < 3; i++ {
		createTestItem(t, s, wsA.ID, colA.ID, "A item", "")
	}
	item := createViaTx(t, s, wsB.ID, colB.ID, models.ItemCreate{Title: "B item", Fields: `{"status":"open"}`})
	if got := itemNumber(t, item); got != 1 {
		t.Fatalf("destination item_number = %d, want 1 (A's numbering must not leak)", got)
	}
}

// --- Parity: workspace seq (delta sync cursor) ---

func TestCreateItemTx_AdvancesWorkspaceSeq(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Seq")
	other := createTestWorkspace(t, s, "Seq Other")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	otherCol := createTestCollection(t, s, other.ID, "Tasks")

	createTestItem(t, s, ws.ID, col.ID, "Existing", "")
	before := maxWorkspaceSeq(t, s, ws.ID)
	otherBefore := maxWorkspaceSeq(t, s, other.ID)
	_ = otherCol

	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Seq bump", Fields: `{"status":"open"}`})

	if item.Seq <= before {
		t.Fatalf("item seq = %d, want > %d", item.Seq, before)
	}
	if got := maxWorkspaceSeq(t, s, ws.ID); got != item.Seq {
		t.Fatalf("workspace MAX(seq) = %d, want %d", got, item.Seq)
	}
	if got := maxWorkspaceSeq(t, s, other.ID); got != otherBefore {
		t.Fatalf("unrelated workspace seq moved: %d -> %d", otherBefore, got)
	}
}

// --- Parity: content flush watermarks ---

func TestCreateItemTx_ContentFlushWatermarks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Watermarks")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	withContent := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Has content", Content: "body", Fields: `{"status":"open"}`})
	empty := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "No content", Fields: `{"status":"open"}`})

	readWatermarks := func(id string) (*string, *int64) {
		var at *string
		var opLogID *int64
		if err := s.db.QueryRow(s.q(`SELECT content_flushed_at, content_flushed_op_log_id FROM items WHERE id = ?`), id).
			Scan(&at, &opLogID); err != nil {
			t.Fatalf("read watermarks: %v", err)
		}
		return at, opLogID
	}

	at, opLogID := readWatermarks(withContent.ID)
	if at == nil || *at == "" {
		t.Fatalf("content_flushed_at must be set when content is non-empty")
	}
	if opLogID == nil || *opLogID != 0 {
		t.Fatalf("content_flushed_op_log_id = %v, want 0", opLogID)
	}

	at, opLogID = readWatermarks(empty.ID)
	if at != nil || opLogID != nil {
		t.Fatalf("empty content must leave both watermarks NULL, got (%v, %v)", at, opLogID)
	}
}

// --- Parity: initial item_versions row ---

func TestCreateItemTx_WritesInitialVersionForNonEmptyContent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Versions")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{
		Title:     "Versioned",
		Content:   "the rewritten body",
		Fields:    `{"status":"open"}`,
		CreatedBy: "agent",
		Source:    "cli",
	})

	var content, createdBy, source string
	var versionSeq int
	var isDiff bool
	if err := s.db.QueryRow(s.q(`SELECT content, created_by, source, version_seq, is_diff FROM item_versions WHERE item_id = ?`), item.ID).
		Scan(&content, &createdBy, &source, &versionSeq, &isDiff); err != nil {
		t.Fatalf("read initial version: %v", err)
	}
	if content != "the rewritten body" {
		t.Fatalf("version content = %q", content)
	}
	if createdBy != "agent" || source != "cli" {
		t.Fatalf("version attribution = (%q, %q), want (agent, cli)", createdBy, source)
	}
	if versionSeq != 1 {
		t.Fatalf("version_seq = %d, want 1", versionSeq)
	}
	if isDiff {
		t.Fatalf("initial version must not be a diff")
	}
}

func TestCreateItemTx_NoVersionForEmptyContent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Versions Empty")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Bodyless", Fields: `{"status":"open"}`})
	if n := scanCount(t, s, `SELECT COUNT(*) FROM item_versions WHERE item_id = ?`, item.ID); n != 0 {
		t.Fatalf("expected no initial version for empty content, got %d", n)
	}
}

// --- Parity: wiki-link indexing ---

func TestCreateItemTx_IndexesWikiLinksFromContent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Wiki Index")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Link Target", "")

	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{
		Title:   "Mentions target",
		Content: "see [[Link Target]] for details",
		Fields:  `{"status":"open"}`,
	})

	var targetID sql.NullString
	var targetTitle sql.NullString
	if err := s.db.QueryRow(s.q(`SELECT target_item_id, target_title FROM item_wiki_links WHERE source_item_id = ?`), item.ID).
		Scan(&targetID, &targetTitle); err != nil {
		t.Fatalf("read wiki link row: %v", err)
	}
	if !targetID.Valid || targetID.String != target.ID {
		t.Fatalf("wiki link target = %v, want %s", targetID, target.ID)
	}
	if !targetTitle.Valid || targetTitle.String != "Link Target" {
		t.Fatalf("target_title = %v", targetTitle)
	}
}

func TestCreateItemTx_ResolvesBrokenTitleLinksInDestination(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Broken Titles")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// A pre-existing item mentions a title that doesn't exist yet: the row
	// lands broken (target_item_id NULL).
	mentioner := createTestItem(t, s, ws.ID, col.ID, "Mentioner", "waiting on [[Arriving Later]]")
	var pre sql.NullString
	if err := s.db.QueryRow(s.q(`SELECT target_item_id FROM item_wiki_links WHERE source_item_id = ?`), mentioner.ID).Scan(&pre); err != nil {
		t.Fatalf("read pre-arrival link: %v", err)
	}
	if pre.Valid {
		t.Fatalf("expected a broken link row before arrival, got target %s", pre.String)
	}

	arrival := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Arriving Later", Fields: `{"status":"open"}`})

	var post sql.NullString
	if err := s.db.QueryRow(s.q(`SELECT target_item_id FROM item_wiki_links WHERE source_item_id = ?`), mentioner.ID).Scan(&post); err != nil {
		t.Fatalf("read post-arrival link: %v", err)
	}
	if !post.Valid || post.String != arrival.ID {
		t.Fatalf("broken title link did not flip to the new item: %v", post)
	}
}

// --- Parity: create-time status_transitions row ---

func TestCreateItemTx_SeedsCreateTimeStatusTransition(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Transitions")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Born done", Fields: `{"status":"done"}`})

	var id, fieldKey, from, to, collectionID, workspaceID string
	if err := s.db.QueryRow(s.q(`
		SELECT id, field_key, from_status, to_status, collection_id, workspace_id
		FROM status_transitions WHERE item_id = ?
	`), item.ID).Scan(&id, &fieldKey, &from, &to, &collectionID, &workspaceID); err != nil {
		t.Fatalf("read create-time transition: %v", err)
	}
	if id != "create_"+item.ID {
		t.Fatalf("transition id = %q, want create_%s (the idempotent backfill key)", id, item.ID)
	}
	if fieldKey != "status" || from != "" || to != "done" {
		t.Fatalf("transition = (%q, %q -> %q), want (status, \"\" -> done)", fieldKey, from, to)
	}
	if collectionID != col.ID || workspaceID != ws.ID {
		t.Fatalf("transition scoped to (%s, %s), want (%s, %s)", collectionID, workspaceID, col.ID, ws.ID)
	}
}

func TestCreateItemTx_NoStatusTransitionWhenDoneFieldUnset(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "No Transition")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Statusless"})
	if n := scanCount(t, s, `SELECT COUNT(*) FROM status_transitions WHERE item_id = ?`, item.ID); n != 0 {
		t.Fatalf("expected no transition when the done field is unset, got %d", n)
	}
}

// --- Parity: CreateItem's defaults ---

func TestCreateItemTx_AppliesCreateItemDefaults(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Defaults")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Bare"})

	if item.Fields != "{}" {
		t.Fatalf("fields default = %q, want {}", item.Fields)
	}
	if item.Tags != "[]" {
		t.Fatalf("tags default = %q, want []", item.Tags)
	}
	if item.CreatedBy != "user" {
		t.Fatalf("created_by default = %q, want user", item.CreatedBy)
	}
	if item.LastModifiedBy != "user" {
		t.Fatalf("last_modified_by default = %q, want user", item.LastModifiedBy)
	}
	if item.Source != "web" {
		t.Fatalf("source default = %q, want web", item.Source)
	}
}

func TestCreateItemTx_DefaultsMatchCreateItemExactly(t *testing.T) {
	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Parity A")
	wsB := createTestWorkspace(t, s, "Parity B")
	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Tasks")

	input := models.ItemCreate{Title: "Parity Subject", Content: "parity body [[Nowhere]]", Fields: `{"status":"done"}`}

	viaCreateItem, err := s.CreateItem(wsA.ID, colA.ID, input)
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	viaTx := createViaTx(t, s, wsB.ID, colB.ID, input)

	if viaTx.Slug != viaCreateItem.Slug {
		t.Fatalf("slug: tx=%q createItem=%q", viaTx.Slug, viaCreateItem.Slug)
	}
	if itemNumber(t, viaTx) != itemNumber(t, viaCreateItem) {
		t.Fatalf("item_number: tx=%d createItem=%d", itemNumber(t, viaTx), itemNumber(t, viaCreateItem))
	}
	if viaTx.Fields != viaCreateItem.Fields || viaTx.Tags != viaCreateItem.Tags {
		t.Fatalf("fields/tags mismatch")
	}
	if viaTx.CreatedBy != viaCreateItem.CreatedBy || viaTx.LastModifiedBy != viaCreateItem.LastModifiedBy || viaTx.Source != viaCreateItem.Source {
		t.Fatalf("attribution mismatch")
	}
	if viaTx.Content != viaCreateItem.Content {
		t.Fatalf("content mismatch")
	}

	// And the same side-effect row counts on both sides.
	for _, q := range []string{
		`SELECT COUNT(*) FROM item_versions WHERE item_id = ?`,
		`SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ?`,
		`SELECT COUNT(*) FROM status_transitions WHERE item_id = ?`,
	} {
		a := scanCount(t, s, q, viaCreateItem.ID)
		b := scanCount(t, s, q, viaTx.ID)
		if a != b {
			t.Fatalf("row-count parity broken for %q: createItem=%d createItemTx=%d", q, a, b)
		}
		if a == 0 {
			t.Fatalf("test is vacuous: %q produced no rows on either path", q)
		}
	}
}

// --- Parity: assignment-scope validation ---

func TestCreateItemTx_RejectsForeignAssignedUser(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Assign Scope")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	outsider := createTestUser(t, s, "outsider-createtx@example.com", "Outsider", "password123")

	outsiderID := outsider.ID
	err := createViaTxErr(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Assigned out", AssignedUserID: &outsiderID})
	if err == nil || !strings.Contains(err.Error(), "not a member of this workspace") {
		t.Fatalf("expected membership rejection, got %v", err)
	}
	if n := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE workspace_id = ?`, ws.ID); n != 0 {
		t.Fatalf("rejected create still wrote an item")
	}
}

func TestCreateItemTx_AcceptsMemberAssignedUser(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Assign Member")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	member := createTestUser(t, s, "member-createtx@example.com", "Member", "password123")
	if err := s.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	memberID := member.ID
	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Assigned in", AssignedUserID: &memberID})
	if item.AssignedUserID == nil || *item.AssignedUserID != member.ID {
		t.Fatalf("assigned user not persisted: %v", item.AssignedUserID)
	}
}

func TestCreateItemTx_RejectsForeignAgentRole(t *testing.T) {
	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Role Owner")
	wsB := createTestWorkspace(t, s, "Role Borrower")
	colB := createTestCollection(t, s, wsB.ID, "Tasks")

	role, err := s.CreateAgentRole(wsA.ID, models.AgentRoleCreate{Name: "Backend"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}

	roleID := role.ID
	err = createViaTxErr(t, s, wsB.ID, colB.ID, models.ItemCreate{Title: "Foreign role", AgentRoleID: &roleID})
	if err == nil || !strings.Contains(err.Error(), "agent role does not belong to this workspace") {
		t.Fatalf("expected agent-role rejection, got %v", err)
	}
	if n := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE workspace_id = ?`, wsB.ID); n != 0 {
		t.Fatalf("rejected create still wrote an item")
	}
}

func TestCreateItemTx_AcceptsLocalAgentRole(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Role Local")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	role, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Backend"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}

	roleID := role.ID
	item := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "Local role", AgentRoleID: &roleID})
	if item.AgentRoleID == nil || *item.AgentRoleID != role.ID {
		t.Fatalf("agent role not persisted: %v", item.AgentRoleID)
	}
}

// --- The transaction-participation test ---

func TestCreateItemTx_RollbackPersistsNothing(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Rollback")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// A pre-existing broken title link that the rolled-back create would
	// otherwise have resolved.
	mentioner := createTestItem(t, s, ws.ID, col.ID, "Rollback mentioner", "waits on [[Phantom Item]]")
	seqBefore := maxWorkspaceSeq(t, s, ws.ID)
	itemsBefore := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE workspace_id = ?`, ws.ID)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	item, err := s.createItemTx(tx, ws.ID, col.ID, models.ItemCreate{
		Title:   "Phantom Item",
		Content: "body that mentions [[Rollback mentioner]]",
		Fields:  `{"status":"done"}`,
	})
	if err != nil {
		tx.Rollback()
		t.Fatalf("createItemTx: %v", err)
	}
	if item == nil {
		tx.Rollback()
		t.Fatalf("createItemTx returned nil item")
	}
	phantomID := item.ID
	phantomSeq := item.Seq
	if phantomSeq <= seqBefore {
		tx.Rollback()
		t.Fatalf("test is vacuous: in-tx seq %d did not advance past %d", phantomSeq, seqBefore)
	}
	// POSITIVE CONTROL for the outbox assertion below (TASK-2658). "No outbox
	// row after rollback" is trivially true if the create never wrote one, so
	// the row has to be observed INSIDE the transaction first. Without this
	// leg, deleting the emit call from createItemTxWithID leaves the test
	// green — which is the exact failure mode this whole unit exists to
	// prevent, and it would be embarrassing to reproduce it in the test.
	var inTxEvents int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE subject_id = ?`), phantomID).Scan(&inTxEvents); err != nil {
		tx.Rollback()
		t.Fatalf("in-tx outbox count: %v", err)
	}
	if inTxEvents != 1 {
		tx.Rollback()
		t.Fatalf("in-tx outbox rows for created item = %d, want 1 (the create emitted no event)", inTxEvents)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if n := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE id = ?`, phantomID); n != 0 {
		t.Fatalf("item survived rollback")
	}
	if n := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE workspace_id = ?`, ws.ID); n != itemsBefore {
		t.Fatalf("workspace item count changed across rollback: %d -> %d", itemsBefore, n)
	}
	if n := scanCount(t, s, `SELECT COUNT(*) FROM item_versions WHERE item_id = ?`, phantomID); n != 0 {
		t.Fatalf("item_versions row survived rollback")
	}
	if n := scanCount(t, s, `SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ?`, phantomID); n != 0 {
		t.Fatalf("item_wiki_links row survived rollback")
	}
	if n := scanCount(t, s, `SELECT COUNT(*) FROM status_transitions WHERE item_id = ?`, phantomID); n != 0 {
		t.Fatalf("status_transitions row survived rollback")
	}
	// The event must roll back with the row it describes (SPEC-3 §choke
	// point, TASK-2658). A leaked event here would advertise an item that
	// does not exist, to consumers that cannot tell the difference.
	if n := scanCount(t, s, `SELECT COUNT(*) FROM event_outbox WHERE subject_id = ?`, phantomID); n != 0 {
		t.Fatalf("event_outbox row survived rollback: a rolled-back mutation leaked an event")
	}
	if got := maxWorkspaceSeq(t, s, ws.ID); got != seqBefore {
		t.Fatalf("workspace seq advanced across rollback: %d -> %d", seqBefore, got)
	}
	// The broken-title resolution must have rolled back too.
	var target sql.NullString
	if err := s.db.QueryRow(s.q(`SELECT target_item_id FROM item_wiki_links WHERE source_item_id = ?`), mentioner.ID).Scan(&target); err != nil {
		t.Fatalf("read mentioner link: %v", err)
	}
	if target.Valid {
		t.Fatalf("broken-title resolution survived rollback (target=%s)", target.String)
	}

	// The next create reuses the seq the rolled-back one held — proof it was
	// never committed.
	next := createViaTx(t, s, ws.ID, col.ID, models.ItemCreate{Title: "After rollback", Fields: `{"status":"open"}`})
	if next.Seq != phantomSeq {
		t.Fatalf("post-rollback seq = %d, want the rolled-back value %d", next.Seq, phantomSeq)
	}
}

// --- Concurrency: no retry loop, so the locks must carry it ---

// CreateItem survives a concurrent item_number claim via its retry loop.
// createItemTx has no retry (a failed statement poisons the caller's tx), so
// the workspace advisory lock — taken before the slug scan and held for the
// whole transaction — is the only thing standing between concurrent copies
// and duplicate item_numbers or duplicate slugs. Runs on both dialects:
// Postgres exercises pg_advisory_xact_lock, SQLite exercises BEGIN IMMEDIATE.
func TestCreateItemTx_ConcurrentCreatesGetDistinctNumbersAndSlugs(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Concurrent Create")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	const n = 8
	type result struct {
		slug   string
		number int
		seq    int64
		err    error
	}
	results := make([]result, n)
	start := make(chan struct{})
	done := make(chan int, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			<-start
			tx, err := s.db.Begin()
			if err != nil {
				results[i] = result{err: err}
				done <- i
				return
			}
			item, err := s.createItemTx(tx, ws.ID, col.ID, models.ItemCreate{
				Title:  "Racing Title",
				Fields: `{"status":"open"}`,
			})
			if err != nil {
				tx.Rollback()
				results[i] = result{err: err}
				done <- i
				return
			}
			if err := tx.Commit(); err != nil {
				results[i] = result{err: err}
				done <- i
				return
			}
			if item.ItemNumber == nil {
				results[i] = result{err: fmt.Errorf("item %s has a NULL item_number", item.ID)}
				done <- i
				return
			}
			results[i] = result{slug: item.Slug, number: *item.ItemNumber, seq: item.Seq}
			done <- i
		}(i)
	}
	close(start)
	for i := 0; i < n; i++ {
		<-done
	}

	slugs := map[string]bool{}
	numbers := map[int]bool{}
	seqs := map[int64]bool{}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("goroutine %d: %v", i, r.err)
		}
		if slugs[r.slug] {
			t.Fatalf("duplicate slug %q across concurrent creates", r.slug)
		}
		if numbers[r.number] {
			t.Fatalf("duplicate item_number %d across concurrent creates", r.number)
		}
		if seqs[r.seq] {
			t.Fatalf("duplicate seq %d across concurrent creates", r.seq)
		}
		slugs[r.slug] = true
		numbers[r.number] = true
		seqs[r.seq] = true
	}
	if got := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE workspace_id = ?`, ws.ID); got != n {
		t.Fatalf("committed %d items, want %d", got, n)
	}
	if got := scanCount(t, s, `SELECT COUNT(DISTINCT slug) FROM items WHERE workspace_id = ?`, ws.ID); got != n {
		t.Fatalf("%d distinct slugs persisted, want %d", got, n)
	}
}

// The same race across the two entry points: half the writers go through the
// public CreateItem, half through createItemTx on their own transaction. Both
// now allocate the slug inside the transaction under the workspace lock, so
// neither can hand the other a stale scan. Before that change CreateItem
// allocated its slug before opening the transaction and re-submitted the same
// stale value on every retry, so a concurrent same-title create made it burn
// all ten attempts and fail with a unique-constraint error.
func TestCreateItemTx_MixedWithCreateItemConcurrently(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Mixed Concurrent")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	const n = 8
	errs := make([]error, n)
	slugs := make([]string, n)
	start := make(chan struct{})
	done := make(chan struct{}, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			<-start
			input := models.ItemCreate{Title: "Mixed Racing Title", Fields: `{"status":"open"}`}
			if i%2 == 0 {
				item, err := s.CreateItem(ws.ID, col.ID, input)
				if err != nil {
					errs[i] = err
					return
				}
				slugs[i] = item.Slug
				return
			}
			tx, err := s.db.Begin()
			if err != nil {
				errs[i] = err
				return
			}
			item, err := s.createItemTx(tx, ws.ID, col.ID, input)
			if err != nil {
				tx.Rollback()
				errs[i] = err
				return
			}
			if err := tx.Commit(); err != nil {
				errs[i] = err
				return
			}
			slugs[i] = item.Slug
		}(i)
	}
	close(start)
	for i := 0; i < n; i++ {
		<-done
	}

	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d (%s path) failed: %v", i, map[bool]string{true: "CreateItem", false: "createItemTx"}[i%2 == 0], err)
		}
		if seen[slugs[i]] {
			t.Fatalf("duplicate slug %q across mixed-path concurrent creates", slugs[i])
		}
		seen[slugs[i]] = true
	}
	if got := scanCount(t, s, `SELECT COUNT(DISTINCT slug) FROM items WHERE workspace_id = ?`, ws.ID); got != n {
		t.Fatalf("%d distinct slugs persisted, want %d", got, n)
	}
}

// A caller doing more work in the same transaction sees the created item, and
// its own later failure takes the create down with it.
func TestCreateItemTx_VisibleToCallerBeforeCommit(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "In-tx Visibility")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	item, err := s.createItemTx(tx, ws.ID, col.ID, models.ItemCreate{Title: "Visible", Content: "x", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("createItemTx: %v", err)
	}

	// Uncommitted, so invisible outside the tx...
	if n := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE id = ?`, item.ID); n != 0 {
		t.Fatalf("uncommitted item visible outside the transaction")
	}
	// ...but readable inside it, which is what an orchestrator needs to write
	// a provenance row referencing the new item in the same tx.
	var n int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE id = ?`), item.ID).Scan(&n); err != nil {
		t.Fatalf("in-tx read: %v", err)
	}
	if n != 1 {
		t.Fatalf("created item not visible inside its own transaction")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := scanCount(t, s, `SELECT COUNT(*) FROM items WHERE id = ?`, item.ID); n != 1 {
		t.Fatalf("item missing after commit")
	}
}
