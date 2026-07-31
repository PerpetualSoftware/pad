package store

import (
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// moveFixture is a source workspace plus two destination workspaces, each
// with a collection, so cross-workspace provenance rows can be written
// against real items (target_item_id carries a FK to items).
type moveFixture struct {
	s      *Store
	srcWS  *models.Workspace
	dstWS  *models.Workspace
	dst2WS *models.Workspace
	source *models.Item
	actor  string
}

func newMoveFixture(t *testing.T, name string) moveFixture {
	t.Helper()
	s := testStore(t)

	srcWS := createTestWorkspace(t, s, name+" Source")
	dstWS := createTestWorkspace(t, s, name+" Dest")
	dst2WS := createTestWorkspace(t, s, name+" Dest Two")

	srcColl := createTestCollection(t, s, srcWS.ID, "Tasks")
	source := createTestItem(t, s, srcWS.ID, srcColl.ID, "Original", "")

	return moveFixture{s: s, srcWS: srcWS, dstWS: dstWS, dst2WS: dst2WS, source: source, actor: "user-1"}
}

// dest creates a destination item in ws and returns it.
func (f moveFixture) dest(t *testing.T, ws *models.Workspace, title string) *models.Item {
	t.Helper()
	coll, err := f.s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   title + " Coll",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create destination collection: %v", err)
	}
	return createTestItem(t, f.s, ws.ID, coll.ID, title, "")
}

// record writes one provenance row through a committed transaction.
func (f moveFixture) record(t *testing.T, m models.ItemWorkspaceMove) *models.ItemWorkspaceMove {
	t.Helper()
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	stored, err := f.s.RecordItemWorkspaceMoveTx(tx, m)
	if err != nil {
		t.Fatalf("RecordItemWorkspaceMoveTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return stored
}

func seqPtr(v int64) *int64 { return &v }

// TestRecordItemWorkspaceMoveTx_RoundTrip covers insert-in-tx plus both
// lookups, and asserts every column survives the dialect round-trip —
// notably archived_source (INTEGER on SQLite, BOOLEAN on Postgres) and the
// nullable source_seq.
func TestRecordItemWorkspaceMoveTx_RoundTrip(t *testing.T) {
	f := newMoveFixture(t, "RoundTrip")
	target := f.dest(t, f.dstWS, "Copy")

	stored := f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID,
		SourceItemID:      f.source.ID,
		TargetWorkspaceID: f.dstWS.ID,
		TargetItemID:      target.ID,
		ArchivedSource:    true,
		SourceSeq:         seqPtr(42),
		CreatedBy:         f.actor,
	})
	if stored.ID == "" {
		t.Fatal("ID not generated")
	}
	if stored.CreatedAt == "" {
		t.Fatal("CreatedAt not generated")
	}

	forward, err := f.s.ListItemWorkspaceMovesBySource(f.source.ID)
	if err != nil {
		t.Fatalf("ListItemWorkspaceMovesBySource: %v", err)
	}
	if len(forward) != 1 {
		t.Fatalf("forward lookup: got %d rows, want 1", len(forward))
	}
	got := forward[0]
	if got.ID != stored.ID {
		t.Errorf("ID: got %q, want %q", got.ID, stored.ID)
	}
	if got.SourceWorkspaceID != f.srcWS.ID || got.SourceItemID != f.source.ID {
		t.Errorf("source columns wrong: %+v", got)
	}
	if got.TargetWorkspaceID != f.dstWS.ID || got.TargetItemID != target.ID {
		t.Errorf("target columns wrong: %+v", got)
	}
	if !got.ArchivedSource {
		t.Error("ArchivedSource lost in round-trip; want true")
	}
	if got.SourceSeq == nil || *got.SourceSeq != 42 {
		t.Errorf("SourceSeq: got %v, want 42", got.SourceSeq)
	}
	if got.CreatedBy != f.actor {
		t.Errorf("CreatedBy: got %q, want %q", got.CreatedBy, f.actor)
	}
	if got.CreatedAt != stored.CreatedAt {
		t.Errorf("CreatedAt: got %q, want %q", got.CreatedAt, stored.CreatedAt)
	}

	// Back lookup finds the same row from the destination side.
	back, err := f.s.GetItemWorkspaceMoveByTarget(target.ID)
	if err != nil {
		t.Fatalf("GetItemWorkspaceMoveByTarget: %v", err)
	}
	if back == nil {
		t.Fatal("back lookup returned nil for a recorded target")
	}
	if back.ID != stored.ID || back.SourceItemID != f.source.ID {
		t.Errorf("back lookup returned wrong row: %+v", back)
	}

	// A target with no provenance is (nil, nil), not an error.
	none, err := f.s.GetItemWorkspaceMoveByTarget(f.source.ID)
	if err != nil {
		t.Fatalf("GetItemWorkspaceMoveByTarget (absent): %v", err)
	}
	if none != nil {
		t.Errorf("expected nil for an item that was never a copy target, got %+v", none)
	}
}

