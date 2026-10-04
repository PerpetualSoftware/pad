package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FencedTx is the only transaction an app write runs in (SPEC-6, DOC-3371
// §4; TASK-3388). It wraps a *sql.Tx that nothing outside this file can
// reach: there is no raw Exec, no free-form query, and no accessor for the
// handle. Every write it will make (U2 onward) is a named method that checks
// the row it writes belongs to one of the install's companion collections,
// or, for a derived row, hangs off a companion item.
//
// Why the store needs a separate door for apps. The human mutation methods
// each open their own transaction and carry workspace-wide side effects
// (title cascades, broken-link resolution, slug probing, relation rebuilds),
// and four review rounds found a new one each time. App writes therefore
// never call them; internal/appstore calls FencedTx methods, and a
// type-resolved boundary test (internal/appstore) fails on anything else.
//
// The fence. BeginFenced's FIRST statement reads the install's epoch and
// state, FOR SHARE on Postgres, and refuses unless the epoch the request was
// admitted under is current and the install is active. Disable, rotate and
// uninstall take the same row FOR UPDATE and bump the epoch, so an app write
// either commits before them or fails inside its own transaction. The shared
// lock is the transaction's first lock and nothing upgrades it. On SQLite
// the DSN's _txlock=immediate makes BEGIN the writer lock, which serializes
// the same way.
//
// READS ARE NOT VISIBILITY-CHECKED HERE. FencedTx offers no free-form query
// (QueryRow would happily run an UPDATE ... RETURNING); the reads it does
// make are fixed ones its own write checks need. What an app may SEE is the
// app API's job: the DTO and handler layer (SPEC-6 U6) applies the
// collection ceiling. Named reads U2 adds here must not be mistaken for a
// visibility check.
//
// Only BeginFenced makes a usable one. Every field is unexported, so no other
// package can write a literal, and a zero value (new, reflect.New) carries no
// transaction and refuses every method.
type FencedTx struct {
	s           *Store
	tx          *sql.Tx
	installID   string
	workspaceID string
	epoch       int64
	companions  map[string]struct{}
	seqLocked   bool
	done        bool
}

// FenceSpec is what a request was admitted under: the install, the epoch its
// token carried, the workspace it addresses and the install's companion
// collection IDs.
type FenceSpec struct {
	InstallID   string
	WorkspaceID string
	Epoch       int64
	Companions  []string
}

// ErrFenceStale: the install is no longer active under the epoch the request
// was admitted with (disabled, rotated, uninstalled, or another workspace).
// Nothing was written.
var ErrFenceStale = errors.New("app install fence: stale epoch or inactive install")

// ErrNotCompanion: a fenced write named a row outside the install's
// companion collections. Nothing was written.
var ErrNotCompanion = errors.New("app install fence: not a companion collection")

// ErrFenceClosed: the FencedTx was already committed or rolled back, or was
// not made by BeginFenced.
var ErrFenceClosed = errors.New("app install fence: transaction finished or never opened")

// unusable reports whether f cannot run work: finished, or not made by
// BeginFenced.
func (f *FencedTx) unusable() bool {
	return f == nil || f.tx == nil || f.s == nil || f.done
}

// BeginFenced opens a FencedTx. It is called only by internal/appstore; a
// test fails if any other package references it.
func (s *Store) BeginFenced(ctx context.Context, spec FenceSpec) (*FencedTx, error) {
	if spec.InstallID == "" || spec.WorkspaceID == "" || spec.Epoch < 1 {
		return nil, ErrFenceStale
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin fenced: %w", err)
	}
	q := `SELECT auth_epoch, state, workspace_id FROM app_installs WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR SHARE`
	}
	var epoch int64
	var state, workspaceID string
	err = tx.QueryRowContext(ctx, s.q(q), spec.InstallID).Scan(&epoch, &state, &workspaceID)
	if err != nil || epoch != spec.Epoch || state != "active" || workspaceID != spec.WorkspaceID {
		_ = tx.Rollback()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("begin fenced: read install: %w", err)
		}
		return nil, ErrFenceStale
	}
	companions := make(map[string]struct{}, len(spec.Companions))
	for _, id := range spec.Companions {
		companions[id] = struct{}{}
	}
	return &FencedTx{s: s, tx: tx, installID: spec.InstallID, workspaceID: spec.WorkspaceID, epoch: epoch, companions: companions}, nil
}

