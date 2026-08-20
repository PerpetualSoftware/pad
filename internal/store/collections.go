package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// CollectionUpdateConflictError is returned by UpdateCollection when the
// caller supplied CollectionUpdate.ExpectedUpdatedAt and it no longer matches
// the collection's current updated_at — another writer changed the row first
// (BUG-2265, optimistic concurrency; mirrors items.go's UpdateConflictError
// for the item path from IDEA-1480/TASK-2022). The check runs under the same
// workspace write lock as the mutation, so a matching timestamp is a genuine
// guarantee that nothing slipped in between. The handler maps this to the same
// pad-structured-error/v1 conflict envelope (HTTP 409, code "update_conflict")
// the item path emits.
type CollectionUpdateConflictError struct {
	CollectionID      string
	ExpectedUpdatedAt string
	ActualUpdatedAt   time.Time
}

func (e *CollectionUpdateConflictError) Error() string {
	return fmt.Sprintf(
		"collection %s was modified by another writer (expected updated_at %s, actual %s)",
		e.CollectionID, e.ExpectedUpdatedAt, e.ActualUpdatedAt.UTC().Format(time.RFC3339),
	)
}

func (s *Store) CreateCollection(workspaceID string, input models.CollectionCreate) (*models.Collection, error) {
	id := newID()
	ts := now()

	schema := input.Schema
	if schema == "" {
		schema = `{"fields":[]}`
	}
	settings := input.Settings
	if settings == "" {
		settings = "{}"
	}
	// Traits default to "{}" (declares nothing), which is correct for every
	// ordinary collection — kernel traits are opt-in and absence is never an
	// error. The column is NOT NULL, so the empty case must be a real object.
	traits := input.Traits
	if strings.TrimSpace(traits) == "" {
		traits = "{}"
	}
	icon := input.Icon
	description := input.Description

	prefix := input.Prefix
	if prefix == "" {
		prefix = collections.DerivePrefix(input.Name)
	}
	if prefix == "" {
		prefix = "ITEM"
	}

	baseSlug := input.Slug
	if baseSlug == "" {
		baseSlug = slugify(input.Name)
	}
	if baseSlug == "" {
		baseSlug = "collection"
	}
	// Avoid slugs that collide with workspace-level UI routes
	if isReservedCollectionSlug(baseSlug) {
		baseSlug = baseSlug + "-collection"
	}
	slug, err := s.uniqueSlug("collections", "workspace_id", workspaceID, baseSlug)
	if err != nil {
		return nil, fmt.Errorf("unique slug: %w", err)
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO collections (id, workspace_id, name, slug, prefix, icon, description, schema, settings, traits, sort_order, is_default, is_system, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, workspaceID, input.Name, slug, prefix, icon, description, schema, settings, traits, 0, s.dialect.BoolToInt(input.IsDefault), s.dialect.BoolToInt(input.IsSystem), ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert collection: %w", err)
	}

	return s.GetCollection(id)
}

// collectionColumns is the ONE full-row projection of the collections table.
// Every accessor that hydrates a COMPLETE models.Collection from a single row
// selects exactly this list, in this order, and scans it through
// scanCollectionRow — so adding a stored column to those accessors is a single
// edit rather than a hunt for copies that silently drift apart (TASK-2368).
//
// SCOPE: the single-row accessors — GetCollection, GetCollectionAnyState, and
// the transactional getCollectionInWorkspaceTx. ListCollections is
// deliberately NOT folded in and remains a separate projection: it is an
// aggregate multi-row query with table-aliased columns, a trailing
// COUNT(i.id), and no deleted_at (its WHERE already excludes deleted rows).
// Sharing a list across those shapes would need a second, count-aware scanner
// and would reshape a hot aggregate for no correctness gain.
//
// SO IF YOU ARE ADDING A STORED COLLECTION COLUMN, this list is one of four
// edits, not the only one. The others are ListCollections (above), and
// ExportWorkspace / ImportWorkspace in export.go, which carry their own
// projection over models.CollectionExport — a portability format, deliberately
// not this model: it omits workspace_id and deleted_at because an import
// assigns a fresh workspace and an export skips deleted rows. A column added
// here but not there hydrates everywhere and still vanishes on export/import.
const collectionColumns = `id, workspace_id, name, slug, prefix, icon, description, schema, settings, traits, sort_order, is_default, is_system, created_at, updated_at, deleted_at`

// collectionSelect is the shared prefix each single-row accessor completes
// with its own predicate. Assembled from constants, so the full statement is
// built at COMPILE time — no per-call concatenation on GetCollection's hot
// path, and no runtime-assembled SQL fragment for s.q to rewrite.
const collectionSelect = `SELECT ` + collectionColumns + ` FROM collections WHERE `

const (
	getCollectionQuery            = collectionSelect + `id = ? AND deleted_at IS NULL`
	getCollectionAnyStateQuery    = collectionSelect + `id = ?`
	getCollectionInWorkspaceQuery = collectionSelect + `id = ? AND workspace_id = ? AND deleted_at IS NULL`
)

// scanCollectionRow reads one collection row through q and hydrates the model.
// The WHERE predicate is the only per-caller difference — scope and
// deleted-state are the callers' decisions, and for the transactional copy
// path the workspace scope is a security boundary, not a hint. Callers pass a
// full constant statement rather than a fragment, so there is no dynamic SQL
// here for a future caller to interpolate into.
//
// Parameterized over rowQueryer (the uniqueSlugQ / validateAssignmentScopeQ
// pattern from TASK-2362) so the same read runs against *sql.DB or inside a
// caller's *sql.Tx, under the locks that transaction already holds. The read
// executes on whatever q is given and never falls back to s.db, so it cannot
// escape the caller's transaction — but rowQueryer accepts both, so CHOOSING
// the wrong one is a semantic error the compiler will not catch. A caller that
// needs the read under its own locks must pass its tx; getCollectionInWorkspaceTx
// is the existing example, and its *sql.Tx signature is what enforces that at
// its call sites.
//
// s.q rewrites the placeholders for the active dialect here, so every caller
// gets that for free and none may skip it.
//
// A MISS IS NOT AN ERROR: sql.ErrNoRows returns (nil, nil), matching every
// caller's contract. Real failures are returned unwrapped so each accessor can
// apply its own distinct error prefix.
func (s *Store) scanCollectionRow(q rowQueryer, query string, args ...any) (*models.Collection, error) {
	var c models.Collection
	var createdAt, updatedAt string
	var deletedAt *string
	var isDefault bool

	err := q.QueryRow(s.q(query), args...).Scan(
		&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Prefix, &c.Icon, &c.Description,
		&c.Schema, &c.Settings, &c.Traits, &c.SortOrder, &isDefault, &c.IsSystem,
		&createdAt, &updatedAt, &deletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	c.IsDefault = isDefault
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	c.DeletedAt = parseTimePtr(deletedAt)
	return &c, nil
}

func (s *Store) GetCollection(id string) (*models.Collection, error) {
	c, err := s.scanCollectionRow(s.db, getCollectionQuery, id)
	if err != nil {
		return nil, fmt.Errorf("get collection: %w", err)
	}
	return c, nil
}

// GetCollectionAnyState is GetCollection without the
// `deleted_at IS NULL` filter — used by the open-children guard
// (IDEA-1494 R3 P3) so a child still attached to a soft-deleted
// collection is evaluated against ITS collection's actual done-field
// schema instead of falling back to the default `status` terminal
// list (which would mis-classify children of custom-done-field
// collections as non-terminal and produce false blockers).
//
// Mirrors the inclusion rule baked into childrenDoneFiltersForParent /
// doneFiltersForWorkspace, both of which already include soft-deleted
// collections for exactly this reason.
func (s *Store) GetCollectionAnyState(id string) (*models.Collection, error) {
	c, err := s.scanCollectionRow(s.db, getCollectionAnyStateQuery, id)
	if err != nil {
		return nil, fmt.Errorf("get collection (any state): %w", err)
	}
	return c, nil
}

// ArchivedCollectionClaimsSlug reports whether a SOFT-DELETED collection in
// the workspace holds this exact slug.
//
// Exists so slug ALIAS resolution can refuse to fall through to a different
// collection when the caller's exact name is taken by an archived one
// (BUG-2578). It answers a boolean rather than returning the row on purpose:
// an archived collection is never a valid target, it only blocks the name.
func (s *Store) ArchivedCollectionClaimsSlug(workspaceID, slug string) (bool, error) {
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM collections
		WHERE workspace_id = ? AND slug = ? AND deleted_at IS NOT NULL
	`), workspaceID, slug).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("archived collection claims slug: %w", err)
	}
	return n > 0, nil
}

func (s *Store) GetCollectionBySlug(workspaceID, slug string) (*models.Collection, error) {
	var id string
	err := s.db.QueryRow(s.q(`
		SELECT id FROM collections
		WHERE workspace_id = ? AND slug = ? AND deleted_at IS NULL
	`), workspaceID, slug).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get collection by slug: %w", err)
	}
	return s.GetCollection(id)
}

// ListCollectionsMinimal returns collection rows populated with just the
// fields needed for done-detection context and slug lookups: ID, Slug,
// Schema, Settings. Skips the per-collection COUNT queries that
// ListCollections runs, which matters on hot paths that only need those
// fields (e.g. handlers that build a ctxMap for isItemDone or an ID→slug
// map). Includes soft-deleted collections so items still attached to them
// can be evaluated.
func (s *Store) ListCollectionsMinimal(workspaceID string) ([]models.Collection, error) {
	rows, err := s.db.Query(
		s.q(`SELECT id, slug, schema, settings FROM collections WHERE workspace_id = ?`),
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list collections minimal: %w", err)
	}
	defer rows.Close()
	var result []models.Collection
	for rows.Next() {
		var c models.Collection
		if err := rows.Scan(&c.ID, &c.Slug, &c.Schema, &c.Settings); err != nil {
			return nil, fmt.Errorf("scan collection minimal: %w", err)
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// ListTraitedCollections returns every live collection in the workspace paired
// with its parsed kernel traits (SPEC-5). This is the lookup that replaced
// slug literals: a consumer asks which collection DECLARES a behavior rather
// than naming "conventions" or "playbooks", so the behavior survives a rename
// ([[BUG-2702]]).
//
// A collection whose traits blob fails to parse is returned with EMPTY traits
// rather than failing the whole call. One malformed declaration must not take
// down bootstrap, export, or seeding for the entire workspace — it degrades to
// "this collection declares nothing", which is the same as the pre-trait
// behavior for any collection that isn't conventions or playbooks. Declarations
// are validated on the way IN (create/update/seed), so a stored blob that
// doesn't parse means something wrote around those gates. TASK-2657.
func (s *Store) ListTraitedCollections(workspaceID string) ([]collections.TraitedCollection, error) {
	rows, err := s.db.Query(
		s.q(`SELECT id, slug, traits FROM collections WHERE workspace_id = ? AND deleted_at IS NULL ORDER BY sort_order ASC, created_at ASC`),
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list traited collections: %w", err)
	}
	defer rows.Close()
	var out []collections.TraitedCollection
	for rows.Next() {
		var id, slug, raw string
		if err := rows.Scan(&id, &slug, &raw); err != nil {
			return nil, fmt.Errorf("scan traited collection: %w", err)
		}
		traits, perr := models.ParseCollectionTraits(raw)
		if perr != nil {
			traits = models.CollectionTraits{}
		}
		out = append(out, collections.TraitedCollection{ID: id, Slug: slug, Traits: traits})
	}
	return out, rows.Err()
}

func (s *Store) ListCollections(workspaceID string) ([]models.Collection, error) {
	rows, err := s.db.Query(s.q(`
		SELECT c.id, c.workspace_id, c.name, c.slug, c.prefix, c.icon, c.description,
		       c.schema, c.settings, c.traits, c.sort_order, c.is_default, c.is_system, c.created_at, c.updated_at,
		       COUNT(i.id) as item_count
		FROM collections c
		LEFT JOIN items i ON i.collection_id = c.id AND i.deleted_at IS NULL
		WHERE c.workspace_id = ? AND c.deleted_at IS NULL
		GROUP BY c.id
		ORDER BY c.sort_order ASC, c.created_at ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()

	var result []models.Collection
	for rows.Next() {
		var c models.Collection
		var createdAt, updatedAt string
		var isDefault bool
		if err := rows.Scan(
			&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Prefix, &c.Icon, &c.Description,
			&c.Schema, &c.Settings, &c.Traits, &c.SortOrder, &isDefault, &c.IsSystem,
			&createdAt, &updatedAt, &c.ItemCount,
		); err != nil {
			return nil, err
		}
		c.IsDefault = isDefault
		c.CreatedAt = parseTime(createdAt)
		c.UpdatedAt = parseTime(updatedAt)
		result = append(result, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Compute active_item_count per collection using that collection's own
	// done-field + terminal options from its schema + settings (not the
	// global default list, and not hardcoded to `status`). See TASK-604:
	// collections whose board is grouped by e.g. `resolution` have
	// done-detection follow that field naturally.
	for idx := range result {
		c := &result[idx]
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(c.Schema), &schema); err != nil {
			schema = models.CollectionSchema{}
		}
		var settings models.CollectionSettings
		if c.Settings != "" {
			_ = json.Unmarshal([]byte(c.Settings), &settings)
		}
		doneKey, termPlaceholders, termArgs := models.TerminalPlaceholdersForDoneField(schema, settings)
		jsonExtractDone := s.dialect.JSONExtractText("i.fields", doneKey)
		args := append([]any{c.ID}, termArgs...)
		err := s.db.QueryRow(s.q(fmt.Sprintf(`
			SELECT COUNT(*) FROM items i
			WHERE i.collection_id = ? AND i.deleted_at IS NULL
			AND LOWER(COALESCE(%s, '')) NOT IN (%s)
		`, jsonExtractDone, termPlaceholders)), args...).Scan(&c.ActiveItemCount)
		if err != nil {
			return nil, fmt.Errorf("count active items for collection %s: %w", c.Slug, err)
		}
	}

	return result, nil
}

func (s *Store) UpdateCollection(id string, input models.CollectionUpdate) (*models.Collection, error) {
	existing, err := s.GetCollection(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	// Sub-second timestamp (BUG-2265): collections.updated_at doubles as the
	// concurrency token, so it must differ between two writes in the same
	// wall-clock second. nowNano() makes that near-certain without a schema
	// change (the column is TEXT on both dialects and never lexically compared
	// — only via time.Equal and for display).
	ts := nowNano()
	sets := []string{"updated_at = ?"}
	args := []interface{}{ts}

	if input.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *input.Name)
		// Update slug too
		baseSlug := slugify(*input.Name)
		if baseSlug == "" {
			baseSlug = "collection"
		}
		if isReservedCollectionSlug(baseSlug) {
			baseSlug = baseSlug + "-collection"
		}
		newSlug, err := s.uniqueSlugExcluding("collections", "workspace_id", existing.WorkspaceID, baseSlug, id)
		if err != nil {
			return nil, fmt.Errorf("unique slug: %w", err)
		}
		sets = append(sets, "slug = ?")
		args = append(args, newSlug)
	}
	if input.Prefix != nil {
		sets = append(sets, "prefix = ?")
		args = append(args, *input.Prefix)
	}
	if input.Icon != nil {
		sets = append(sets, "icon = ?")
		args = append(args, *input.Icon)
	}
	if input.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *input.Description)
	}
	if input.Schema != nil {
		sets = append(sets, "schema = ?")
		args = append(args, *input.Schema)
	}
	// Traits are updated ONLY when explicitly supplied. Every existing client
	// rebuilds schema/settings without knowing traits exist, so a nil here is
	// overwhelmingly "this caller doesn't know about traits" rather than
	// "clear them" — and treating it as a clear would silently disarm the
	// kernel behaviors a collection declares. Clearing is still reachable:
	// send an explicit "{}". TASK-2657.
	if input.Traits != nil {
		traits := strings.TrimSpace(*input.Traits)
		if traits == "" {
			traits = "{}"
		}
		sets = append(sets, "traits = ?")
		args = append(args, traits)
	}
	if input.Settings != nil {
		// Normalize the empty-string sentinel to a valid JSON object before
		// writing. The NOT NULL DEFAULT '{}' constraint (IDEA-1484) only
		// fires when the UPDATE omits the column; explicit values are
		// written verbatim. Postgres rejects `""` at JSONB type-validation;
		// SQLite would silently store invalid JSON. Same boundary
		// normalization as ImportWorkspace.
		settings := *input.Settings
		if settings == "" {
			settings = "{}"
		}
		sets = append(sets, "settings = ?")
		args = append(args, settings)
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE collections SET %s WHERE id = ?", strings.Join(sets, ", "))

	// BUG-2265: collections.updated_at doubles as the optimistic-concurrency
	// token (no schema migration — it's the existing column). For that to be
	// reliable it must satisfy two invariants that a plain
	// `UPDATE ... SET updated_at = now()` doesn't, so EVERY update (guarded or
	// not) runs through one small transaction that re-reads the row and derives
	// the new timestamp atomically:
	//
	//   1. Serialized check-and-set. The re-read happens under a lock, so a
	//      concurrent writer can't slip between it and the UPDATE. SQLite gets
	//      this from the db-wide BEGIN IMMEDIATE write lock (_txlock=immediate);
	//      Postgres needs an explicit `FOR UPDATE` row lock (it rejects `FOR
	//      UPDATE` on SQLite syntactically, hence the driver gate). This also
	//      closes the tokenless-writer race the advisory lock could NOT (an
	//      advisory lock only serializes writers that also take it — Codex P1).
	//
	//   2. Strictly monotonic token. Sub-second nowNano() makes same-second
	//      collisions near-impossible, but a coarse platform clock (or an NTP
	//      step-back) could still return a value <= the row's current one, which
	//      would let a tokenless or racing write keep/regress the token and a
	//      stale reader clobber newer data. Guard it: when the fresh timestamp
	//      doesn't already exceed the row's current value, advance by a single
	//      NANOSECOND past it. That keeps the token strictly increasing on every
	//      writer while bounding any drift to nanoseconds — no meaningful
	//      advance-into-the-future even under sustained writes.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// LOCK ORDER (Codex P1 deadlock fix). When this update also migrates item
	// field values it locks TWO things: the workspace advisory/seq lock (for
	// the per-row seq bumps) and the collection ROW (FOR UPDATE, below). Item
	// creation locks the same pair in the order [workspace advisory lock
	// (tryCreateItem) → collection-row FK key-share lock (on INSERT)], so we
	// MUST take the workspace lock FIRST here too — grabbing the row lock first
	// would ABBA-deadlock against a concurrent item-create. Acquired only on the
	// migration path (the only path that touches the workspace seq lock); a
	// plain update just takes the row lock and can't deadlock. On SQLite both
	// are no-ops under the single BEGIN IMMEDIATE write lock.
	if len(input.Migrations) > 0 {
		if err := s.acquireWorkspaceSeqLock(tx, existing.WorkspaceID); err != nil {
			return nil, err
		}
	}

	reread := "SELECT updated_at FROM collections WHERE id = ? AND deleted_at IS NULL"
	if s.dialect.Driver() == DriverPostgres {
		reread += " FOR UPDATE"
	}
	var currentUpdatedAt string
	rerr := tx.QueryRow(s.q(reread), id).Scan(&currentUpdatedAt)
	if rerr == sql.ErrNoRows {
		// Deleted between the pre-tx GetCollection and here — treat as not-found.
		return nil, nil
	}
	if rerr != nil {
		return nil, fmt.Errorf("re-read collection under lock: %w", rerr)
	}
	current := parseTime(currentUpdatedAt)

	// Optimistic-concurrency guard — only when the caller opted in by sending
	// the token it last read. Compared with time.Equal (format-agnostic).
	if input.ExpectedUpdatedAt != "" {
		expected, perr := time.Parse(time.RFC3339, input.ExpectedUpdatedAt)
		if perr != nil {
			return nil, fmt.Errorf("invalid expected_updated_at %q: %w", input.ExpectedUpdatedAt, perr)
		}
		if !current.Equal(expected) {
			return nil, &CollectionUpdateConflictError{
				CollectionID:      id,
				ExpectedUpdatedAt: input.ExpectedUpdatedAt,
				ActualUpdatedAt:   current,
			}
		}
	}

	// Monotonic advance (invariant 2 above). args[0] is the `updated_at = ?`
	// bind built first, so overwrite it in place. A one-nanosecond step keeps
	// the token strictly increasing without drifting meaningfully ahead of
	// wall-clock.
	if !parseTime(ts).After(current) {
		args[0] = current.Add(time.Nanosecond).UTC().Format(time.RFC3339Nano)
	}

	if _, err := tx.Exec(s.q(query), args...); err != nil {
		return nil, fmt.Errorf("update collection: %w", err)
	}

	// Apply field-value migrations (select-option renames) in the SAME tx as
	// the schema change (BUG-2265 Codex): a migration failure rolls back the
	// schema AND the concurrency-token advance, so the row is untouched and the
	// caller's retry works cleanly — no committed-schema/stale-items split and
	// no guaranteed-409. The workspace seq lock was already taken above (before
	// the row lock) to preserve the item-create lock order.
	if len(input.Migrations) > 0 {
		if _, err := s.applyFieldMigrationsTx(tx, id, existing.WorkspaceID, input.Migrations); err != nil {
			return nil, fmt.Errorf("apply field migrations: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit collection update: %w", err)
	}

	return s.GetCollection(id)
}

// DeleteCollection soft-deletes a collection by id. When expectedUpdatedAt is
// non-empty it opts into optimistic-concurrency control (BUG-2265): the row's
// updated_at is re-read UNDER A LOCK and the delete is rejected with a
// CollectionUpdateConflictError when it no longer matches. This closes the
// archive TOCTOU where the handler resolved a collection by its (mutable) slug
// but a concurrent RENAME re-owned that slug with a DIFFERENT collection before
// the delete landed — the token mismatch 409s instead of archiving the wrong
// collection. Empty token keeps the legacy unconditional soft-delete (CLI/API
// callers that don't opt in).
func (s *Store) DeleteCollection(id string, expectedUpdatedAt string) error {
	// Check if it's a default collection
	var isDefault bool
	err := s.db.QueryRow(s.q("SELECT is_default FROM collections WHERE id = ? AND deleted_at IS NULL"), id).Scan(&isDefault)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("check collection: %w", err)
	}
	if isDefault {
		return fmt.Errorf("cannot delete default collection")
	}

	ts := now()

	if expectedUpdatedAt != "" {
		expected, perr := time.Parse(time.RFC3339, expectedUpdatedAt)
		if perr != nil {
			return fmt.Errorf("invalid expected_updated_at %q: %w", expectedUpdatedAt, perr)
		}
		tx, terr := s.db.Begin()
		if terr != nil {
			return terr
		}
		defer tx.Rollback()

		reread := "SELECT updated_at FROM collections WHERE id = ? AND deleted_at IS NULL"
		if s.dialect.Driver() == DriverPostgres {
			reread += " FOR UPDATE"
		}
		var currentUpdatedAt string
		rerr := tx.QueryRow(s.q(reread), id).Scan(&currentUpdatedAt)
		if rerr == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		if rerr != nil {
			return fmt.Errorf("re-read collection under lock: %w", rerr)
		}
		if actual := parseTime(currentUpdatedAt); !actual.Equal(expected) {
			return &CollectionUpdateConflictError{
				CollectionID:      id,
				ExpectedUpdatedAt: expectedUpdatedAt,
				ActualUpdatedAt:   actual,
			}
		}
		if _, err := tx.Exec(s.q(`UPDATE collections SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`), ts, ts, id); err != nil {
			return fmt.Errorf("delete collection: %w", err)
		}
		return tx.Commit()
	}

	result, err := s.db.Exec(s.q(`
		UPDATE collections SET deleted_at = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
	`), ts, ts, id)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MigrateItemFieldValues bulk-updates items in a collection when select
// options are renamed. Each entry in renames maps old_value → new_value
// for the given field key.
//
// Each affected row gets its OWN fresh seq (no two share the same
// value) so /items-changes pagination is correct even when a
// rename touches more rows than the page limit. We do this with a
// per-row UPDATE loop: an earlier single-statement bulk UPDATE gave
// every row the same MAX(seq)+1, which broke the cursor contract —
// `limit=N` could cut through an equal-seq group, the cursor would
// advance to that shared seq, and the next `seq > cursor` poll
// would permanently miss the remaining rows in the group (Codex
// review of TASK-1354 round 2 [P1]).
//
// Trade-off: O(N) statements instead of O(1) for the bulk path.
// A schema-option rename is an admin one-off, so even a few
// thousand rows is acceptable (~1s/1000 rows on a warm SQLite
// connection). If a future use case demands a higher row budget,
// switch to an UPDATE..FROM with a ROW_NUMBER() CTE that assigns
// sequential per-row seqs in a single statement.
func (s *Store) MigrateItemFieldValues(collectionID string, migrations []models.FieldMigration) (int64, error) {
	if len(migrations) == 0 {
		return 0, nil
	}

	// Look up the workspace for advisory locking + scoping the seq
	// subquery. If the collection has vanished out from under the
	// caller we can short-circuit.
	var workspaceID string
	if err := s.db.QueryRow(s.q(`SELECT workspace_id FROM collections WHERE id = ?`), collectionID).Scan(&workspaceID); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("lookup workspace for migrate: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return 0, err
	}

	totalAffected, err := s.applyFieldMigrationsTx(tx, collectionID, workspaceID, migrations)
	if err != nil {
		return totalAffected, err
	}

	if err := tx.Commit(); err != nil {
		return totalAffected, err
	}
	return totalAffected, nil
}

// applyFieldMigrationsTx runs the per-row field-value migration loop on an
// EXISTING transaction. Extracted so UpdateCollection can run the schema
// change and its item migrations ATOMICALLY in one tx (BUG-2265 Codex: a
// migration failure must roll back the schema AND the concurrency token, or a
// 500 leaves the row changed and every client retry blind-409s). Callers must
// already hold the workspace seq lock (acquireWorkspaceSeqLock) so the per-row
// seq bumps serialize on Postgres.
func (s *Store) applyFieldMigrationsTx(tx *sql.Tx, collectionID, workspaceID string, migrations []models.FieldMigration) (int64, error) {
	ts := now()
	var totalAffected int64

	// TASK-2658: this loop rewrites items.fields on every row carrying the old
	// option value — a real change to item state that consumers cache — and it
	// emitted nothing before. It is a SINGLE-TRANSACTION bulk mutation, so it
	// gets one in-tx item.bulk_updated rather than per-row item.updated: a user
	// renaming one select option must not arrive at a webhook consumer as
	// hundreds of deliveries (TASK-1668 / SPEC-3 v1.1).
	var touchedIDs []string
	var renames []map[string]any

	for _, m := range migrations {
		for oldVal, newVal := range m.RenameOptions {
			if oldVal == newVal {
				continue
			}
			jsonSet := s.dialect.JSONSet("fields", m.Field)
			jsonExtract := s.dialect.JSONExtractText("fields", m.Field)

			// Step 1: find all matching item IDs. SELECT inside the
			// same transaction as the subsequent UPDATEs, so we
			// observe a consistent snapshot of who needs to migrate.
			idRows, err := tx.Query(s.q(fmt.Sprintf(`
				SELECT id FROM items
				WHERE collection_id = ?
				  AND %s = ?
				  AND deleted_at IS NULL
			`, jsonExtract)), collectionID, oldVal)
			if err != nil {
				return totalAffected, fmt.Errorf("migrate field %s (%s → %s) list: %w", m.Field, oldVal, newVal, err)
			}
			var ids []string
			for idRows.Next() {
				var id string
				if err := idRows.Scan(&id); err != nil {
					idRows.Close()
					return totalAffected, fmt.Errorf("migrate field %s scan: %w", m.Field, err)
				}
				ids = append(ids, id)
			}
			if err := idRows.Err(); err != nil {
				idRows.Close()
				return totalAffected, fmt.Errorf("migrate field %s rows: %w", m.Field, err)
			}
			idRows.Close()

			// Step 2: update each row individually. Each UPDATE is
			// a separate statement, so MAX(seq) advances between
			// them inside the transaction — every row ends up with
			// a unique sequential seq.
			updateSQL := s.q(fmt.Sprintf(`
				UPDATE items
				SET fields = %s,
				    updated_at = ?,
				    seq = `+nextWorkspaceSeqSubquery+`
				WHERE id = ?
			`, jsonSet))
			for _, id := range ids {
				result, err := tx.Exec(updateSQL, newVal, ts, workspaceID, id)
				if err != nil {
					return totalAffected, fmt.Errorf("migrate field %s (%s → %s) row %s: %w", m.Field, oldVal, newVal, id, err)
				}
				n, _ := result.RowsAffected()
				totalAffected += n
				if n > 0 {
					touchedIDs = append(touchedIDs, id)
				}
			}
			if len(ids) > 0 {
				renames = append(renames, map[string]any{
					"field": m.Field,
					"from":  oldVal,
					"to":    newVal,
				})
			}
		}
	}

	// Snapshots are read AFTER every UPDATE in this transaction, so a row
	// touched by two migrations in one call is carried once, in its final
	// state, rather than twice in intermediate ones.
	// Report ZERO on failure, not totalAffected. Every error out of this
	// function rolls the caller's transaction back, so the row count describes
	// writes that did not commit — a caller observing (N, err) would be reading
	// a number for changes that never happened (Codex round 4).
	members, err := s.itemSnapshotsTx(tx, touchedIDs)
	if err != nil {
		return 0, err
	}
	if err := s.emitBulkItemEventTx(tx, workspaceID, members, map[string]any{
		"kind":    "field_option_renamed",
		"renames": renames,
	}); err != nil {
		return 0, err
	}

	return totalAffected, nil
}

// SeedDefaultCollections rescues a workspace that ended up with zero
// collections by seeding it with the standard Software-shape Defaults().
// It is intentionally a no-op for workspaces that already have any
// collections — those have an established shape (from a template or
// user edits) that the rescue must not clobber.
//
// No longer called automatically at server startup (removed in
// IDEA-1479); preserved as a building block for any future explicit
// rescue command or migration that wants this behavior.
func (s *Store) SeedDefaultCollections(workspaceID string) error {
	// Use a direct COUNT query rather than ListCollectionsMinimal: the
	// rescue gate only needs to know whether ANY collection exists, and
	// the minimal lister's SELECT touches JSON/JSONB columns whose
	// COALESCE expression doesn't round-trip cleanly on Postgres
	// (BUG triggered in the IDEA-1479 PR's first Postgres CI run).
	// COUNT(*) sidesteps that path entirely and is also cheaper.
	var existing int
	if err := s.db.QueryRow(
		s.q(`SELECT COUNT(*) FROM collections WHERE workspace_id = ?`),
		workspaceID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("check existing collections for rescue seed: %w", err)
	}
	if existing > 0 {
		// Workspace already has collections — its shape was set by a
		// template (default, blank, hiring, etc.) or by manual user
		// edits. Either way, the rescue path doesn't apply.
		return nil
	}
	return s.SeedCollectionsFromTemplate(workspaceID, "")
}

// SeedCollectionsFromTemplate seeds the workspace with collections from the
// named template. An empty template name materializes the default collections
// without any seed items or starter pack — this preserves backward
// compatibility for callers that don't opt into a template. An explicit
// template name (including "startup") additionally seeds the template's
// SeedItems, Conventions, and Playbooks as items in the new workspace.
//
// Seeding is idempotent per-item by title: existing collections are skipped,
// and existing items (matched by title within their target collection) are
// skipped. That design lets the server's startup auto-upgrade re-run safely
// AND lets a partial init (e.g. DB error after some items were seeded) be
// recovered by simply retrying — the retry fills in missing items instead
// of stopping at the first "collection already exists" signal.
func (s *Store) SeedCollectionsFromTemplate(workspaceID string, templateName string) error {
	var defs []collections.DefaultCollection
	var seedItems []collections.SeedItem
	var seedConventions []collections.SeedConvention
	var seedPlaybooks []collections.SeedPlaybook

	if templateName == "" {
		defs = collections.Defaults()
		// Empty template = backward-compatible, no starter pack.
	} else {
		tmpl := collections.GetTemplate(templateName)
		if tmpl == nil {
			return fmt.Errorf("unknown workspace template: %s", templateName)
		}
		defs = tmpl.Collections
		seedItems = tmpl.SeedItems
		seedConventions = tmpl.Conventions
		seedPlaybooks = tmpl.Playbooks
	}

	for _, def := range defs {
		existing, err := s.GetCollectionBySlug(workspaceID, def.Slug)
		if err != nil {
			return fmt.Errorf("check existing collection %s: %w", def.Slug, err)
		}
		if existing != nil {
			continue
		}

		schemaJSON, err := json.Marshal(def.Schema)
		if err != nil {
			return fmt.Errorf("marshal schema for %s: %w", def.Slug, err)
		}
		settingsJSON, err := json.Marshal(def.Settings)
		if err != nil {
			return fmt.Errorf("marshal settings for %s: %w", def.Slug, err)
		}
		// Validate the template's own trait declarations on the way in. A
		// malformed declaration in first-party template code would otherwise
		// seed a workspace whose kernel behaviors silently never fire — the
		// exact failure mode traits exist to remove. Fail at seed time
		// instead (SPEC-0 L6, fail loud). TASK-2657.
		if err := def.Traits.Validate(); err != nil {
			return fmt.Errorf("invalid traits for %s: %w", def.Slug, err)
		}
		traitsJSON, err := def.Traits.JSON()
		if err != nil {
			return fmt.Errorf("marshal traits for %s: %w", def.Slug, err)
		}

		_, err = s.CreateCollection(workspaceID, models.CollectionCreate{
			Name:        def.Name,
			Slug:        def.Slug,
			Prefix:      def.Prefix,
			Icon:        def.Icon,
			Description: def.Description,
			Schema:      string(schemaJSON),
			Settings:    string(settingsJSON),
			Traits:      traitsJSON,
			IsDefault:   true,
			IsSystem:    def.IsSystem,
		})
		if err != nil {
			return fmt.Errorf("create default collection %s: %w", def.Slug, err)
		}
	}

	// existingTitles caches the set of item titles already present in a
	// collection so repeated seed calls against the same collection don't
	// re-query. Lazily populated on first use per slug.
	existingTitles := make(map[string]map[string]bool)

	// seedItem inserts a seed item if no item with the same title already
	// exists in the target collection. Missing target collections (a
	// template-authoring mistake) are silently skipped; real DB errors are
	// propagated so callers can detect partial init failures and retry.
	seedItem := func(collSlug, title, content, fields string) error {
		coll, err := s.GetCollectionBySlug(workspaceID, collSlug)
		if err != nil {
			return fmt.Errorf("lookup %s collection for seeding %q: %w", collSlug, title, err)
		}
		if coll == nil {
			return nil
		}

		titles, ok := existingTitles[collSlug]
		if !ok {
			items, err := s.ListItems(workspaceID, models.ItemListParams{CollectionSlug: collSlug})
			if err != nil {
				return fmt.Errorf("list existing items in %s: %w", collSlug, err)
			}
			titles = make(map[string]bool, len(items))
			for _, it := range items {
				titles[it.Title] = true
			}
			existingTitles[collSlug] = titles
		}
		if titles[title] {
			return nil // already seeded (idempotent + retry-safe)
		}

		_, err = s.CreateItem(workspaceID, coll.ID, models.ItemCreate{
			Title:     title,
			Content:   content,
			Fields:    fields,
			CreatedBy: "system",
			Source:    "template",
		})
		if err != nil {
			return fmt.Errorf("seed item %q in %s: %w", title, collSlug, err)
		}
		titles[title] = true
		return nil
	}

	// Sample items
	for _, item := range seedItems {
		if err := seedItem(item.CollectionSlug, item.Title, item.Content, item.Fields); err != nil {
			return err
		}
	}
	// Starter conventions and playbooks route by TRAIT, not by slug: the
	// destination is whichever collection declares it holds items of that
	// artifact kind. A template that renames its conventions collection still
	// gets its starter pack seeded into the right place. TASK-2657.
	traited, err := s.ListTraitedCollections(workspaceID)
	if err != nil {
		return fmt.Errorf("resolve collection traits for seeding: %w", err)
	}
	conventionsSlug := ""
	if c := collections.FindByArtifactKind(traited, string(artifact.KindConvention)); c != nil {
		conventionsSlug = c.Slug
	}
	playbooksSlug := ""
	if c := collections.FindByArtifactKind(traited, string(artifact.KindPlaybook)); c != nil {
		playbooksSlug = c.Slug
	}

	// Starter conventions
	if conventionsSlug != "" {
		for _, conv := range seedConventions {
			if err := seedItem(conventionsSlug, conv.Title, conv.Content, conv.Fields); err != nil {
				return err
			}
		}
	}
	// Starter playbooks
	if playbooksSlug != "" {
		for _, pb := range seedPlaybooks {
			if err := seedItem(playbooksSlug, pb.Title, pb.Content, pb.Fields); err != nil {
				return err
			}
		}
	}

	// Universal onboard playbook (PLAN-1496 / TASK-1500). Auto-seeded
	// into every workspace created via a real template — including
	// `blank` (where it's the only payload) and the opinionated
	// software/people templates (where it complements the seeded
	// starter pack with the "/pad onboard" entry point for further
	// adaptation).
	//
	// Skipped when templateName == "" — that's the explicit
	// backward-compat escape hatch for tests and direct API callers
	// who want a bare workspace with default system collections and
	// NO seeded content. cmd/pad/init.go ALWAYS supplies a non-empty
	// template (interactive picker or defaultTemplateName), so real
	// user-facing workspace creation always lands here.
	//
	// Seeded last so any future template that ships its own
	// "Onboard a workspace"-titled playbook can take precedence —
	// the seedItem helper is idempotent by title inside a collection.
	if templateName != "" && playbooksSlug != "" {
		onboardSeed := collections.OnboardSeedPlaybook()
		if err := seedItem(playbooksSlug, onboardSeed.Title, onboardSeed.Content, onboardSeed.Fields); err != nil {
			return err
		}
	}

	return nil
}

// reservedCollectionSlugs are workspace-level UI route paths that must not
// be used as collection slugs, to avoid routing collisions.
var reservedCollectionSlugs = map[string]bool{
	"settings": true,
	"activity": true,
	"roles":    true,
	"starred":  true,
	"tags":     true, // Tag pages: /{ws}/tags and /{ws}/tags/{tag} (PLAN-1652)
	"library":  true,
	"insights": true, // Insights analytics page route (PLAN-1628 / TASK-1633)
	"graph":    true, // 3D workspace graph page route (PLAN-1730 / TASK-1733)
	"new":      true,
	// "ref" is reserved for the cross-workspace wiki-link resolver route
	// (IDEA-1492): GET /{username}/{workspace}/ref/{REF} → 302 to the
	// canonical item URL. A collection slug of "ref" would intercept every
	// item URL under that collection (the resolver's `{ref}` segment can't
	// match an arbitrary slug shape), so we forbid it at create time
	// rather than tolerate silent 404s after the fact.
	"ref": true,
}

// isReservedCollectionSlug checks whether a slug would collide with a
// workspace-level UI route.
func isReservedCollectionSlug(slug string) bool {
	return reservedCollectionSlugs[strings.ToLower(slug)]
}