// TestRecordItemWorkspaceMoveTx_CopyLeavesSeqNull pins the copy-vs-move
// distinction: a plain copy never archives, so it has no source workspace seq
// and must store NULL.
func TestRecordItemWorkspaceMoveTx_CopyLeavesSeqNull(t *testing.T) {
	f := newMoveFixture(t, "CopySeqNull")
	target := f.dest(t, f.dstWS, "Copy")

	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID,
		SourceItemID:      f.source.ID,
		TargetWorkspaceID: f.dstWS.ID,
		TargetItemID:      target.ID,
		ArchivedSource:    false,
		CreatedBy:         f.actor,
	})

	forward, err := f.s.ListItemWorkspaceMovesBySource(f.source.ID)
	if err != nil {
		t.Fatalf("forward lookup: %v", err)
	}
	if len(forward) != 1 {
		t.Fatalf("got %d rows, want 1", len(forward))
	}
	if forward[0].ArchivedSource {
		t.Error("ArchivedSource: got true, want false for a plain copy")
	}
	if forward[0].SourceSeq != nil {
		t.Errorf("SourceSeq: got %v, want nil for a plain copy", *forward[0].SourceSeq)
	}
}

// Fixed row IDs, used where a test must survive mutation of the ORDER BY.
// The ordering ends in `id DESC`, so a test that lets the helper mint random
// UUIDs would pick the right answer roughly half the time even with the
// meaningful ordering term deleted. Assigning IDs whose lexical order
// contradicts the expected answer makes the id tiebreak actively hostile:
// drop the term under test and the assertion fails every run, not sometimes.
const (
	lexHighMoveID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	lexLowMoveID  = "00000000-0000-0000-0000-000000000000"
)