// Commit commits the fenced transaction.
func (f *FencedTx) Commit() error {
	if f.unusable() {
		return ErrFenceClosed
	}
	f.done = true
	return f.tx.Commit()
}

// Rollback abandons the fenced transaction. It is safe to defer after a
// successful Commit.
func (f *FencedTx) Rollback() error {
	if f.unusable() {
		return nil
	}
	f.done = true
	return f.tx.Rollback()
}

// LockWorkspaceSeq takes the workspace lock every human item write takes for
// item_number and seq (insertItemTx, acquireWorkspaceSeqLock), so an app write
// and a human write can never read the same MAX and allocate the same number.
// Equal seq values would make cursor pagination skip items. Idempotent within
// one FencedTx. On SQLite the writer lock already serializes it.
func (f *FencedTx) LockWorkspaceSeq() error {
	if f.unusable() {
		return ErrFenceClosed
	}
	if f.seqLocked {
		return nil
	}
	if err := f.s.acquireWorkspaceSeqLock(f.tx, f.workspaceID); err != nil {
		return err
	}
	f.seqLocked = true
	return nil
}

// NextItemNumber returns the workspace's next item_number, the human create's
// own subquery, read under LockWorkspaceSeq.
func (f *FencedTx) NextItemNumber() (int64, error) {
	if err := f.LockWorkspaceSeq(); err != nil {
		return 0, err
	}
	var n int64
	if err := f.tx.QueryRow(f.s.q(`SELECT COALESCE(MAX(item_number), 0) + 1 FROM items WHERE workspace_id = ?`), f.workspaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("fenced next item_number: %w", err)
	}
	return n, nil
}

// NextSeq returns the workspace's next seq (nextWorkspaceSeqSubquery), read
// under LockWorkspaceSeq.
func (f *FencedTx) NextSeq() (int64, error) {
	if err := f.LockWorkspaceSeq(); err != nil {
		return 0, err
	}
	var n int64
	if err := f.tx.QueryRow(f.s.q(`SELECT COALESCE(MAX(seq), 0) + 1 FROM items WHERE workspace_id = ?`), f.workspaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("fenced next seq: %w", err)
	}
	return n, nil
}

// requireCompanionCollection refuses a write to a collection outside the
// install's companions. Every fenced write method calls it, or
// requireCompanionItem, before executing.
func (f *FencedTx) requireCompanionCollection(collectionID string) error {
	if f.unusable() {
		return ErrFenceClosed
	}
	if _, ok := f.companions[collectionID]; !ok {
		return ErrNotCompanion
	}
	// And the collection as it stands in THIS transaction (TASK-3401 U6c,
	// codex r1): the set above is the request's, from admission, and a
	// companion can stop being one while a request is in flight (deleted, or
	// re-stamped to another install). An upload's body can take minutes, so
	// that window is not academic.
	var live int
	if err := f.tx.QueryRow(f.s.q(`SELECT COUNT(*) FROM collections
		WHERE id = ? AND workspace_id = ? AND via_app = ? AND deleted_at IS NULL`),
		collectionID, f.workspaceID, f.installID).Scan(&live); err != nil {
		return fmt.Errorf("fenced companion check: %w", err)
	}
	if live == 0 {
		return ErrNotCompanion
	}
	return nil
}

// requireCompanionItem resolves a live item in the install's workspace,
// inside the fenced transaction, and refuses unless its CURRENT collection is
// a companion. A derived row (a comment, a version, an activity) is fenced on
// the item it hangs off. It returns the collection for the caller's write.
func (f *FencedTx) requireCompanionItem(itemID string) (string, error) {
	if f.unusable() {
		return "", ErrFenceClosed
	}
	var collectionID string
	err := f.tx.QueryRow(f.s.q(`SELECT collection_id FROM items WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL`),
		itemID, f.workspaceID).Scan(&collectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotCompanion
	}
	if err != nil {
		return "", fmt.Errorf("fenced item lookup: %w", err)
	}
	if err := f.requireCompanionCollection(collectionID); err != nil {
		return "", err
	}
	return collectionID, nil
}