// TestListItemWorkspaceMovesBySource_MultipleDestinationsNewestFirst covers
// the "forward lookup returns a SET" contract: one source copied into several
// workspaces yields several rows, newest first — and only that source's rows.
func TestListItemWorkspaceMovesBySource_MultipleDestinationsNewestFirst(t *testing.T) {
	f := newMoveFixture(t, "MultiDest")
	first := f.dest(t, f.dstWS, "First")
	second := f.dest(t, f.dst2WS, "Second")

	// The OLDER row gets the lexically HIGHER id, so the created_at ordering
	// is the only thing that can put `second` in front.
	f.record(t, models.ItemWorkspaceMove{
		ID:                lexHighMoveID,
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: first.ID,
		CreatedBy: f.actor, CreatedAt: "2026-01-01T00:00:00Z",
	})
	f.record(t, models.ItemWorkspaceMove{
		ID:                lexLowMoveID,
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dst2WS.ID, TargetItemID: second.ID,
		CreatedBy: f.actor, CreatedAt: "2026-01-02T00:00:00Z",
	})

	// A row belonging to a DIFFERENT source in the same workspace. Without it
	// the lookup's WHERE clause could be deleted entirely and every
	// assertion here would still hold.
	otherSource := createTestItem(t, f.s, f.srcWS.ID,
		createTestCollection(t, f.s, f.srcWS.ID, "Other Src").ID, "Other Source", "")
	otherTarget := f.dest(t, f.dstWS, "Other Target")
	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: otherSource.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: otherTarget.ID,
		CreatedBy: f.actor, CreatedAt: "2026-01-03T00:00:00Z",
	})

	forward, err := f.s.ListItemWorkspaceMovesBySource(f.source.ID)
	if err != nil {
		t.Fatalf("forward lookup: %v", err)
	}
	if len(forward) != 2 {
		t.Fatalf("got %d rows, want 2 (the other source's row must be excluded)", len(forward))
	}
	for _, m := range forward {
		if m.SourceItemID != f.source.ID {
			t.Errorf("forward lookup leaked a row for source %q", m.SourceItemID)
		}
	}
	if forward[0].TargetItemID != second.ID {
		t.Errorf("newest-first violated: first row targets %q, want the later copy %q",
			forward[0].TargetItemID, second.ID)
	}
	if forward[1].TargetItemID != first.ID {
		t.Errorf("second row targets %q, want the earlier copy %q", forward[1].TargetItemID, first.ID)
	}

	// Each destination resolves back to its OWN row — asserting the target
	// too, so a lookup ignoring its WHERE clause is caught here as well.
	backCases := map[*models.Item]string{first: f.source.ID, second: f.source.ID, otherTarget: otherSource.ID}
	for target, wantSource := range backCases {
		back, err := f.s.GetItemWorkspaceMoveByTarget(target.ID)
		if err != nil {
			t.Fatalf("back lookup for %s: %v", target.ID, err)
		}
		if back == nil {
			t.Errorf("back lookup for %s returned nil", target.ID)
			continue
		}
		if back.TargetItemID != target.ID {
			t.Errorf("back lookup for %s returned a row targeting %q", target.ID, back.TargetItemID)
		}
		if back.SourceItemID != wantSource {
			t.Errorf("back lookup for %s resolved to source %q, want %q", target.ID, back.SourceItemID, wantSource)
		}
	}
}

// TestRecordItemWorkspaceMoveTx_RollbackLeavesNoRow proves the row is bound to
// the caller's transaction: this is the whole reason the helper is tx-taking
// rather than self-committing (DR-9).
func TestRecordItemWorkspaceMoveTx_RollbackLeavesNoRow(t *testing.T) {
	f := newMoveFixture(t, "Rollback")
	target := f.dest(t, f.dstWS, "Copy")

	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := f.s.RecordItemWorkspaceMoveTx(tx, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: target.ID,
		ArchivedSource: true, SourceSeq: seqPtr(7), CreatedBy: f.actor,
	}); err != nil {
		t.Fatalf("RecordItemWorkspaceMoveTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	forward, err := f.s.ListItemWorkspaceMovesBySource(f.source.ID)
	if err != nil {
		t.Fatalf("forward lookup: %v", err)
	}
	if len(forward) != 0 {
		t.Errorf("rolled-back tx left %d provenance rows behind", len(forward))
	}
	back, err := f.s.GetItemWorkspaceMoveByTarget(target.ID)
	if err != nil {
		t.Fatalf("back lookup: %v", err)
	}
	if back != nil {
		t.Errorf("rolled-back tx left a back-pointer: %+v", back)
	}
}

// TestRecordItemWorkspaceMoveTx_RequiresIdentifiers guards the write boundary:
// source_item_id and target_item_id carry no workspace FK pair that would
// catch a blank, and a blank created_by would violate NOT NULL only at the
// driver layer with a far worse error.
func TestRecordItemWorkspaceMoveTx_RequiresIdentifiers(t *testing.T) {
	f := newMoveFixture(t, "Validation")
	target := f.dest(t, f.dstWS, "Copy")

	valid := models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: target.ID,
		CreatedBy: f.actor,
	}

	cases := map[string]func(*models.ItemWorkspaceMove){
		"source_workspace_id": func(m *models.ItemWorkspaceMove) { m.SourceWorkspaceID = "" },
		"source_item_id":      func(m *models.ItemWorkspaceMove) { m.SourceItemID = "" },
		"target_workspace_id": func(m *models.ItemWorkspaceMove) { m.TargetWorkspaceID = "" },
		"target_item_id":      func(m *models.ItemWorkspaceMove) { m.TargetItemID = "" },
		"created_by":          func(m *models.ItemWorkspaceMove) { m.CreatedBy = "" },
	}
	for name, blank := range cases {
		t.Run(name, func(t *testing.T) {
			tx, err := f.s.db.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()

			m := valid
			blank(&m)
			if _, err := f.s.RecordItemWorkspaceMoveTx(tx, m); err == nil {
				t.Errorf("blank %s was accepted", name)
			}
		})
	}
}

// TestRecordItemWorkspaceMoveTx_SourceSeqMatchesArchivedSource pins the
// invariant that makes the same-second ordering below trustworthy: a move
// MUST carry the seq its archive assigned, and a copy — which never archives
// — must not carry one. An archived row with a nil seq would silently fall
// through to the ID tiebreak, which is exactly the arbitrary answer source_seq
// exists to prevent (DR-2a).
func TestRecordItemWorkspaceMoveTx_SourceSeqMatchesArchivedSource(t *testing.T) {
	f := newMoveFixture(t, "SeqInvariant")
	target := f.dest(t, f.dstWS, "Copy")

	base := models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: target.ID,
		CreatedBy: f.actor,
	}

	cases := []struct {
		name     string
		archived bool
		seq      *int64
	}{
		{"move without seq", true, nil},
		{"copy with seq", false, seqPtr(5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.s.db.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()

			m := base
			m.ArchivedSource = tc.archived
			m.SourceSeq = tc.seq
			if _, err := f.s.RecordItemWorkspaceMoveTx(tx, m); err == nil {
				t.Errorf("%s was accepted; want an error", tc.name)
			}
		})
	}
}

// TestItemWorkspaceMoves_TargetIsUnique proves the back-pointer cannot become
// ambiguous. Two rows naming one destination is a bug by construction (the
// row is written in the transaction that creates the target), and left
// unenforced it would silently change which source the back lookup names.
func TestItemWorkspaceMoves_TargetIsUnique(t *testing.T) {
	f := newMoveFixture(t, "TargetUnique")
	target := f.dest(t, f.dstWS, "Copy")
	otherSource := createTestItem(t, f.s, f.srcWS.ID,
		createTestCollection(t, f.s, f.srcWS.ID, "Other Src").ID, "Other Source", "")

	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: target.ID,
		CreatedBy: f.actor,
	})

	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if _, err := f.s.RecordItemWorkspaceMoveTx(tx, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: otherSource.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: target.ID,
		CreatedBy: f.actor,
	}); err == nil {
		t.Error("a second provenance row for the same target was accepted")
	}
}

// TestItemWorkspaceMoves_MovedToIgnoresCopies is DR-2a's first acceptance
// criterion: copy twice, then move. The archived source advertises the MOVE
// target only — neither copy claims the source went anywhere.
func TestItemWorkspaceMoves_MovedToIgnoresCopies(t *testing.T) {
	f := newMoveFixture(t, "MovedToIgnoresCopies")
	copyA := f.dest(t, f.dstWS, "Copy A")
	copyB := f.dest(t, f.dst2WS, "Copy B")
	moveTarget := f.dest(t, f.dstWS, "Move Target")

	// Two plain copies FIRST, then the move — and the copies carry LATER
	// created_at values than the move, so a naive "newest row wins" that
	// forgets to filter on archived_source picks a copy and fails here.
	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: copyA.ID,
		ArchivedSource: false, CreatedBy: f.actor, CreatedAt: "2026-03-02T00:00:00Z",
	})
	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dst2WS.ID, TargetItemID: copyB.ID,
		ArchivedSource: false, CreatedBy: f.actor, CreatedAt: "2026-03-03T00:00:00Z",
	})
	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: moveTarget.ID,
		ArchivedSource: true, SourceSeq: seqPtr(10),
		CreatedBy: f.actor, CreatedAt: "2026-03-01T00:00:00Z",
	})

	forward, err := f.s.ListItemWorkspaceMovesBySource(f.source.ID)
	if err != nil {
		t.Fatalf("forward lookup: %v", err)
	}
	if len(forward) != 3 {
		t.Fatalf("got %d rows, want 3", len(forward))
	}

	movedTo := firstArchived(forward)
	if movedTo == nil {
		t.Fatal("no archived_source row found; the move produced no moved-to pointer")
	}
	if movedTo.TargetItemID != moveTarget.ID {
		t.Errorf("moved-to resolved to %q, want the move target %q", movedTo.TargetItemID, moveTarget.ID)
	}
}

// TestItemWorkspaceMoves_MovedToSameSecondUsesSourceSeq is the criterion that
// justifies source_seq existing at all: two restore→move cycles inside the
// SAME second. created_at is second-precision RFC3339, so it ties, and only
// source_seq can say which destination is the later one. Without the seq
// ordering this test passes or fails by luck.
func TestItemWorkspaceMoves_MovedToSameSecondUsesSourceSeq(t *testing.T) {
	f := newMoveFixture(t, "SameSecond")
	earlier := f.dest(t, f.dstWS, "Earlier Move")
	later := f.dest(t, f.dst2WS, "Later Move")

	const sameSecond = "2026-04-01T12:00:00Z"

	// Insert the LATER move first so row insertion order can't be what makes
	// the assertion pass — and give it the lexically LOWEST id while the
	// earlier move gets the highest. created_at is identical, so with the
	// source_seq term removed from the ordering the query falls through to
	// `id DESC` and returns `earlier` DETERMINISTICALLY. That is the point:
	// with random UUIDs this test would still pass half the time against a
	// query that had lost the very ordering it exists to prove.
	f.record(t, models.ItemWorkspaceMove{
		ID:                lexLowMoveID,
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dst2WS.ID, TargetItemID: later.ID,
		ArchivedSource: true, SourceSeq: seqPtr(200),
		CreatedBy: f.actor, CreatedAt: sameSecond,
	})
	f.record(t, models.ItemWorkspaceMove{
		ID:                lexHighMoveID,
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: earlier.ID,
		ArchivedSource: true, SourceSeq: seqPtr(100),
		CreatedBy: f.actor, CreatedAt: sameSecond,
	})

	forward, err := f.s.ListItemWorkspaceMovesBySource(f.source.ID)
	if err != nil {
		t.Fatalf("forward lookup: %v", err)
	}
	if len(forward) != 2 {
		t.Fatalf("got %d rows, want 2 (restore-then-move-again must NOT be deduped)", len(forward))
	}

	movedTo := firstArchived(forward)
	if movedTo == nil {
		t.Fatal("no archived_source row found")
	}
	if movedTo.TargetItemID != later.ID {
		t.Errorf("same-second tie resolved to %q, want the higher-seq destination %q",
			movedTo.TargetItemID, later.ID)
	}
	if movedTo.SourceSeq == nil || *movedTo.SourceSeq != 200 {
		t.Errorf("moved-to row carries seq %v, want 200", movedTo.SourceSeq)
	}
}

// firstArchived picks the newest row whose source was archived — the head of
// what the moved-to pointer considers.
//
// It is a TEST helper for exercising the broad forward lookup's ordering, not
// a mirror of the consumer: the real consumer (TASK-2359) queries
// ListArchivedItemWorkspaceMovesBySource and ACL-filters the whole bounded
// set per destination rather than taking the first entry.
func firstArchived(moves []models.ItemWorkspaceMove) *models.ItemWorkspaceMove {
	for i := range moves {
		if moves[i].ArchivedSource {
			return &moves[i]
		}
	}
	return nil
}

// TestPurgeWorkspaceData_ClearsItemWorkspaceMovesBothDirections covers the FK
// hazard the two-workspace shape introduces: item_workspace_moves references
// workspaces(id) twice with RESTRICT, so a purge that cleared only one
// direction would fail outright when the purged workspace sits on the other
// end.
func TestPurgeWorkspaceData_ClearsItemWorkspaceMovesBothDirections(t *testing.T) {
	f := newMoveFixture(t, "Purge")
	s := f.s

	// f.srcWS is the workspace we purge. One row where it is the SOURCE, one
	// where it is the TARGET.
	outbound := f.dest(t, f.dstWS, "Outbound")
	inboundSource := createTestItem(t, s, f.dstWS.ID,
		createTestCollection(t, s, f.dstWS.ID, "Inbound Src Coll").ID, "Inbound Source", "")
	inboundTarget := createTestItem(t, s, f.srcWS.ID,
		createTestCollection(t, s, f.srcWS.ID, "Inbound Dst Coll").ID, "Inbound Target", "")

	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: outbound.ID,
		ArchivedSource: true, SourceSeq: seqPtr(1), CreatedBy: f.actor,
	})
	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.dstWS.ID, SourceItemID: inboundSource.ID,
		TargetWorkspaceID: f.srcWS.ID, TargetItemID: inboundTarget.ID,
		CreatedBy: f.actor,
	})

	// A row entirely inside the surviving workspace must NOT be over-purged.
	bystanderSrc := createTestItem(t, s, f.dstWS.ID,
		createTestCollection(t, s, f.dstWS.ID, "Bystander Src").ID, "Bystander Source", "")
	bystanderDst := createTestItem(t, s, f.dst2WS.ID,
		createTestCollection(t, s, f.dst2WS.ID, "Bystander Dst").ID, "Bystander Target", "")
	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.dstWS.ID, SourceItemID: bystanderSrc.ID,
		TargetWorkspaceID: f.dst2WS.ID, TargetItemID: bystanderDst.ID,
		CreatedBy: f.actor,
	})

	if got := s.countRows(t, `SELECT COUNT(*) FROM item_workspace_moves`); got != 3 {
		t.Fatalf("seed: got %d provenance rows, want 3", got)
	}

	if err := s.DeleteWorkspace(f.srcWS.Slug); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	if err := s.PurgeWorkspaceData(f.srcWS.ID); err != nil {
		t.Fatalf("PurgeWorkspaceData: %v", err)
	}

	if got := s.countRows(t,
		`SELECT COUNT(*) FROM item_workspace_moves WHERE source_workspace_id = ? OR target_workspace_id = ?`,
		f.srcWS.ID, f.srcWS.ID); got != 0 {
		t.Errorf("purge left %d rows referencing the purged workspace", got)
	}
	if got := s.countRows(t, `SELECT COUNT(*) FROM item_workspace_moves`); got != 1 {
		t.Errorf("after purge: got %d provenance rows, want 1 (the bystander pair)", got)
	}
}

// TestItemWorkspaceMoves_TargetDeleteCascades pins the asymmetric cascade
// choice: a deleted DESTINATION item takes its provenance row with it (a
// pointer at a vanished item is worse than none), while the SOURCE side
// carries no FK at all — the archived source is precisely the row whose
// pointer must survive.
func TestItemWorkspaceMoves_TargetDeleteCascades(t *testing.T) {
	f := newMoveFixture(t, "Cascade")
	target := f.dest(t, f.dstWS, "Copy")

	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: target.ID,
		ArchivedSource: true, SourceSeq: seqPtr(3), CreatedBy: f.actor,
	})

	// Hard-deleting the SOURCE must leave the row intact. (Per-item hard
	// delete has no product surface today — this asserts the schema choice,
	// not a live code path.)
	if _, err := f.s.db.Exec(f.s.q(`DELETE FROM items WHERE id = ?`), f.source.ID); err != nil {
		t.Fatalf("hard-delete source: %v", err)
	}
	if got := f.s.countRows(t, `SELECT COUNT(*) FROM item_workspace_moves`); got != 1 {
		t.Fatalf("source hard-delete removed the provenance row; got %d rows, want 1", got)
	}

	// Hard-deleting the TARGET must cascade the row away.
	if _, err := f.s.db.Exec(f.s.q(`DELETE FROM items WHERE id = ?`), target.ID); err != nil {
		t.Fatalf("hard-delete target: %v", err)
	}
	if got := f.s.countRows(t, `SELECT COUNT(*) FROM item_workspace_moves`); got != 0 {
		t.Errorf("target hard-delete did not cascade; got %d rows, want 0", got)
	}
}

// TestListArchivedItemWorkspaceMovesBySource covers the narrow, SQL-bounded
// query the moved-to pointer reads (TASK-2359): archived_source rows only,
// newest first, capped. The cap has to be real in SQL rather than applied by
// the caller — otherwise a source with a long tail of plain copies pays to
// load, sort, scan and allocate every one of them on every read of the source,
// and none of them can ever contribute to the result.
func TestListArchivedItemWorkspaceMovesBySource(t *testing.T) {
	f := newMoveFixture(t, "ArchivedOnly")

	// Two plain copies with LATER timestamps than either move, so a query that
	// forgets the archived_source predicate returns them first and the
	// ordering assertions below fail loudly.
	for i, ts := range []string{"2026-05-09T00:00:00Z", "2026-05-10T00:00:00Z"} {
		f.record(t, models.ItemWorkspaceMove{
			SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
			TargetWorkspaceID: f.dstWS.ID,
			TargetItemID:      f.dest(t, f.dstWS, fmt.Sprintf("Copy %d", i)).ID,
			ArchivedSource:    false, CreatedBy: f.actor, CreatedAt: ts,
		})
	}

	older := f.dest(t, f.dstWS, "Older Move")
	newer := f.dest(t, f.dst2WS, "Newer Move")
	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: older.ID,
		ArchivedSource: true, SourceSeq: seqPtr(10),
		CreatedBy: f.actor, CreatedAt: "2026-05-01T00:00:00Z",
	})
	f.record(t, models.ItemWorkspaceMove{
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dst2WS.ID, TargetItemID: newer.ID,
		ArchivedSource: true, SourceSeq: seqPtr(20),
		CreatedBy: f.actor, CreatedAt: "2026-05-02T00:00:00Z",
	})

	got, err := f.s.ListArchivedItemWorkspaceMovesBySource(f.source.ID, 10)
	if err != nil {
		t.Fatalf("ListArchivedItemWorkspaceMovesBySource: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the 2 moves (the 2 copies must be excluded)", len(got))
	}
	if got[0].TargetItemID != newer.ID || got[1].TargetItemID != older.ID {
		t.Errorf("expected newest-first [newer, older], got [%s, %s]", got[0].TargetItemID, got[1].TargetItemID)
	}
	for _, m := range got {
		if !m.ArchivedSource {
			t.Errorf("a plain copy leaked into the archived-only result: %+v", m)
		}
		if m.SourceSeq == nil {
			t.Errorf("archived row lost its source_seq in the dialect round-trip: %+v", m)
		}
	}

	// The cap keeps the newest, which is what a banner most wants.
	capped, err := f.s.ListArchivedItemWorkspaceMovesBySource(f.source.ID, 1)
	if err != nil {
		t.Fatalf("capped lookup: %v", err)
	}
	if len(capped) != 1 || capped[0].TargetItemID != newer.ID {
		t.Fatalf("limit=1 should return the newest move, got %+v", capped)
	}

	// A forgotten bound is the safe answer, not every row.
	for _, limit := range []int{0, -1} {
		none, err := f.s.ListArchivedItemWorkspaceMovesBySource(f.source.ID, limit)
		if err != nil {
			t.Fatalf("limit=%d: %v", limit, err)
		}
		if len(none) != 0 {
			t.Errorf("limit=%d returned %d rows; a non-positive bound must return none", limit, len(none))
		}
	}

	// And the broad forward lookup is untouched — it still sees everything.
	all, err := f.s.ListItemWorkspaceMovesBySource(f.source.ID)
	if err != nil {
		t.Fatalf("forward lookup: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("the broad forward lookup should still return all 4 rows, got %d", len(all))
	}
}

// TestListArchivedItemWorkspaceMovesBySource_SameSecondUsesSourceSeq is the
// archived-only query's copy of DR-2a's decisive case. Two
// archive→restore→move cycles inside one second tie on created_at — that tie
// is the entire reason source_seq exists — so without the seq term the
// ordering falls through to the id tiebreak and returns an arbitrary
// destination. The ids below are fixed so "arbitrary" is deterministic and the
// wrong answer is reproducible rather than a coin flip.
func TestListArchivedItemWorkspaceMovesBySource_SameSecondUsesSourceSeq(t *testing.T) {
	f := newMoveFixture(t, "ArchivedOrder")
	earlier := f.dest(t, f.dstWS, "Earlier Move")
	later := f.dest(t, f.dst2WS, "Later Move")

	const sameSecond = "2026-06-01T12:00:00Z"

	// The LATER move gets the lexically LOWEST id, so an ordering that lost
	// the source_seq term would return `earlier` first.
	f.record(t, models.ItemWorkspaceMove{
		ID:                lexLowMoveID,
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dst2WS.ID, TargetItemID: later.ID,
		ArchivedSource: true, SourceSeq: seqPtr(200),
		CreatedBy: f.actor, CreatedAt: sameSecond,
	})
	f.record(t, models.ItemWorkspaceMove{
		ID:                lexHighMoveID,
		SourceWorkspaceID: f.srcWS.ID, SourceItemID: f.source.ID,
		TargetWorkspaceID: f.dstWS.ID, TargetItemID: earlier.ID,
		ArchivedSource: true, SourceSeq: seqPtr(100),
		CreatedBy: f.actor, CreatedAt: sameSecond,
	})

	got, err := f.s.ListArchivedItemWorkspaceMovesBySource(f.source.ID, 10)
	if err != nil {
		t.Fatalf("ListArchivedItemWorkspaceMovesBySource: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (restore-then-move-again must NOT be deduped)", len(got))
	}
	if got[0].TargetItemID != later.ID {
		t.Errorf("head is %q, want the higher-seq move %q — the ordering is not seq-driven",
			got[0].TargetItemID, later.ID)
	}
	// And the cap keeps the seq-newest one, which is the case the bound
	// actually has to get right.
	capped, err := f.s.ListArchivedItemWorkspaceMovesBySource(f.source.ID, 1)
	if err != nil {
		t.Fatalf("capped lookup: %v", err)
	}
	if len(capped) != 1 || capped[0].TargetItemID != later.ID {
		t.Fatalf("limit=1 should keep the higher-seq move, got %+v", capped)
	}
}

// TestItemWorkspaceMoves_ArchivedSourceIsConstrainedAtRest pins the CHECK that
// makes migration 077 (SQLite) equivalent to 055 (Postgres) at rest.
//
// Postgres' BOOLEAN admits exactly two values; a bare SQLite INTEGER admits
// any. The gap is not cosmetic: a stray 2 scans as true through BoolToInt,
// while the partial index the "moved to" lookup relies on is
// `WHERE archived_source = 1` — so the row reads as a move but is invisible to
// the query that finds moves. That is a state the Postgres schema cannot
// represent, and the dialects must not disagree about it.
//
// Every id below is a REAL row from the fixture. An earlier version of this
// test used placeholder strings and passed against a schema with no CHECK at
// all, because the insert was rejected by the foreign keys long before the
// constraint under test was reached.
func TestItemWorkspaceMoves_ArchivedSourceIsConstrainedAtRest(t *testing.T) {
	f := newMoveFixture(t, "AtRestCheck")
	if f.s.dialect.Driver() == DriverPostgres {
		t.Skip("Postgres enforces this with BOOLEAN; the CHECK exists to match it on SQLite")
	}
	target := f.dest(t, f.dstWS, "Copy")

	_, err := f.s.db.Exec(`
		INSERT INTO item_workspace_moves
			(id, source_workspace_id, source_item_id, target_workspace_id,
			 target_item_id, archived_source, source_seq, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, 2, 1, ?, '2026-01-01T00:00:00Z')
	`, "chk-1", f.srcWS.ID, f.source.ID, f.dstWS.ID, target.ID, f.actor)
	if err == nil {
		t.Fatal("archived_source = 2 was accepted; the CHECK constraint is missing, " +
			"so SQLite can hold a provenance row Postgres cannot represent")
	}

	// Prove the insert is otherwise valid, so the rejection above is the CHECK
	// and not some unrelated constraint.
	if _, err := f.s.db.Exec(`
		INSERT INTO item_workspace_moves
			(id, source_workspace_id, source_item_id, target_workspace_id,
			 target_item_id, archived_source, source_seq, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, 1, 1, ?, '2026-01-01T00:00:00Z')
	`, "chk-2", f.srcWS.ID, f.source.ID, f.dstWS.ID, target.ID, f.actor); err != nil {
		t.Fatalf("the same row with archived_source = 1 must insert cleanly: %v", err)
	}
}
