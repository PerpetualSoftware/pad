package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// childLinkTypes lists the link types that establish a parent→child relationship
// for progress tracking. Both 'parent' and 'implements' links count as children.
var childLinkTypes = []string{"parent", "implements"}

// childLinkTypeSQL returns a SQL IN clause fragment like "'parent','implements'"
// for filtering item_links by child relationship types.
func childLinkTypeSQL() string {
	quoted := make([]string, len(childLinkTypes))
	for i, t := range childLinkTypes {
		quoted[i] = "'" + t + "'"
	}
	return strings.Join(quoted, ",")
}

// ItemSearchResult holds FTS search results for items.
type ItemSearchResult struct {
	Item    models.Item `json:"item"`
	Snippet string      `json:"snippet"`
	Rank    float64     `json:"rank"`
}

// validateAssignmentScope checks that the assigned user and agent role belong to the
// same workspace as the item. This prevents cross-workspace assignment leaks.
func (s *Store) validateAssignmentScope(workspaceID string, assignedUserID, agentRoleID *string) error {
	if assignedUserID != nil && *assignedUserID != "" {
		isMember, err := s.IsWorkspaceMember(workspaceID, *assignedUserID)
		if err != nil {
			return fmt.Errorf("validate assigned user: %w", err)
		}
		if !isMember {
			return fmt.Errorf("assigned user is not a member of this workspace")
		}
	}
	if agentRoleID != nil && *agentRoleID != "" {
		role, err := s.GetAgentRole(workspaceID, *agentRoleID)
		if err != nil {
			return fmt.Errorf("validate agent role: %w", err)
		}
		if role == nil {
			return fmt.Errorf("agent role does not belong to this workspace")
		}
	}
	return nil
}

// maxItemNumberRetries is the number of times CreateItem will retry when a
// concurrent insert claims the same workspace-global item_number.
const maxItemNumberRetries = 10

// nextWorkspaceSeqSubquery is the SQL fragment used to atomically compute
// the next workspace-scoped `seq` value inside an INSERT / UPDATE.
// Every items mutation (create / update / soft-delete / restore) stamps
// the new row's seq with `MAX(seq) + 1 WHERE workspace_id = ?`, which is
// the cursor mechanic for the local-first read model's delta sync
// (PLAN-1343 / DOC-1342 decision #1).
//
// Callers must append exactly one `workspaceID` arg for this fragment.
// SQLite is single-writer so the read-modify-write is naturally
// serialized; Postgres callers must additionally hold the workspace
// advisory lock acquired via acquireWorkspaceSeqLock so concurrent
// writes can't both read the same MAX(seq) and produce duplicates.
const nextWorkspaceSeqSubquery = "(SELECT COALESCE(MAX(seq), 0) + 1 FROM items WHERE workspace_id = ?)"

// nextTransitionSeqSubquery assigns a monotonic, insertion-ordered seq to each
// status_transitions row (global MAX+1 — cross-row dupes across items/workspaces
// are harmless since the ordering is only used WITHIN a single item's history).
// It's the precise tiebreak for "latest transition <= T" when created_at
// (second precision) ties (PLAN-1628 / TASK-1643). The inserts that use it run
// inside the workspace seq lock, so per-item assignment is serialized.
const nextTransitionSeqSubquery = "(SELECT COALESCE(MAX(seq), 0) + 1 FROM status_transitions)"

// acquireWorkspaceSeqLock takes a Postgres advisory transaction lock
// keyed on the workspace ID so concurrent seq-bumping mutations
// serialize. The lock auto-releases on COMMIT / ROLLBACK. On SQLite
// the single-writer rule already serializes writes, so this is a
// no-op there. Mirrors the existing advisory-lock pattern in
// tryCreateItem (which uses the same key for item_number assignment).
func (s *Store) acquireWorkspaceSeqLock(tx *sql.Tx, workspaceID string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext($1))", workspaceID); err != nil {
		return fmt.Errorf("acquire workspace seq lock: %w", err)
	}
	return nil
}

func (s *Store) CreateItem(workspaceID, collectionID string, input models.ItemCreate) (*models.Item, error) {
	// Validate assignment scope before writing
	if err := s.validateAssignmentScope(workspaceID, input.AssignedUserID, input.AgentRoleID); err != nil {
		return nil, err
	}

	id := newID()
	ts := now()

	fields := input.Fields
	if fields == "" {
		fields = "{}"
	}
	tags := input.Tags
	if tags == "" {
		tags = "[]"
	}
	createdBy := input.CreatedBy
	if createdBy == "" {
		createdBy = "user"
	}
	source := input.Source
	if source == "" {
		source = "web"
	}

	baseSlug := slugify(input.Title)
	if baseSlug == "" {
		baseSlug = "untitled"
	}
	slug, err := s.uniqueSlug("items", "workspace_id", workspaceID, baseSlug)
	if err != nil {
		return nil, fmt.Errorf("unique slug: %w", err)
	}

	// Retry loop: if a concurrent insert claims the same item_number we
	// roll back and re-read MAX(item_number) on the next attempt.
	var lastErr error
	for attempt := 0; attempt < maxItemNumberRetries; attempt++ {
		lastErr = s.tryCreateItem(id, workspaceID, collectionID, slug, ts, fields, tags, createdBy, source, input)
		if lastErr == nil {
			return s.GetItem(id)
		}
		// Only retry on unique-constraint violations (item_number conflict)
		if !isUniqueViolation(lastErr) {
			return nil, fmt.Errorf("insert item: %w", lastErr)
		}
	}
	return nil, fmt.Errorf("insert item after %d retries: %w", maxItemNumberRetries, lastErr)
}

// tryCreateItem attempts a single transactional insert of an item with the
// next available workspace-global item_number. The item_number is computed
// atomically via a subquery in the INSERT to avoid races between concurrent
// inserts reading the same MAX(item_number).
func (s *Store) tryCreateItem(id, workspaceID, collectionID, slug, ts, fields, tags, createdBy, source string, input models.ItemCreate) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// PostgreSQL: take an advisory lock keyed on the workspace to serialize
	// item_number assignment. This eliminates the race between concurrent
	// transactions reading the same MAX(item_number). The lock is released
	// automatically when the transaction commits or rolls back.
	// SQLite: single-writer by design, no advisory locks needed.
	if s.dialect.Driver() == DriverPostgres {
		// Use a hash of the workspace ID as the advisory lock key.
		_, err = tx.Exec("SELECT pg_advisory_xact_lock(hashtext($1))", workspaceID)
		if err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
	}

	// Compute and insert the next item_number atomically within the lock.
	// content_flushed_at + content_flushed_op_log_id are the op-log GC
	// watermarks (TASK-1309). The id column is authoritative — sweeper
	// uses strict id comparison to avoid second-granularity timestamp
	// false positives. Both set iff content is non-empty:
	//   - timestamp = creation time (informational)
	//   - id = 0 (vacuously safe — there are no op-log rows yet, so
	//     "covers all rows up to id 0" never gates anything until the
	//     first op-log row arrives, at which point this item is
	//     non-dormant by virtue of having a recent row)
	// Empty content → both NULL; sweeper treats NULL as "never flushed"
	// and skips pruning.
	var contentFlushedAt interface{}
	var contentFlushedOpLogID interface{}
	if input.Content != "" {
		contentFlushedAt = ts
		contentFlushedOpLogID = int64(0)
	}
	// The workspace advisory lock acquired above for item_number
	// assignment ALSO serializes the seq subquery below — both read
	// MAX(...) per workspace and would otherwise race in Postgres.
	_, err = tx.Exec(s.q(`
		INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags,
		                   pinned, sort_order, parent_id, assigned_user_id, agent_role_id, role_sort_order,
		                   created_by, last_modified_by, source, item_number, created_at, updated_at,
		                   content_flushed_at, content_flushed_op_log_id, seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 0, ?, ?, ?,
		        (SELECT COALESCE(MAX(item_number), 0) + 1 FROM items WHERE workspace_id = ?),
		        ?, ?, ?, ?, `+nextWorkspaceSeqSubquery+`)
	`), id, workspaceID, collectionID, input.Title, slug, input.Content, fields, tags,
		s.dialect.BoolToInt(input.Pinned), input.ParentID, input.AssignedUserID, input.AgentRoleID,
		createdBy, createdBy, source, workspaceID, ts, ts, contentFlushedAt, contentFlushedOpLogID, workspaceID)
	if err != nil {
		return err
	}

	// Create initial version if there's content
	if input.Content != "" {
		vid := newID()
		_, err = tx.Exec(s.q(`
			INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at)
			VALUES (?, ?, ?, '', ?, ?, ?, ?)
		`), vid, id, input.Content, createdBy, source, s.dialect.BoolToInt(false), ts)
		if err != nil {
			return fmt.Errorf("create initial version: %w", err)
		}
	}

	// Index [[...]] wiki-links from the new content. Lives inside the
	// same tx as the items INSERT so partial state never lands and a
	// content rollback also rolls back the index rows. Empty content
	// is fine — replaceWikiLinks short-circuits after deleting any
	// prior rows (there are none on initial create). PLAN-1593 /
	// TASK-1594.
	if err := s.replaceWikiLinks(tx, id, workspaceID, input.Content); err != nil {
		return fmt.Errorf("index wiki links: %w", err)
	}

	// Phase 2a (TASK-1595): flip any pre-existing broken `[[Title]]`
	// rows that have been waiting for an item with this title to
	// arrive. Cheap when no broken rows match (the common case).
	// Without this, sources that mention the new item by title would
	// stay broken until either their content is rewritten or the
	// next migration-driven backfill — both rare.
	collSlug, err := s.getCollectionSlugTx(tx, collectionID)
	if err != nil {
		return fmt.Errorf("lookup collection slug: %w", err)
	}
	if err := s.resolveBrokenTitleLinks(tx, id, workspaceID, collSlug, input.Title); err != nil {
		return fmt.Errorf("resolve broken titles: %w", err)
	}

	// Seed the create-time "entered initial status" transition so an item
	// created directly in a terminal done-field value (e.g. a retroactively
	// logged "done" task, or an import) still counts as a completion in
	// reports (PLAN-1628 / TASK-1637). Resolve the collection's done field
	// in-tx; skip when it's unset at creation. The deterministic id keeps
	// this idempotent with the backfill's create-seed for the same item.
	var schemaJSON, settingsJSON string
	if err := tx.QueryRow(s.q(`SELECT schema, settings FROM collections WHERE id = ?`), collectionID).Scan(&schemaJSON, &settingsJSON); err == nil {
		doneKey := doneFieldKeyFromSchemaJSON(schemaJSON, settingsJSON)
		if initial := extractFieldValue(fields, doneKey); initial != "" {
			if _, err = tx.Exec(s.q(`
				INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at, seq)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+nextTransitionSeqSubquery+`)
			`), "create_"+id, id, workspaceID, collectionID, doneKey, "", initial, ts); err != nil {
				return fmt.Errorf("record create-time status transition: %w", err)
			}
		}
	}

	return tx.Commit()
}

// getCollectionSlugTx reads collections.slug for a collection_id
// inside the supplied tx. Tiny helper used by the wiki-link cascade
// hooks that need the slug to build collection-qualified link keys.
func (s *Store) getCollectionSlugTx(tx *sql.Tx, collectionID string) (string, error) {
	var slug string
	if err := tx.QueryRow(s.q(`SELECT slug FROM collections WHERE id = ?`), collectionID).Scan(&slug); err != nil {
		return "", err
	}
	return slug, nil
}

// isUniqueViolation checks whether an error is a unique constraint violation.
// Works for both SQLite (UNIQUE constraint failed) and PostgreSQL (duplicate key).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint")
}

func (s *Store) GetItem(id string) (*models.Item, error) {
	var item models.Item
	var createdAt, updatedAt string
	var deletedAt *string
	var pinned bool

	err := s.db.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.id = ? AND i.deleted_at IS NULL
	`), id).Scan(
		&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
		&item.Content, &item.Fields, &item.Tags,
		&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
		&item.CreatedBy, &item.LastModifiedBy, &item.Source,
		&item.ItemNumber, &item.Seq, &createdAt, &updatedAt, &deletedAt,
		&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
		&item.AssignedUserName, &item.AssignedUserEmail,
		&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item: %w", err)
	}

	item.Pinned = pinned
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.DeletedAt = parseTimePtr(deletedAt)
	hydrateItemComputedMetadata(&item)
	return &item, nil
}

// getItemTx is the in-transaction variant of GetItem. Used by
// UpdateItemWithPreCheck to re-read the parent under the workspace +
// parent-children locks so the invariant precheck classifies the
// transition against a snapshot that's stable for the rest of the tx
// (Codex round-3 P2). Soft-deleted items are excluded, matching
// GetItem's contract.
func (s *Store) getItemTx(tx *sql.Tx, id string) (*models.Item, error) {
	var item models.Item
	var createdAt, updatedAt string
	var deletedAt *string
	var pinned bool

	err := tx.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.id = ? AND i.deleted_at IS NULL
	`), id).Scan(
		&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
		&item.Content, &item.Fields, &item.Tags,
		&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
		&item.CreatedBy, &item.LastModifiedBy, &item.Source,
		&item.ItemNumber, &item.Seq, &createdAt, &updatedAt, &deletedAt,
		&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
		&item.AssignedUserName, &item.AssignedUserEmail,
		&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item (tx): %w", err)
	}

	item.Pinned = pinned
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.DeletedAt = parseTimePtr(deletedAt)
	hydrateItemComputedMetadata(&item)
	return &item, nil
}

func (s *Store) GetItemBySlug(workspaceID, slug string) (*models.Item, error) {
	var id string
	err := s.db.QueryRow(s.q(`
		SELECT id FROM items
		WHERE workspace_id = ? AND slug = ? AND deleted_at IS NULL
	`), workspaceID, slug).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item by slug: %w", err)
	}
	return s.GetItem(id)
}

// GetItemByRef looks up an item by its PREFIX-NUMBER reference (e.g. "IDEA-15").
// Since item numbers are workspace-unique, this first tries an exact prefix match
// and falls back to a number-only lookup. This allows old refs to still resolve
// after an item has been moved to a different collection (e.g. PLAN-42 still
// finds the item even after it became TASK-42).
func (s *Store) GetItemByRef(workspaceID, prefix string, number int) (*models.Item, error) {
	var id string
	// Try exact prefix + number match first
	err := s.db.QueryRow(s.q(`
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ? AND i.deleted_at IS NULL
	`), workspaceID, prefix, number).Scan(&id)
	if err == nil {
		return s.GetItem(id)
	}
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("get item by ref: %w", err)
	}

	// Fallback: item numbers are workspace-unique, so look up by number alone.
	// This handles the case where an item was moved to a different collection
	// but is still being referenced by its old prefix.
	err = s.db.QueryRow(s.q(`
		SELECT id FROM items
		WHERE workspace_id = ? AND item_number = ? AND deleted_at IS NULL
	`), workspaceID, number).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item by number: %w", err)
	}
	return s.GetItem(id)
}

// ResolveItem looks up an item by UUID, PREFIX-NUMBER ref (e.g. "IDEA-15"),
// or slug. UUID is tried first, then ref, then slug.
func (s *Store) ResolveItem(workspaceID, identifier string) (*models.Item, error) {
	// Try UUID lookup first (8-4-4-4-12 hex format)
	if isUUID(identifier) {
		item, err := s.GetItem(identifier)
		if err != nil {
			return nil, err
		}
		if item != nil && item.WorkspaceID == workspaceID {
			return item, nil
		}
	}
	// Try PREFIX-NUMBER ref
	if prefix, number, ok := parseItemRef(identifier); ok {
		item, err := s.GetItemByRef(workspaceID, prefix, number)
		if err != nil {
			return nil, err
		}
		if item != nil {
			return item, nil
		}
	}
	// Fall back to slug lookup
	return s.GetItemBySlug(workspaceID, identifier)
}

// isUUID checks if a string looks like a UUID (8-4-4-4-12 hex).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ResolveItemIncludeDeleted is like ResolveItem but includes soft-deleted items.
func (s *Store) ResolveItemIncludeDeleted(workspaceID, slugOrRef string) (*models.Item, error) {
	// UUID lookup first, mirroring ResolveItem — but include-deleted so a
	// bulk restore (TASK-1674) can resolve archived rows by id.
	if isUUID(slugOrRef) {
		item, err := s.GetItemIncludeDeleted(slugOrRef)
		if err != nil {
			return nil, err
		}
		if item != nil && item.WorkspaceID == workspaceID {
			return item, nil
		}
	}
	if prefix, number, ok := parseItemRef(slugOrRef); ok {
		var item models.Item
		var createdAt, updatedAt string
		var deletedAt *string
		var pinned bool

		err := s.db.QueryRow(s.q(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.seq, i.created_at, i.updated_at, i.deleted_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ?
		`), workspaceID, prefix, number).Scan(
			&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
			&item.Content, &item.Fields, &item.Tags,
			&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
			&item.CreatedBy, &item.LastModifiedBy, &item.Source,
			&item.ItemNumber, &item.Seq, &createdAt, &updatedAt, &deletedAt,
			&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
			&item.AssignedUserName, &item.AssignedUserEmail,
			&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
		)
		if err == nil {
			item.Pinned = pinned
			item.CreatedAt = parseTime(createdAt)
			item.UpdatedAt = parseTime(updatedAt)
			item.DeletedAt = parseTimePtr(deletedAt)
			hydrateItemComputedMetadata(&item)
			return &item, nil
		}
		if err != sql.ErrNoRows {
			return nil, fmt.Errorf("resolve ref (include deleted): %w", err)
		}
	}
	return s.GetItemBySlugIncludeDeleted(workspaceID, slugOrRef)
}

// parseItemRef parses "PREFIX-123" into ("PREFIX", 123, true).
// Returns false if the string is not a valid item ref.
// Case-insensitive: "task-5", "Task-5", and "TASK-5" all parse to ("TASK", 5, true).
func parseItemRef(s string) (string, int, bool) {
	s = strings.ToUpper(s)
	idx := strings.LastIndex(s, "-")
	if idx <= 0 || idx == len(s)-1 {
		return "", 0, false
	}
	prefix := s[:idx]
	// Prefix must be all uppercase letters
	for _, c := range prefix {
		if c < 'A' || c > 'Z' {
			return "", 0, false
		}
	}
	numStr := s[idx+1:]
	num := 0
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		num = num*10 + int(c-'0')
	}
	if num == 0 {
		return "", 0, false
	}
	return prefix, num, true
}

// parseItemNumber parses a bare numeric string (e.g. "843") into a positive
// item number. Returns false for empty strings, non-digit input, zero, or
// values exceeding a sane upper bound (999999 — items_workspace_number is
// workspace-global so this comfortably fits any real workspace).
//
// Used by Search() to support "type a number, get the item" — a workspace
// has at most one item with any given item_number (unique index on
// (workspace_id, item_number)) so this resolves to a single direct hit.
// See BUG-910.
func parseItemNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	num := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		num = num*10 + int(c-'0')
		if num > 999999 {
			return 0, false
		}
	}
	if num == 0 {
		return 0, false
	}
	return num, true
}

// GetItemIncludeDeleted finds an item by id including soft-deleted
// items. Used by code paths that need to act on records the user
// already owns even though the parent item has been moved to trash —
// the most common case is the Settings → Storage attachment list,
// where attachments survive a soft-deleted parent (so the user can
// see what's still consuming quota and decide whether to delete the
// blob). The visibility check still keys off the (still-set)
// collection_id, so soft-deleting an item doesn't escalate access.
func (s *Store) GetItemIncludeDeleted(id string) (*models.Item, error) {
	var item models.Item
	var createdAt, updatedAt string
	var deletedAt *string
	var pinned bool

	err := s.db.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.id = ?
	`), id).Scan(
		&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
		&item.Content, &item.Fields, &item.Tags,
		&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
		&item.CreatedBy, &item.LastModifiedBy, &item.Source,
		&item.ItemNumber, &item.Seq, &createdAt, &updatedAt, &deletedAt,
		&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
		&item.AssignedUserName, &item.AssignedUserEmail,
		&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item (include deleted): %w", err)
	}

	item.Pinned = pinned
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.DeletedAt = parseTimePtr(deletedAt)
	hydrateItemComputedMetadata(&item)
	return &item, nil
}

// GetItemBySlugIncludeDeleted finds an item by slug including soft-deleted items.
// Used for restore operations where the item is archived.
func (s *Store) GetItemBySlugIncludeDeleted(workspaceID, slug string) (*models.Item, error) {
	var item models.Item
	var createdAt, updatedAt string
	var deletedAt *string
	var pinned bool

	err := s.db.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.workspace_id = ? AND i.slug = ?
	`), workspaceID, slug).Scan(
		&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
		&item.Content, &item.Fields, &item.Tags,
		&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
		&item.CreatedBy, &item.LastModifiedBy, &item.Source,
		&item.ItemNumber, &item.Seq, &createdAt, &updatedAt, &deletedAt,
		&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
		&item.AssignedUserName, &item.AssignedUserEmail,
		&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item by slug (include deleted): %w", err)
	}

	item.Pinned = pinned
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.DeletedAt = parseTimePtr(deletedAt)
	hydrateItemComputedMetadata(&item)
	return &item, nil
}

func (s *Store) ListItems(workspaceID string, params models.ItemListParams) ([]models.Item, error) {
	// Non-nil empty CollectionIDs means "no visible collections" — return
	// empty results immediately, unless ItemIDs are also provided (item-level
	// grants may still allow access to specific items even without full
	// collection access).
	if params.CollectionIDs != nil && len(params.CollectionIDs) == 0 && len(params.ItemIDs) == 0 {
		return nil, nil
	}

	// When search is specified, use FTS. Whitespace-only input is treated as
	// "no search filter" (would otherwise sanitize to empty and crash SQLite
	// FTS5 with "syntax error near \"\"" — see BUG-818).
	if strings.TrimSpace(params.Search) != "" {
		return s.listItemsFTS(workspaceID, params)
	}

	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''), i.deleted_at
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.workspace_id = ?
	`
	args := []interface{}{workspaceID}

	if !params.IncludeArchived {
		query += " AND i.deleted_at IS NULL"
	}

	if params.CollectionSlug != "" {
		query += " AND c.slug = ?"
		args = append(args, params.CollectionSlug)
	}

	if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
		// Guest with both collection-level and item-level grants:
		// item must be in a fully-granted collection OR be a specifically granted item
		collPlaceholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			collPlaceholders[i] = "?"
			args = append(args, id)
		}
		itemPlaceholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			itemPlaceholders[i] = "?"
			args = append(args, id)
		}
		query += " AND (i.collection_id IN (" + strings.Join(collPlaceholders, ",") + ") OR i.id IN (" + strings.Join(itemPlaceholders, ",") + "))"
	} else if len(params.CollectionIDs) > 0 {
		placeholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.collection_id IN (" + strings.Join(placeholders, ",") + ")"
	} else if len(params.ItemIDs) > 0 {
		// Guest with only item-level grants (no collection-level grants)
		placeholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.id IN (" + strings.Join(placeholders, ",") + ")"
	}

	if params.Tag != "" {
		tagExpr, tagArg := s.dialect.JSONArrayContains("i.tags", params.Tag)
		query += " AND " + tagExpr
		args = append(args, tagArg)
	}

	if params.ParentID != "" {
		query += " AND i.parent_id = ?"
		args = append(args, params.ParentID)
	}

	if params.AssignedUserID != "" {
		query += " AND i.assigned_user_id = ?"
		args = append(args, params.AssignedUserID)
	}

	if params.AgentRoleID != "" {
		query += " AND (i.agent_role_id = ? OR ar.slug = ?)"
		args = append(args, params.AgentRoleID, params.AgentRoleID)
	}

	// Parent link filter via item_links. Joins items so we ignore links pointing
	// to a soft-deleted parent — slug/ref filtering already rejects deleted
	// parents upstream, but raw-UUID input bypasses that path. See BUG-734 /
	// Codex review on PR #259.
	if params.ParentLinkID != "" {
		query += " AND EXISTS (SELECT 1 FROM item_links il JOIN items p ON p.id = il.target_id AND p.deleted_at IS NULL WHERE il.source_id = i.id AND il.link_type = 'parent' AND il.target_id = ?)"
		args = append(args, params.ParentLinkID)
	}

	// Field filters — supports comma-separated values as OR
	for key, value := range params.Fields {
		// Sanitize the key to prevent SQL injection — field names must be
		// alphanumeric/underscore only (user-controlled from query params).
		if !isValidFieldKey(key) {
			continue
		}
		jsonExpr := s.dialect.JSONExtractText("i.fields", key)
		if strings.Contains(value, ",") {
			values := strings.Split(value, ",")
			placeholders := make([]string, len(values))
			for i, v := range values {
				placeholders[i] = "?"
				args = append(args, strings.TrimSpace(v))
			}
			query += " AND " + jsonExpr + " IN (" + strings.Join(placeholders, ",") + ")"
		} else {
			query += " AND " + jsonExpr + " = ?"
			args = append(args, value)
		}
	}

	// Non-terminal filter (BUG-2001): keep only items whose resolved done
	// field is NOT one of their collection's terminal options. Evaluated
	// per-collection so custom status vocabularies work.
	if params.NonTerminal {
		clause, ntArgs := s.nonTerminalFilter(workspaceID, "i")
		query += " AND " + clause
		args = append(args, ntArgs...)
	}

	// Sorting
	query += buildItemSort(params.Sort, s.dialect)

	// Pagination
	if params.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, params.Limit)
		if params.Offset > 0 {
			query += " OFFSET ?"
			args = append(args, params.Offset)
		}
	}

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

// ListWorkspaceTags returns the distinct tags used across a workspace's items
// with the number of (non-archived) items carrying each, ordered by count
// desc then tag asc.
//
// collectionIDs / itemIDs are the same permission filters ListItems takes and
// carry identical semantics: nil collectionIDs means no restriction (admins /
// owners); a non-nil empty collectionIDs with no itemIDs means "no visible
// collections" and returns an empty result. When both are non-empty (a guest
// with collection- and item-level grants) the filters are OR'd, matching
// ListItems exactly so tag counts can never leak items the caller can't see.
func (s *Store) ListWorkspaceTags(workspaceID string, collectionIDs, itemIDs []string) ([]models.TagCount, error) {
	if collectionIDs != nil && len(collectionIDs) == 0 && len(itemIDs) == 0 {
		return []models.TagCount{}, nil
	}

	fromExpr, valueExpr := s.dialect.JSONArrayElements("i.tags", "je")
	// COUNT(DISTINCT i.id), not COUNT(*): the contract is "items carrying the
	// tag". The unnest produces one row per array element, so an item with
	// duplicate tags (e.g. ["ux","ux"] — the write path doesn't enforce
	// per-item uniqueness) would otherwise be counted twice.
	query := `
		SELECT ` + valueExpr + ` AS tag, COUNT(DISTINCT i.id) AS cnt
		FROM items i, ` + fromExpr + `
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL`
	args := []interface{}{workspaceID}

	if len(collectionIDs) > 0 && len(itemIDs) > 0 {
		collPlaceholders := make([]string, len(collectionIDs))
		for i, id := range collectionIDs {
			collPlaceholders[i] = "?"
			args = append(args, id)
		}
		itemPlaceholders := make([]string, len(itemIDs))
		for i, id := range itemIDs {
			itemPlaceholders[i] = "?"
			args = append(args, id)
		}
		query += " AND (i.collection_id IN (" + strings.Join(collPlaceholders, ",") + ") OR i.id IN (" + strings.Join(itemPlaceholders, ",") + "))"
	} else if len(collectionIDs) > 0 {
		placeholders := make([]string, len(collectionIDs))
		for i, id := range collectionIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.collection_id IN (" + strings.Join(placeholders, ",") + ")"
	} else if len(itemIDs) > 0 {
		placeholders := make([]string, len(itemIDs))
		for i, id := range itemIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.id IN (" + strings.Join(placeholders, ",") + ")"
	}

	query += " GROUP BY " + valueExpr + " ORDER BY cnt DESC, tag ASC"

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list workspace tags: %w", err)
	}
	defer rows.Close()

	tags := []models.TagCount{}
	for rows.Next() {
		var tc models.TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, fmt.Errorf("scan tag count: %w", err)
		}
		tags = append(tags, tc)
	}
	return tags, rows.Err()
}

// ItemIndexParams is the trimmed parameter set for ListItemsIndex.
// It deliberately omits sort/search/pagination/field-filter knobs that the
// "skinny projection" endpoint doesn't expose — the local-first read model
// fetches the entire workspace once and does its own client-side filtering.
type ItemIndexParams struct {
	// CollectionSlug optionally restricts to a single collection by slug.
	CollectionSlug string
	// CollectionIDs is the permission filter for visible collections.
	// nil = unfiltered. A non-nil empty slice means "no visible collections"
	// and (combined with empty ItemIDs) returns an empty result immediately,
	// matching ListItems semantics.
	CollectionIDs []string
	// ItemIDs additionally allows specific items through (item-level grants
	// for guests / restricted members).
	ItemIDs []string
	// IncludeArchived returns soft-deleted items when true.
	IncludeArchived bool
}

// ListItemsIndex returns the skinny-projection of items in a workspace —
// every column EXCEPT i.content. Used by the local-first read model
// (PLAN-1343) so the client can hydrate an in-memory + IndexedDB index
// without paying the rich-text body cost.
//
// Deterministic sort: updated_at DESC, id ASC (stable tiebreaker so cursors
// over equal-timestamp items are reproducible).
func (s *Store) ListItemsIndex(workspaceID string, params ItemIndexParams) ([]models.Item, error) {
	// Mirror ListItems: a non-nil empty CollectionIDs without item-level grants
	// means "no visible collections" — return empty immediately.
	if params.CollectionIDs != nil && len(params.CollectionIDs) == 0 && len(params.ItemIDs) == 0 {
		return nil, nil
	}

	// `i.deleted_at` is in the projection so the local-first client
	// (PLAN-1343 / TASK-1355) can distinguish archived rows hydrated
	// with `IncludeArchived=true` from live rows. When the flag is
	// false, the WHERE clause filters them out anyway; when it's
	// true, the field is populated for the soft-deleted subset and
	// nil for live rows. Mirrors the projection of
	// ListItemsChangesSince which has always carried this column.
	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.workspace_id = ?
	`
	args := []interface{}{workspaceID}

	if !params.IncludeArchived {
		query += " AND i.deleted_at IS NULL"
	}

	if params.CollectionSlug != "" {
		query += " AND c.slug = ?"
		args = append(args, params.CollectionSlug)
	}

	if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
		collPlaceholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			collPlaceholders[i] = "?"
			args = append(args, id)
		}
		itemPlaceholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			itemPlaceholders[i] = "?"
			args = append(args, id)
		}
		query += " AND (i.collection_id IN (" + strings.Join(collPlaceholders, ",") + ") OR i.id IN (" + strings.Join(itemPlaceholders, ",") + "))"
	} else if len(params.CollectionIDs) > 0 {
		placeholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.collection_id IN (" + strings.Join(placeholders, ",") + ")"
	} else if len(params.ItemIDs) > 0 {
		placeholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.id IN (" + strings.Join(placeholders, ",") + ")"
	}

	// Deterministic sort: most-recently-updated first, with id as a stable
	// secondary key so equal-timestamp rows have a reproducible order.
	query += " ORDER BY i.updated_at DESC, i.id ASC"

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list items index: %w", err)
	}
	defer rows.Close()

	return scanItemsIndex(rows)
}

// ItemChangesParams is the parameter set for ListItemsChangesSince.
// Mirrors the visibility-filter half of ItemIndexParams (CollectionIDs
// / ItemIDs) plus the cursor-specific knobs Since / Limit.
type ItemChangesParams struct {
	// CollectionIDs is the permission filter for visible collections.
	// nil = unfiltered. A non-nil empty slice (with empty ItemIDs)
	// short-circuits to an empty result, matching ListItemsIndex
	// semantics.
	CollectionIDs []string
	// ItemIDs is the item-level grant set (guests / restricted
	// members can see specific items even outside their collection
	// scope).
	ItemIDs []string
	// Since is the exclusive seq lower bound (returns rows where
	// `seq > since`).
	Since int64
	// Limit caps the returned slice. <=0 means use the default cap
	// (DefaultItemChangesLimit); values above MaxItemChangesLimit
	// are clamped.
	Limit int
}

// DefaultItemChangesLimit is the default cap on /items-changes
// responses. 5,000 is enough to drain a typical workspace in one
// round-trip; clients that need more re-page via the cursor.
const DefaultItemChangesLimit = 5000

// MaxItemChangesLimit clamps any caller-supplied limit so a runaway
// `?limit=999999` poll can't materialize the entire workspace
// (think: months-offline tab).
const MaxItemChangesLimit = 50000

// ListItemsChangesSince returns the skinny-projection of items that
// have mutated (create / update / soft-delete / restore) since the
// given seq cursor, in ascending seq order. Soft-deleted rows ARE
// included so delta-sync clients can drop tombstoned items from
// their local index — the scan populates models.Item.DeletedAt for
// every row so the caller can distinguish upserts from deletes.
//
// Auth: same shape as ListItemsIndex — CollectionIDs / ItemIDs gate
// visibility. A delta from `since=0` over a fully-permitted scope
// matches the /items-index payload modulo ordering (changes is seq
// ASC, index is updated_at DESC).
//
// Returns at most Limit rows (default DefaultItemChangesLimit,
// capped at MaxItemChangesLimit). When truncated, the caller's
// next poll should pass the returned cursor's MAX(seq) as `since`
// to resume.
func (s *Store) ListItemsChangesSince(workspaceID string, params ItemChangesParams) ([]models.Item, error) {
	if params.CollectionIDs != nil && len(params.CollectionIDs) == 0 && len(params.ItemIDs) == 0 {
		return nil, nil
	}

	limit := params.Limit
	if limit <= 0 {
		limit = DefaultItemChangesLimit
	}
	if limit > MaxItemChangesLimit {
		limit = MaxItemChangesLimit
	}

	// Same column list as scanItemsIndex plus deleted_at so the caller
	// can distinguish upserts from tombstones. There is no
	// `i.deleted_at IS NULL` filter here — that's the whole point of
	// the delta: soft-deleted rows propagate so clients can remove
	// them from their local index.
	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.workspace_id = ? AND i.seq > ?
	`
	args := []interface{}{workspaceID, params.Since}

	if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
		collPlaceholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			collPlaceholders[i] = "?"
			args = append(args, id)
		}
		itemPlaceholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			itemPlaceholders[i] = "?"
			args = append(args, id)
		}
		query += " AND (i.collection_id IN (" + strings.Join(collPlaceholders, ",") + ") OR i.id IN (" + strings.Join(itemPlaceholders, ",") + "))"
	} else if len(params.CollectionIDs) > 0 {
		placeholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.collection_id IN (" + strings.Join(placeholders, ",") + ")"
	} else if len(params.ItemIDs) > 0 {
		placeholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.id IN (" + strings.Join(placeholders, ",") + ")"
	}

	// Ascending seq is the canonical delta order: the next poll passes
	// the response's MAX(seq) as `since` and resumes with no gap or
	// overlap. The supporting index is (workspace_id, seq DESC)
	// (TASK-1352 migration) — the engine can still use it for ASC
	// scans, just walked in reverse.
	query += " ORDER BY i.seq ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list items changes: %w", err)
	}
	defer rows.Close()

	return scanItemsChanges(rows)
}

// scanItemsChanges scans rows from ListItemsChangesSince (skinny
// projection + deleted_at so callers can distinguish tombstones).
func scanItemsChanges(rows *sql.Rows) ([]models.Item, error) {
	var items []models.Item
	for rows.Next() {
		var item models.Item
		var createdAt, updatedAt string
		var deletedAt *string
		var pinned bool
		if err := rows.Scan(
			&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
			&item.Fields, &item.Tags,
			&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
			&item.CreatedBy, &item.LastModifiedBy, &item.Source,
			&item.ItemNumber, &item.Seq, &createdAt, &updatedAt, &deletedAt,
			&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
			&item.AssignedUserName, &item.AssignedUserEmail,
			&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
		); err != nil {
			return nil, err
		}
		item.Pinned = pinned
		item.CreatedAt = parseTime(createdAt)
		item.UpdatedAt = parseTime(updatedAt)
		item.DeletedAt = parseTimePtr(deletedAt)
		hydrateItemComputedMetadata(&item)
		items = append(items, item)
	}
	return items, rows.Err()
}

// MovedOutRow is a minimal "this item left your view" signal returned
// by ListMovedOutSince — id + seq only, no title/fields/target so a
// caller learns an item disappeared from a collection they can see
// without leaking any data from the (invisible) destination.
type MovedOutRow struct {
	ID  string
	Seq int64
}

// ListMovedOutSince finds items that the main /items-changes delta drops
// because they moved OUT of the caller's visible scope: their CURRENT
// collection is one the caller can't see (so the seq>since row is
// filtered out), yet item_collection_moves records that they left a
// collection the caller CAN see at a seq in this delta window. The
// caller has read access to that source collection, so signalling "id X
// left your view" is within their scope — and the returned row carries
// only id+seq, never any destination data (BUG-1675).
//
// Keyed on the MOVE's seq (item_collection_moves.seq), which is written
// in the SAME transaction as the move and never changes on later edits —
// so the tombstone fires once, pages deterministically, and can't be
// raced or lost by the best-effort activity log (Codex rounds 1–3). The
// per-move rows also handle multi-hop moves: only a move whose
// from-collection is visible qualifies, and MIN(seq) picks the earliest
// such visibility loss so the cursor can't skip it.
//
// collLevelVisibleIDs is the set of collections the caller browses at the
// collection level (same set used to filter the main delta).
// grantedItemIDs are excluded: an item the caller holds a direct grant on
// stays visible across a move (the grant transcends collection), so the
// main delta still delivers it and it must NOT be tombstoned.
//
// Returns nil for unrestricted callers (empty visible scope) — full
// members never lose visibility on a move, so this path is moot for them.
func (s *Store) ListMovedOutSince(
	workspaceID string,
	since int64,
	limit int,
	collLevelVisibleIDs []string,
	grantedItemIDs []string,
) ([]MovedOutRow, error) {
	if len(collLevelVisibleIDs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = DefaultItemChangesLimit
	}
	if limit > MaxItemChangesLimit {
		limit = MaxItemChangesLimit
	}

	// Earliest in-window move OUT of a visible source collection, per
	// item that is currently NOT visible to the caller. Fully SQL +
	// indexed — no JSON parsing, no Go-side filtering.
	query := `
		SELECT m.item_id, MIN(m.seq) AS move_seq
		FROM item_collection_moves m
		JOIN items i ON i.id = m.item_id
		WHERE m.workspace_id = ? AND m.seq > ?
	`
	args := []interface{}{workspaceID, since}

	vis := make([]string, len(collLevelVisibleIDs))
	for i, id := range collLevelVisibleIDs {
		vis[i] = "?"
		args = append(args, id)
	}
	// Left a collection the caller CAN see...
	query += " AND m.from_collection_id IN (" + strings.Join(vis, ",") + ")"
	// ...and now lives in one they CAN'T.
	vis2 := make([]string, len(collLevelVisibleIDs))
	for i, id := range collLevelVisibleIDs {
		vis2[i] = "?"
		args = append(args, id)
	}
	query += " AND i.collection_id NOT IN (" + strings.Join(vis2, ",") + ")"

	if len(grantedItemIDs) > 0 {
		gph := make([]string, len(grantedItemIDs))
		for i, id := range grantedItemIDs {
			gph[i] = "?"
			args = append(args, id)
		}
		query += " AND i.id NOT IN (" + strings.Join(gph, ",") + ")"
	}

	query += " GROUP BY m.item_id ORDER BY move_seq ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list moved-out: %w", err)
	}
	defer rows.Close()

	var out []MovedOutRow
	for rows.Next() {
		var r MovedOutRow
		if err := rows.Scan(&r.ID, &r.Seq); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MaxItemSeq returns the largest items.seq across the workspace, or 0
// if the workspace has no items. This is the cursor floor for the
// local-first read model (PLAN-1343 / TASK-1353): /items-index hands
// it back when its filtered result set is empty so the client can
// poll /items-changes?since=<cursor> against the workspace's true
// current position instead of restarting from 0.
//
// Soft-deleted items DO contribute to MAX(seq) — the seq column
// bumps on tombstone writes (DeleteItem) so a client's cursor must
// move past those events for the next /items-changes scan to skip
// them. Filtering by `deleted_at IS NULL` here would silently regress
// the cursor whenever the most recent mutation was a delete.
func (s *Store) MaxItemSeq(workspaceID string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(s.q(`SELECT COALESCE(MAX(seq), 0) FROM items WHERE workspace_id = ?`), workspaceID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("max item seq: %w", err)
	}
	return seq, nil
}

// ItemCheckboxProgress is the per-item count of markdown checkboxes
// (`- [ ]` / `- [x]`) extracted from item content. Used by the
// collection page to render checklist progress badges without
// shipping the rich-text body over the wire (PLAN-1343 Phase 1 /
// TASK-1349).
type ItemCheckboxProgress struct {
	ItemID string `json:"item_id"`
	Total  int    `json:"total"`
	Done   int    `json:"done"`
}

// checkboxCountSQL is the SQL fragment used to count `- [ ]` and
// `- [x]` markers inside item content. Implemented identically on
// SQLite and PostgreSQL via the LENGTH/REPLACE arithmetic trick —
// both dialects support LENGTH and REPLACE on TEXT, and integer
// division is identical.
//
// The `i.deleted_at` clause is appended dynamically in
// CollectionCheckboxProgress so callers can request progress for
// archived rows (matches /items-index's include_archived semantics).
const checkboxCountSQL = `
	SELECT i.id,
	       (LENGTH(i.content) - LENGTH(REPLACE(i.content, '- [ ]', ''))) / 5
	         + (LENGTH(i.content) - LENGTH(REPLACE(i.content, '- [x]', ''))) / 5 AS total,
	       (LENGTH(i.content) - LENGTH(REPLACE(i.content, '- [x]', ''))) / 5 AS done
	FROM items i
	WHERE i.workspace_id = ?
	  AND i.collection_id = ?
	  AND i.content LIKE '%- [%]%'
`

// CollectionCheckboxProgress returns the per-item checkbox totals for
// every item in a collection whose content has at least one
// `- [ ]` / `- [x]` marker. The query computes counts server-side via
// LENGTH/REPLACE arithmetic so the wire payload stays small (three
// ints per non-zero item) — much cheaper than shipping every item's
// rich-text body just so the client can grep for checkboxes.
//
// includeArchived controls whether soft-deleted items contribute
// rows. The default (false) matches the pre-existing client-side
// parse for the un-toggled view. With the page's Archived toggle
// on, the collection page renders archived items too — passing
// true preserves their progress badges (per Codex round 2 [P2] on
// PR #491).
//
// Items with no markers, or with non-positive totals after subtracting
// done from open, are filtered out. Result order is unspecified.
func (s *Store) CollectionCheckboxProgress(workspaceID, collectionID string, includeArchived bool) ([]ItemCheckboxProgress, error) {
	query := checkboxCountSQL
	if !includeArchived {
		query += " AND i.deleted_at IS NULL"
	}
	rows, err := s.db.Query(s.q(query), workspaceID, collectionID)
	if err != nil {
		return nil, fmt.Errorf("collection checkbox progress: %w", err)
	}
	defer rows.Close()

	var result []ItemCheckboxProgress
	for rows.Next() {
		var p ItemCheckboxProgress
		if err := rows.Scan(&p.ItemID, &p.Total, &p.Done); err != nil {
			return nil, err
		}
		// Skip rows with no checkboxes — the LIKE filter is a fast
		// preliminary check, but item bodies can contain the substring
		// inside a code block or other context that doesn't end up as
		// a markdown checkbox; the per-row Total accounts for that.
		if p.Total <= 0 {
			continue
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// scanItemsIndex scans rows from ListItemsIndex (skinny projection — no
// i.content column).
func scanItemsIndex(rows *sql.Rows) ([]models.Item, error) {
	var items []models.Item
	for rows.Next() {
		var item models.Item
		var createdAt, updatedAt string
		var deletedAt *string
		var pinned bool
		if err := rows.Scan(
			&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
			&item.Fields, &item.Tags,
			&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
			&item.CreatedBy, &item.LastModifiedBy, &item.Source,
			&item.ItemNumber, &item.Seq, &createdAt, &updatedAt, &deletedAt,
			&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
			&item.AssignedUserName, &item.AssignedUserEmail,
			&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
		); err != nil {
			return nil, err
		}
		item.Pinned = pinned
		item.CreatedAt = parseTime(createdAt)
		item.UpdatedAt = parseTime(updatedAt)
		if deletedAt != nil {
			t := parseTime(*deletedAt)
			item.DeletedAt = &t
		}
		hydrateItemComputedMetadata(&item)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) listItemsFTS(workspaceID string, params models.ItemListParams) ([]models.Item, error) {
	var query string
	var args []interface{}
	var ftsRank string

	if s.dialect.Driver() == DriverPostgres {
		// PostgreSQL: search_vector lives on the items table (aliased as "i").
		ftsMatch := s.dialect.FTSMatch("i", "search_vector")
		ftsRank = s.dialect.FTSRank("i", "search_vector")

		query = fmt.Sprintf(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.seq, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''), i.deleted_at
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE i.workspace_id = ? AND i.deleted_at IS NULL
			AND %s
		`, ftsMatch)
		// PG FTSMatch consumes TWO args: the raw user query AND its
		// hyphen-sanitized form, OR-combined inside the SQL fragment so
		// that hyphenated terms like `task-five` match titles indexed as
		// `task-five-distinctive` while preserving `BUG-842`-style
		// matches (BUG-842).
		args = []interface{}{workspaceID, params.Search, sanitizePGFTSQuery(params.Search)}
	} else {
		// SQLite: uses FTS5 virtual table "items_fts".
		ftsMatch := s.dialect.FTSMatch("items_fts", "search_vector")
		ftsRank = s.dialect.FTSRank("items_fts", "search_vector")

		query = fmt.Sprintf(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.seq, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''), i.deleted_at
			FROM items i
			JOIN items_fts fts ON i.rowid = fts.rowid
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE i.workspace_id = ? AND i.deleted_at IS NULL
			AND %s
		`, ftsMatch)
		// Wrap each whitespace-delimited token in double quotes so FTS5 treats
		// hyphens (and other special chars like AND/OR/NOT/(/)) as literals
		// rather than boolean operators. Without this, `?search=TASK-5` raises
		// "no such column: 5" — see BUG-818. Postgres handles raw input via
		// the OR-combined plainto_tsquery in the dialect (BUG-842).
		args = []interface{}{workspaceID, sanitizeFTSQuery(params.Search)}
	}

	if params.CollectionSlug != "" {
		query += " AND c.slug = ?"
		args = append(args, params.CollectionSlug)
	}

	// Parent link filter — mirrors the non-FTS path so combining
	// `parent=<UUID>&search=<q>` doesn't silently drop the parent constraint
	// (and, by extension, the soft-deleted-parent rejection from BUG-734).
	if params.ParentLinkID != "" {
		query += " AND EXISTS (SELECT 1 FROM item_links il JOIN items p ON p.id = il.target_id AND p.deleted_at IS NULL WHERE il.source_id = i.id AND il.link_type = 'parent' AND il.target_id = ?)"
		args = append(args, params.ParentLinkID)
	}

	if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
		collPlaceholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			collPlaceholders[i] = "?"
			args = append(args, id)
		}
		itemPlaceholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			itemPlaceholders[i] = "?"
			args = append(args, id)
		}
		query += " AND (i.collection_id IN (" + strings.Join(collPlaceholders, ",") + ") OR i.id IN (" + strings.Join(itemPlaceholders, ",") + "))"
	} else if len(params.CollectionIDs) > 0 {
		placeholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.collection_id IN (" + strings.Join(placeholders, ",") + ")"
	} else if len(params.ItemIDs) > 0 {
		placeholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.id IN (" + strings.Join(placeholders, ",") + ")"
	}

	// Filter parity with the non-FTS path. Without these, `?search=...` combined
	// with any of these filter params silently drops the filter and over-returns
	// items. See BUG-812.

	if params.Tag != "" {
		tagExpr, tagArg := s.dialect.JSONArrayContains("i.tags", params.Tag)
		query += " AND " + tagExpr
		args = append(args, tagArg)
	}

	if params.ParentID != "" {
		query += " AND i.parent_id = ?"
		args = append(args, params.ParentID)
	}

	if params.AssignedUserID != "" {
		query += " AND i.assigned_user_id = ?"
		args = append(args, params.AssignedUserID)
	}

	if params.AgentRoleID != "" {
		query += " AND (i.agent_role_id = ? OR ar.slug = ?)"
		args = append(args, params.AgentRoleID, params.AgentRoleID)
	}

	// Field filters — supports comma-separated values as OR. Field keys are
	// user-controlled (query params), so isValidFieldKey gates SQL composition.
	for key, value := range params.Fields {
		if !isValidFieldKey(key) {
			continue
		}
		jsonExpr := s.dialect.JSONExtractText("i.fields", key)
		if strings.Contains(value, ",") {
			values := strings.Split(value, ",")
			placeholders := make([]string, len(values))
			for i, v := range values {
				placeholders[i] = "?"
				args = append(args, strings.TrimSpace(v))
			}
			query += " AND " + jsonExpr + " IN (" + strings.Join(placeholders, ",") + ")"
		} else {
			query += " AND " + jsonExpr + " = ?"
			args = append(args, value)
		}
	}

	// Non-terminal filter (BUG-2001) — parity with the non-FTS path so a
	// `search + non_terminal` combination hides terminal items per each
	// collection's own terminal_options.
	if params.NonTerminal {
		clause, ntArgs := s.nonTerminalFilter(workspaceID, "i")
		query += " AND " + clause
		args = append(args, ntArgs...)
	}

	// SQLite bm25(): more negative = more relevant → ASC (default).
	// PostgreSQL ts_rank(): higher = more relevant → DESC.
	// PG FTSRank embeds the same OR-combined plainto_tsquery as FTSMatch
	// and consumes TWO args (raw + hyphen-sanitized) — BUG-842.
	if s.dialect.Driver() == DriverPostgres {
		query += " ORDER BY " + ftsRank + " DESC"
		args = append(args, params.Search, sanitizePGFTSQuery(params.Search))
	} else {
		query += " ORDER BY " + ftsRank
	}

	if params.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, params.Limit)
	}

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("search items: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

func (s *Store) UpdateItem(id string, input models.ItemUpdate) (*models.Item, error) {
	return s.UpdateItemWithPreCheck(id, input, nil)
}

// UpdateItemWithPreCheck is UpdateItem with an optional pre-mutation
// hook that runs inside the same transaction (and, on Postgres, holds
// the same workspace advisory lock) as the update itself. Callers can
// use the hook to enforce cross-row invariants whose decision must be
// atomic with the write — e.g. the open-children guard (IDEA-1494)
// needs the children-list query and the parent's status flip to share
// a tx so a concurrent child insert / child status change can't slip
// between them.
//
// The hook receives the transaction and the freshly-read existing
// item. Returning a non-nil error rolls the tx back and surfaces the
// error verbatim — callers can return a sentinel and `errors.Is` it
// in the handler.
//
// Pass a nil precheck for the standard, unchecked update path.
func (s *Store) UpdateItemWithPreCheck(
	id string,
	input models.ItemUpdate,
	precheck func(tx *sql.Tx, existing *models.Item) error,
) (*models.Item, error) {
	existing, err := s.GetItem(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	// Validate assignment scope before writing
	if err := s.validateAssignmentScope(existing.WorkspaceID, input.AssignedUserID, input.AgentRoleID); err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Serialize concurrent seq assignments per workspace on Postgres
	// (no-op on SQLite). Held until COMMIT / ROLLBACK.
	if err := s.acquireWorkspaceSeqLock(tx, existing.WorkspaceID); err != nil {
		return nil, err
	}

	// IDEA-1494 round 2: also acquire the parent-children advisory
	// lock for THIS item (as a potential parent) AND for its own
	// parent (when it is itself a child). That gives the open-children
	// guard a tight serialization:
	//
	//   - A parent's UpdateItem precheck holds `pad:parent-children:<parent_id>`
	//     while reading the children list and writing the parent.
	//   - A child's UpdateItem holds the same key for its parent
	//     while it writes itself.
	//
	// Result: a child status-flip that would invalidate the parent's
	// guard cannot interleave between the parent's children-read and
	// the parent's status-write. SQLite gets this for free from
	// BEGIN IMMEDIATE; Postgres needs the explicit advisory lock.
	//
	// Lock ordering: workspace lock → THIS item's parent lock → THIS
	// item's own children lock. Both lock keys are namespaced under
	// `pad:parent-children:` so they only contend on the parent ID;
	// acquiring two distinct keys in a fixed order can't deadlock.
	if err := s.acquireParentChildrenLocksForUpdate(tx, id); err != nil {
		return nil, err
	}

	// IDEA-1494 round 2: run the caller's invariant check (if any)
	// AFTER the locks are held but BEFORE any mutation. Closing the
	// guard-vs-write TOCTOU window relies on this ordering — the
	// precheck's view of `items` / `item_links` is the same one the
	// UPDATE below will write against because every concurrent
	// UpdateItem on this parent (or on any of its children) blocks
	// on the same advisory key.
	//
	// Codex round-3 P2: re-read the item INSIDE the tx (after locks)
	// and pass that fresh snapshot to the precheck. The pre-tx
	// `existing` above was loaded without holding the workspace seq
	// lock or the parent-children lock — a concurrent writer could
	// have flipped the parent's done-field between that read and
	// here, which would mis-classify the transition (false-fire or
	// false-skip). The post-lock re-read sees what the UPDATE will
	// write against.
	if precheck != nil {
		freshExisting, ferr := s.getItemTx(tx, id)
		if ferr != nil {
			return nil, fmt.Errorf("re-read item under lock: %w", ferr)
		}
		if freshExisting == nil {
			// Item was deleted between the pre-tx read and the
			// post-lock re-read. Treat as not-found and let the
			// handler surface a 404. Returning nil here mirrors the
			// existing == nil branch above.
			return nil, nil
		}
		if err := precheck(tx, freshExisting); err != nil {
			return nil, err
		}
		// Use the fresh snapshot for the rest of the function too —
		// otherwise the mutation logic below would proceed from
		// stale data and undo the integrity gain.
		existing = freshExisting
	}

	ts := now()

	// Create version if content is changing
	if input.Content != nil && *input.Content != existing.Content {
		createdBy := input.LastModifiedBy
		if createdBy == "" {
			createdBy = "user"
		}
		// VersionSource takes precedence so the per-version-row
		// attribution can differ from the (persisted) item source.
		// See ItemUpdate.VersionSource doc comment + TASK-1267.
		source := input.VersionSource
		if source == "" {
			source = input.Source
		}
		if source == "" {
			source = "web"
		}

		forceVersion := input.Title != nil && *input.Title != existing.Title
		shouldVersion := forceVersion
		if !shouldVersion {
			shouldVersion, err = s.shouldCreateItemVersion(id, createdBy, source)
			if err != nil {
				return nil, fmt.Errorf("check version throttle: %w", err)
			}
		}

		if shouldVersion {
			vid := newID()
			versionContent := existing.Content
			isDiff := false
			patch := diff.CreateReversePatch(existing.Content, *input.Content)
			if diff.IsDiffSmaller(patch, existing.Content) {
				versionContent = patch
				isDiff = true
			}

			_, err = tx.Exec(s.q(`
				INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`), vid, id, versionContent, input.ChangeSummary, createdBy, source, s.dialect.BoolToInt(isDiff), ts)
			if err != nil {
				return nil, fmt.Errorf("create version: %w", err)
			}
		}
	}

	// Build update query. Every mutation bumps seq to MAX(seq)+1 per
	// workspace; the local-first read model uses that as a cursor (see
	// nextWorkspaceSeqSubquery / PLAN-1343).
	sets := []string{"updated_at = ?", "seq = " + nextWorkspaceSeqSubquery}
	args := []interface{}{ts, existing.WorkspaceID}

	if input.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *input.Title)
		baseSlug := slugify(*input.Title)
		if baseSlug == "" {
			baseSlug = "untitled"
		}
		newSlug, err := s.uniqueSlugExcluding("items", "workspace_id", existing.WorkspaceID, baseSlug, id)
		if err != nil {
			return nil, fmt.Errorf("unique slug: %w", err)
		}
		sets = append(sets, "slug = ?")
		args = append(args, newSlug)
	}
	if input.Content != nil {
		sets = append(sets, "content = ?")
		args = append(args, *input.Content)
		// Bump the human-readable timestamp on every content
		// update — this is informational and never gates GC.
		sets = append(sets, "content_flushed_at = ?")
		args = append(args, ts)

		// Op-log GC watermark policy (TASK-1309 round 5 [P1]):
		// only advance content_flushed_op_log_id from server-driven
		// full-content writes (CLI / MCP / applier-direct-write /
		// version-restore / PruneAndApply). Browser collab-snapshot
		// PATCHes are NOT eligible because they can't prove their
		// markdown captures every peer op:
		//
		//   Tab A's Y.Doc is at op N. Peer B's op N+1 commits to
		//   the op-log. Tab A's 5s flush PATCHes stale markdown
		//   derived from its op-N view. If we advanced the watermark
		//   to MAX(op-log.id) = N+1 here, the sweeper would later
		//   prune op N+1 — the only durable copy of peer B's edit.
		//
		// VersionSource == "collab-snapshot" is the marker the
		// HTTP handler sets for browser flushes (TASK-1267). All
		// other content writes either rebuild content from full
		// op-log state (PruneAndApply) or replace it wholesale
		// (CLI / version restore) — those CAN safely stamp the
		// watermark to MAX(op-log.id) at write time.
		//
		// **Cursor-gated browser flush** (TASK-1319). Browser
		// flushes carry an OpLogCursor recording the highest
		// op-log id their Y.Doc has applied. When that cursor
		// equals the current MAX(item_yjs_updates.id), the
		// flusher has demonstrably captured every persisted op
		// and we advance the watermark to that id. When the
		// cursor is below MAX, peer ops outside the flusher's
		// view exist; the watermark stays put so the GC sweeper
		// cannot delete them. When the cursor is missing (older
		// clients, malformed bodies) we behave as before — no
		// advancement.
		if input.VersionSource != "collab-snapshot" {
			sets = append(sets, "content_flushed_op_log_id = (SELECT COALESCE(MAX(id), 0) FROM item_yjs_updates WHERE item_id = ?)")
			args = append(args, id)
		} else if input.OpLogCursor != nil {
			// Conditional advance: the SQL UPDATE stamps the
			// caller's cursor IFF that cursor still matches the
			// current MAX(op-log.id) at COMMIT time. A peer op
			// that lands between the client computing its
			// cursor and this UPDATE running causes MAX to be
			// strictly greater than the cursor, the predicate
			// fails, and the watermark expression evaluates to
			// the existing column value (a no-op). Never
			// regresses, never over-advances.
			sets = append(sets,
				"content_flushed_op_log_id = CASE "+
					"WHEN ? = (SELECT COALESCE(MAX(id), 0) FROM item_yjs_updates WHERE item_id = ?) "+
					"THEN ? "+
					"ELSE content_flushed_op_log_id "+
					"END")
			args = append(args, *input.OpLogCursor, id, *input.OpLogCursor)
		}
	}
	if input.Fields != nil {
		// IDEA-1486: normalize the empty-string sentinel to a valid JSON
		// object before writing. After the NOT NULL DEFAULT '{}'
		// hardening, Postgres rejects "" at JSONB type-validation and
		// SQLite would silently store invalid JSON. Same boundary
		// normalization as CreateItem (items.go:103-110) and the
		// IDEA-1484 precedent at collections.go:248. Shape validation
		// (object vs. array vs. primitive) is handled at the handler
		// boundary by ItemUpdate.UnmarshalJSON (BUG-1144).
		fields := *input.Fields
		if fields == "" {
			fields = "{}"
		}
		sets = append(sets, "fields = ?")
		args = append(args, fields)
	}
	if input.Tags != nil {
		// IDEA-1486: same empty-string coercion as fields above, but
		// tags is array-shaped so the default is "[]". Mirrors
		// CreateItem at items.go:107-110.
		tags := *input.Tags
		if tags == "" {
			tags = "[]"
		}
		sets = append(sets, "tags = ?")
		args = append(args, tags)
	}
	if input.Pinned != nil {
		sets = append(sets, "pinned = ?")
		args = append(args, s.dialect.BoolToInt(*input.Pinned))
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}
	if input.ParentID != nil {
		sets = append(sets, "parent_id = ?")
		args = append(args, *input.ParentID)
	}
	if input.AssignedUserID != nil {
		sets = append(sets, "assigned_user_id = ?")
		args = append(args, *input.AssignedUserID)
	} else if input.ClearAssignedUser {
		sets = append(sets, "assigned_user_id = NULL")
	}
	if input.AgentRoleID != nil {
		sets = append(sets, "agent_role_id = ?")
		args = append(args, *input.AgentRoleID)
	} else if input.ClearAgentRole {
		sets = append(sets, "agent_role_id = NULL")
	}
	if input.LastModifiedBy != "" {
		sets = append(sets, "last_modified_by = ?")
		args = append(args, input.LastModifiedBy)
	}
	if input.Source != "" {
		sets = append(sets, "source = ?")
		args = append(args, input.Source)
	}

	// Capture the pre-update status BEFORE the UPDATE runs, so the
	// transition log below records an accurate from_status. It must reflect
	// the locked, in-tx state: when a precheck ran, `existing` was already
	// replaced with the fresh in-tx snapshot above; otherwise `existing` is
	// the pre-tx read, which a concurrent update (now serialized behind the
	// workspace/parent locks we hold) could have superseded — re-read in-tx
	// in that case. Reading here, before the UPDATE, is essential: a re-read
	// after the UPDATE would see the new status and the hop would vanish.
	var statusBefore, doneKey string
	if input.Fields != nil {
		doneKey = s.doneFieldKey(existing.CollectionID)
		oldFields := existing.Fields
		if precheck == nil {
			if fresh, ferr := s.getItemTx(tx, id); ferr == nil && fresh != nil {
				oldFields = fresh.Fields
			}
		}
		statusBefore = extractFieldValue(oldFields, doneKey)
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE items SET %s WHERE id = ?", strings.Join(sets, ", "))
	_, err = tx.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update item: %w", err)
	}

	// Record a structured status transition when the fields blob was part
	// of this update AND the `status` value actually changed. Written in
	// the same tx as the item UPDATE so the transition log can never
	// diverge from the item's persisted status, and — unlike the activity
	// feed — NOT debounced, so every hop (open → in-progress → done) is its
	// own row. This is the canonical timestamp source for the Reports
	// completed-throughput and cycle-time series (PLAN-1628 / TASK-1637).
	if input.Fields != nil {
		newStatus := extractFieldValue(*input.Fields, doneKey)
		// Record any change in the done-field value, INCLUDING a clear
		// (X → ""). input.Fields is the full merged blob (CLI/handler merge
		// before write), so newStatus == "" genuinely means the field was
		// cleared, not omitted — recording it keeps the as-of-T reconstruction
		// accurate (an item cleared back to no-status reads as open).
		if newStatus != statusBefore {
			if _, err = tx.Exec(s.q(`
				INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at, seq)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+nextTransitionSeqSubquery+`)
			`), newID(), id, existing.WorkspaceID, existing.CollectionID, doneKey, statusBefore, newStatus, ts); err != nil {
				return nil, fmt.Errorf("record status transition: %w", err)
			}
		}
	}

	// Title rename — cascade to title-form backlinks. Fires whether
	// content changed or not. ORDER MATTERS: cascade runs BEFORE
	// replaceWikiLinks(self) so the pre-existing wl rows pointing
	// at self via target_item_id=renamedItemID are still present
	// when cascade does its SELECT. Codex round 6 finding 2 caught
	// the original order (re-index self → cascade) silently
	// breaking the self-ref cascade on title+content combined
	// updates: re-indexing self first would delete the self-row
	// that cascade needs to find. Function early-returns when
	// oldTitle == newTitle so title-shaped-but-unchanged updates
	// pay nothing. PLAN-1593 / TASK-1595.
	if input.Title != nil && *input.Title != existing.Title {
		// excludeSelf=true when the caller also supplied new content
		// — they're authoritatively rewriting the renamed item's own
		// body and the cascade should respect that. Self-refs in
		// title-only renames still get cascade-rewritten so stale
		// `[[Old Title]]` literals in unmodified content don't go
		// broken. Mirrors documents.go::updateLinksInTx's pattern.
		excludeSelf := input.Content != nil
		if err := s.cascadeTitleRename(tx, id, existing.WorkspaceID, existing.Title, *input.Title, excludeSelf); err != nil {
			return nil, fmt.Errorf("cascade title rename: %w", err)
		}
	}

	// Re-index [[...]] wiki-links if the content was part of this
	// update (regardless of whether the new content equals the old —
	// the caller already paid the UPDATE cost so the delete-then-insert
	// is cheap and keeps the index consistent if a previous reparse
	// left stale rows). When `input.Content == nil` the content
	// wasn't touched, so the existing rows remain valid and we skip
	// work. PLAN-1593 / TASK-1594.
	//
	// Read items.content fresh from the DB instead of using
	// *input.Content directly: cascadeTitleRename above may have
	// rewritten the renamed item's own content if it contained
	// self-references (`[[oldTitle]]` → `[[newTitle]]`), and we
	// want the index to reflect that post-cascade state. Without
	// the fresh read, this re-index would overwrite the cascade-
	// rewritten rows back to whatever the user submitted, undoing
	// the cascade's effect.
	if input.Content != nil {
		currentContent := *input.Content
		if input.Title != nil && *input.Title != existing.Title {
			if err := tx.QueryRow(s.q(`SELECT content FROM items WHERE id = ?`), id).Scan(&currentContent); err != nil {
				return nil, fmt.Errorf("re-read self content after cascade: %w", err)
			}
		}
		if err := s.replaceWikiLinks(tx, id, existing.WorkspaceID, currentContent); err != nil {
			return nil, fmt.Errorf("index wiki links: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetItem(id)
}

// DeleteItem soft-deletes the item by stamping deleted_at and bumping
// the workspace-scoped seq so delta-sync clients see the tombstone.
// The seq bump uses the same MAX(seq)+1 subquery the other mutations
// rely on; the advisory lock keeps concurrent Postgres writes from
// racing on it.
func (s *Store) DeleteItem(id string) error {
	// Look up the workspace before the write so we can key the
	// advisory lock and the seq subquery. The lookup tolerates
	// already-deleted items (we still need to short-circuit cleanly
	// in that case) by reading the include-deleted variant.
	existing, err := s.GetItemIncludeDeleted(id)
	if err != nil {
		return err
	}
	if existing == nil {
		return sql.ErrNoRows
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.acquireWorkspaceSeqLock(tx, existing.WorkspaceID); err != nil {
		return err
	}

	ts := now()
	result, err := tx.Exec(s.q(`
		UPDATE items SET deleted_at = ?, updated_at = ?, seq = `+nextWorkspaceSeqSubquery+`
		WHERE id = ? AND deleted_at IS NULL
	`), ts, ts, existing.WorkspaceID, id)
	if err != nil {
		return fmt.Errorf("delete item: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// RestoreItem un-archives a soft-deleted item and bumps the
// workspace-scoped seq so delta-sync clients re-materialize the row.
// Same lock + subquery shape as DeleteItem.
func (s *Store) RestoreItem(id string) (*models.Item, error) {
	existing, err := s.GetItemIncludeDeleted(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, sql.ErrNoRows
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := s.acquireWorkspaceSeqLock(tx, existing.WorkspaceID); err != nil {
		return nil, err
	}

	// Codex round-3 P1 / round-4 P1: restoring an item resurrects it
	// as a (potentially non-terminal) child of EVERY parent it's
	// linked to (one item can have both a `parent` and an
	// `implements` link). Lock ALL of those parents' children-keys
	// via the canonical sorted-multi-lock helper so concurrent
	// UpdateItemWithPreCheck callers on any of them see this
	// resurrection in their post-lock snapshots.
	//
	// Pre-fix this called AcquireParentChildrenLock for a single
	// LIMIT 1 row — a multi-parent child would have left another
	// parent's precheck racing the resurrection.
	parentIDs, err := s.listParentChildLockKeys(tx, id)
	if err != nil {
		return nil, err
	}
	if err := s.AcquireParentChildrenLocks(tx, parentIDs...); err != nil {
		return nil, err
	}

	ts := now()
	result, err := tx.Exec(s.q(`
		UPDATE items SET deleted_at = NULL, updated_at = ?, seq = `+nextWorkspaceSeqSubquery+`
		WHERE id = ? AND deleted_at IS NOT NULL
	`), ts, existing.WorkspaceID, id)
	if err != nil {
		return nil, fmt.Errorf("restore item: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return nil, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetItem(id)
}

func (s *Store) SearchItems(workspaceID, query string) ([]ItemSearchResult, error) {
	// Whitespace-only queries collapse to empty after FTS5 sanitization and
	// would error on `MATCH ''`. Treat them as no-result rather than failing.
	// See BUG-818.
	if strings.TrimSpace(query) == "" {
		return []ItemSearchResult{}, nil
	}

	var sqlQuery string
	var args []interface{}

	if s.dialect.Driver() == DriverPostgres {
		// PostgreSQL: search_vector lives on the items table (aliased as "i").
		ftsSnippet := s.dialect.FTSSnippet("i", 1, "i.content")
		ftsMatch := s.dialect.FTSMatch("i", "search_vector")
		ftsRank := s.dialect.FTSRank("i", "search_vector")

		sqlQuery = fmt.Sprintf(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.seq, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''),
			       %s as snippet,
			       %s as rank_score
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE %s
			AND i.deleted_at IS NULL
		`, ftsSnippet, ftsRank, ftsMatch)
		// PG FTSSnippet, FTSRank, and FTSMatch each consume TWO "?" args
		// (raw query + hyphen-sanitized query) for the OR-combined
		// plainto_tsquery — see dialect.go and BUG-842.
		sanitized := sanitizePGFTSQuery(query)
		args = []interface{}{query, sanitized, query, sanitized, query, sanitized}
	} else {
		// SQLite: uses FTS5 virtual table "items_fts".
		ftsSnippet := s.dialect.FTSSnippet("items_fts", 1, "i.content")
		ftsMatch := s.dialect.FTSMatch("items_fts", "search_vector")
		ftsRank := s.dialect.FTSRank("items_fts", "search_vector")

		sqlQuery = fmt.Sprintf(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.seq, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''),
			       %s as snippet,
			       %s as rank_score
			FROM items_fts fts
			JOIN items i ON i.rowid = fts.rowid
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE %s
			AND i.deleted_at IS NULL
		`, ftsSnippet, ftsRank, ftsMatch)
		// Sanitize the user query so FTS5 special characters (hyphens, boolean
		// operators) are treated as literals — see BUG-818.
		args = []interface{}{sanitizeFTSQuery(query)}
	}

	if workspaceID != "" {
		sqlQuery += " AND i.workspace_id = ?"
		args = append(args, workspaceID)
	}

	if s.dialect.Driver() == DriverPostgres {
		sqlQuery += " ORDER BY rank_score DESC LIMIT 50"
	} else {
		sqlQuery += " ORDER BY rank_score LIMIT 50"
	}

	rows, err := s.db.Query(s.q(sqlQuery), args...)
	if err != nil {
		return nil, fmt.Errorf("search items: %w", err)
	}
	defer rows.Close()

	var results []ItemSearchResult
	for rows.Next() {
		var r ItemSearchResult
		var createdAt, updatedAt string
		var pinned bool
		if err := rows.Scan(
			&r.Item.ID, &r.Item.WorkspaceID, &r.Item.CollectionID, &r.Item.Title, &r.Item.Slug,
			&r.Item.Content, &r.Item.Fields, &r.Item.Tags,
			&pinned, &r.Item.SortOrder, &r.Item.ParentID, &r.Item.AssignedUserID, &r.Item.AgentRoleID, &r.Item.RoleSortOrder,
			&r.Item.CreatedBy, &r.Item.LastModifiedBy,
			&r.Item.Source, &r.Item.ItemNumber, &r.Item.Seq, &createdAt, &updatedAt,
			&r.Item.CollectionSlug, &r.Item.CollectionName, &r.Item.CollectionIcon, &r.Item.CollectionPrefix,
			&r.Item.AssignedUserName, &r.Item.AssignedUserEmail,
			&r.Item.AgentRoleName, &r.Item.AgentRoleSlug, &r.Item.AgentRoleIcon,
			&r.Snippet, &r.Rank,
		); err != nil {
			return nil, err
		}
		r.Item.Pinned = pinned
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		r.Item.ComputeRef()
		r.Item.Content = "" // Don't include full content in search results
		results = append(results, r)
	}
	return results, rows.Err()
}

// --- Item Links ---

func (s *Store) CreateItemLink(workspaceID string, input models.ItemLinkCreate, sourceID string) (*models.ItemLink, error) {
	id := newID()
	ts := now()

	linkType, err := models.NormalizeItemLinkType(input.LinkType)
	if err != nil {
		return nil, err
	}
	if sourceID == input.TargetID {
		return nil, fmt.Errorf("cannot link an item to itself")
	}
	createdBy := input.CreatedBy
	if createdBy == "" {
		createdBy = "user"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Codex round-3 P1: when this link puts `sourceID` into the
	// children-set of `target` (i.e. linkType ∈ childLinkTypes), lock
	// the target's parent-children key so a concurrent
	// UpdateItemWithPreCheck on the target can't read 0 open children
	// while we're about to attach a non-terminal one. Non-child link
	// types (blocks, supersedes, …) don't affect the children-set so
	// we skip the lock — keeps the common case lock-free.
	if isChildLinkType(linkType) {
		if err := s.AcquireParentChildrenLocks(tx, input.TargetID); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(s.q(`
		INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`), id, workspaceID, sourceID, input.TargetID, linkType, createdBy, ts); err != nil {
		return nil, fmt.Errorf("create item link: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create item link: %w", err)
	}

	return s.getItemLink(id)
}

// getItemLink is the unfiltered post-insert readback used by CreateItemLink to
// hydrate the freshly-inserted row with collection/source/target metadata. It
// intentionally does NOT filter on items.deleted_at IS NULL: the only caller
// is the immediate readback after INSERT, and a delete race against either
// endpoint would otherwise cause the just-successful insert to return nil
// (Codex review on PR #259). User-facing surfaces all read links via
// GetItemLinks (plural) or GetParentForItem, both of which DO filter.
func (s *Store) getItemLink(id string) (*models.ItemLink, error) {
	var link models.ItemLink
	var createdAt string

	var sourcePrefix, targetPrefix string
	var sourceItemNumber, targetItemNumber sql.NullInt64
	var sourceStatus, targetStatus sql.NullString

	srcStatus := s.dialect.JSONExtractText("s.fields", "status")
	tgtStatus := s.dialect.JSONExtractText("t.fields", "status")
	err := s.db.QueryRow(s.q(fmt.Sprintf(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title, s.slug, t.slug, sc.slug, tc.slug, sc.prefix, tc.prefix,
		       s.item_number, t.item_number,
		       %s, %s
		FROM item_links l
		JOIN items s ON s.id = l.source_id
		JOIN items t ON t.id = l.target_id
		JOIN collections sc ON sc.id = s.collection_id
		JOIN collections tc ON tc.id = t.collection_id
		WHERE l.id = ?
	`, srcStatus, tgtStatus)), id).Scan(
		&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
		&link.LinkType, &link.CreatedBy, &createdAt,
		&link.SourceTitle, &link.TargetTitle,
		&link.SourceSlug, &link.TargetSlug,
		&link.SourceCollectionSlug, &link.TargetCollectionSlug,
		&sourcePrefix, &targetPrefix,
		&sourceItemNumber, &targetItemNumber,
		&sourceStatus, &targetStatus,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item link: %w", err)
	}
	link.CreatedAt = parseTime(createdAt)
	if sourceItemNumber.Valid && sourcePrefix != "" {
		link.SourceRef = fmt.Sprintf("%s-%d", sourcePrefix, sourceItemNumber.Int64)
	}
	if targetItemNumber.Valid && targetPrefix != "" {
		link.TargetRef = fmt.Sprintf("%s-%d", targetPrefix, targetItemNumber.Int64)
	}
	if sourceStatus.Valid {
		link.SourceStatus = sourceStatus.String
	}
	if targetStatus.Valid {
		link.TargetStatus = targetStatus.String
	}
	return &link, nil
}

// GetItemLinks returns links where the given item is either source or target.
// Links pointing to or from soft-deleted items are filtered out so callers (e.g.
// `pad item related`, the lineage panel, the dashboard enrichment pass) don't
// surface dangling endpoints. The link rows themselves are preserved on disk —
// restoring a soft-deleted item resurrects its relationships automatically. See
// BUG-734.
func (s *Store) GetItemLinks(itemID string) ([]models.ItemLink, error) {
	srcStatusExpr := s.dialect.JSONExtractText("s.fields", "status")
	tgtStatusExpr := s.dialect.JSONExtractText("t.fields", "status")
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title, s.slug, t.slug, sc.slug, tc.slug, sc.prefix, tc.prefix,
		       s.item_number, t.item_number,
		       %s, %s
		FROM item_links l
		JOIN items s ON s.id = l.source_id AND s.deleted_at IS NULL
		JOIN items t ON t.id = l.target_id AND t.deleted_at IS NULL
		JOIN collections sc ON sc.id = s.collection_id
		JOIN collections tc ON tc.id = t.collection_id
		WHERE l.source_id = ? OR l.target_id = ?
		ORDER BY l.created_at DESC
	`, srcStatusExpr, tgtStatusExpr)), itemID, itemID)
	if err != nil {
		return nil, fmt.Errorf("get item links: %w", err)
	}
	defer rows.Close()

	var links []models.ItemLink
	for rows.Next() {
		var link models.ItemLink
		var createdAt string
		var sourcePrefix, targetPrefix string
		var sourceItemNumber, targetItemNumber sql.NullInt64
		var sourceStatus, targetStatus sql.NullString
		if err := rows.Scan(
			&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
			&link.LinkType, &link.CreatedBy, &createdAt,
			&link.SourceTitle, &link.TargetTitle,
			&link.SourceSlug, &link.TargetSlug,
			&link.SourceCollectionSlug, &link.TargetCollectionSlug,
			&sourcePrefix, &targetPrefix,
			&sourceItemNumber, &targetItemNumber,
			&sourceStatus, &targetStatus,
		); err != nil {
			return nil, err
		}
		link.CreatedAt = parseTime(createdAt)
		if sourceItemNumber.Valid && sourcePrefix != "" {
			link.SourceRef = fmt.Sprintf("%s-%d", sourcePrefix, sourceItemNumber.Int64)
		}
		if targetItemNumber.Valid && targetPrefix != "" {
			link.TargetRef = fmt.Sprintf("%s-%d", targetPrefix, targetItemNumber.Int64)
		}
		if sourceStatus.Valid {
			link.SourceStatus = sourceStatus.String
		}
		if targetStatus.Valid {
			link.TargetStatus = targetStatus.String
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// GetItemLinkByID returns a single item link by its ID, or nil if not found.
func (s *Store) GetItemLinkByID(id string) (*models.ItemLink, error) {
	var link models.ItemLink
	var createdAt string
	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, source_id, target_id, link_type, created_by, created_at
		FROM item_links WHERE id = ?
	`), id).Scan(&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
		&link.LinkType, &link.CreatedBy, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item link by id: %w", err)
	}
	link.CreatedAt = parseTime(createdAt)
	return &link, nil
}

func (s *Store) DeleteItemLink(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Codex round-3 P1: peek the link's type + target before deleting
	// so we can lock the target's parent-children key when this link
	// participates in the children-set. Without this, a concurrent
	// UpdateItemWithPreCheck on the target could read the child as
	// still attached, decide the parent has no open children, and
	// commit a terminal status while we orphan a non-terminal child.
	//
	// We DON'T lock for non-child link types (blocks, supersedes, …)
	// — they don't affect the children-set, so contention there is
	// unnecessary.
	var linkType, targetID string
	err = tx.QueryRow(s.q("SELECT link_type, target_id FROM item_links WHERE id = ?"), id).Scan(&linkType, &targetID)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("peek item link for delete: %w", err)
	}
	if isChildLinkType(linkType) {
		if err := s.AcquireParentChildrenLocks(tx, targetID); err != nil {
			return err
		}
	}

	result, err := tx.Exec(s.q("DELETE FROM item_links WHERE id = ?"), id)
	if err != nil {
		return fmt.Errorf("delete item link: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// --- Phase Links ---

// SetParentLink sets the parent for an item. Since an item can belong to at most
// one parent, this deletes any existing parent link for the item first.
// Includes cycle detection to prevent A→B→A or deeper ancestor loops.
//
// Codex round-3 P1: acquires `pad:parent-children:<id>` for BOTH the
// old parent (if any) AND the new parent in sorted order. That makes
// a concurrent UpdateItemWithPreCheck on either parent block on the
// same key, closing the link-mutation TOCTOU gap — without this, the
// guard could read 0 open children while this method was about to
// attach a non-terminal child.
func (s *Store) SetParentLink(workspaceID, itemID, parentID, createdBy string) (*models.ItemLink, error) {
	// Cycle detection: walk the ancestor chain from parentID to ensure itemID is not an ancestor.
	if err := s.checkParentCycle(itemID, parentID); err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Find the existing parent (if any) so we can lock against it too.
	// The DELETE below targets link_type='parent' specifically, which
	// matches what the guard's children query treats as the parent
	// edge (childLinkTypes includes 'parent'); other child-link types
	// like 'implements' aren't displaced by this method so we don't
	// need their old parent here.
	var oldParentID sql.NullString
	if err := tx.QueryRow(s.q(`
		SELECT target_id FROM item_links
		WHERE source_id = ? AND link_type = 'parent'
		LIMIT 1
	`), itemID).Scan(&oldParentID); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("lookup existing parent: %w", err)
	}

	if err := s.AcquireParentChildrenLocks(tx, oldParentID.String, parentID); err != nil {
		return nil, err
	}

	// Delete existing parent link for this item (if any)
	if _, err := tx.Exec(s.q(`DELETE FROM item_links WHERE source_id = ? AND link_type = 'parent'`), itemID); err != nil {
		return nil, fmt.Errorf("delete existing parent link: %w", err)
	}

	// Insert new parent link
	id := newID()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(s.q(`
		INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
		VALUES (?, ?, ?, ?, 'parent', ?, ?)
	`), id, workspaceID, itemID, parentID, createdBy, now); err != nil {
		return nil, fmt.Errorf("insert parent link: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit parent link: %w", err)
	}

	// Return the full link with enriched fields. Use the unfiltered readback
	// helper so that a delete race against either endpoint between commit and
	// readback doesn't cause the successful insert to surface as nil.
	return s.getItemLink(id)
}

// checkParentCycle walks the ancestor chain from parentID and returns an error
// if itemID is found (which would create a cycle).
func (s *Store) checkParentCycle(itemID, parentID string) error {
	visited := map[string]bool{itemID: true}
	current := parentID
	for {
		if visited[current] {
			return fmt.Errorf("cannot set parent: would create a cycle")
		}
		visited[current] = true

		// Look up the parent of current
		var targetID sql.NullString
		err := s.db.QueryRow(s.q(`
			SELECT target_id FROM item_links
			WHERE source_id = ? AND link_type = 'parent'
		`), current).Scan(&targetID)
		if err != nil || !targetID.Valid {
			break // no parent — no cycle
		}
		current = targetID.String
	}
	return nil
}

// ClearParentLink removes the parent link for an item.
//
// Codex round-3 P1: runs in a tx and acquires `pad:parent-children:<old>`
// before the DELETE so a concurrent UpdateItemWithPreCheck on the old
// parent blocks until this commit. Detaching a child is materially
// similar to attaching one — the parent's children-set changes either
// way and the guard must see a consistent view.
func (s *Store) ClearParentLink(itemID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var oldParentID sql.NullString
	if err := tx.QueryRow(s.q(`
		SELECT target_id FROM item_links
		WHERE source_id = ? AND link_type = 'parent'
		LIMIT 1
	`), itemID).Scan(&oldParentID); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("lookup parent for clear: %w", err)
	}
	if oldParentID.Valid && oldParentID.String != "" {
		if err := s.AcquireParentChildrenLocks(tx, oldParentID.String); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(s.q(`DELETE FROM item_links WHERE source_id = ? AND link_type = 'parent'`), itemID); err != nil {
		return fmt.Errorf("clear parent link: %w", err)
	}
	return tx.Commit()
}

// GetParentForItem returns the parent link for an item, or nil if it has no parent.
// A parent link pointing to a soft-deleted item is treated as no parent — the
// breadcrumb / lineage UI shouldn't show a deleted ancestor. See BUG-734.
func (s *Store) GetParentForItem(itemID string) (*models.ItemLink, error) {
	sStatusExpr := s.dialect.JSONExtractText("s.fields", "status")
	tStatusExpr := s.dialect.JSONExtractText("t.fields", "status")
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title, s.slug, t.slug, sc.slug, tc.slug, sc.prefix, tc.prefix,
		       s.item_number, t.item_number,
		       %s, %s
		FROM item_links l
		JOIN items s ON s.id = l.source_id AND s.deleted_at IS NULL
		JOIN items t ON t.id = l.target_id AND t.deleted_at IS NULL
		JOIN collections sc ON sc.id = s.collection_id
		JOIN collections tc ON tc.id = t.collection_id
		WHERE l.source_id = ? AND l.link_type IN (%s)
	`, sStatusExpr, tStatusExpr, childLinkTypeSQL())), itemID)
	if err != nil {
		return nil, fmt.Errorf("get parent for item: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, nil
	}

	var link models.ItemLink
	var createdAt string
	var sourcePrefix, targetPrefix string
	var sourceItemNumber, targetItemNumber sql.NullInt64
	var sourceStatus, targetStatus sql.NullString
	if err := rows.Scan(
		&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
		&link.LinkType, &link.CreatedBy, &createdAt,
		&link.SourceTitle, &link.TargetTitle,
		&link.SourceSlug, &link.TargetSlug,
		&link.SourceCollectionSlug, &link.TargetCollectionSlug,
		&sourcePrefix, &targetPrefix,
		&sourceItemNumber, &targetItemNumber,
		&sourceStatus, &targetStatus,
	); err != nil {
		return nil, fmt.Errorf("scan parent link: %w", err)
	}
	link.CreatedAt = parseTime(createdAt)
	if sourceItemNumber.Valid && sourcePrefix != "" {
		link.SourceRef = fmt.Sprintf("%s-%d", sourcePrefix, sourceItemNumber.Int64)
	}
	if targetItemNumber.Valid && targetPrefix != "" {
		link.TargetRef = fmt.Sprintf("%s-%d", targetPrefix, targetItemNumber.Int64)
	}
	if sourceStatus.Valid {
		link.SourceStatus = sourceStatus.String
	}
	if targetStatus.Valid {
		link.TargetStatus = targetStatus.String
	}
	return &link, nil
}

// GetParentMap returns a map of item ID -> parent item ID for all parent links
// in a workspace. Used for efficient batch lookups (e.g., dashboard, list enrichment).
//
// Links whose source or target item is soft-deleted are excluded so that
// dashboard orphan-detection (handlers_dashboard.go) and similar enrichment
// passes don't treat a task whose parent has been archived as still parented.
// See BUG-734.
func (s *Store) GetParentMap(workspaceID string) (map[string]string, error) {
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT il.source_id, il.target_id FROM item_links il
		JOIN items s ON s.id = il.source_id AND s.deleted_at IS NULL
		JOIN items t ON t.id = il.target_id AND t.deleted_at IS NULL
		WHERE il.workspace_id = ? AND il.link_type IN (%s)
	`, childLinkTypeSQL())), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("get parent map: %w", err)
	}
	defer rows.Close()

	m := make(map[string]string)
	for rows.Next() {
		var sourceID, targetID string
		if err := rows.Scan(&sourceID, &targetID); err != nil {
			return nil, err
		}
		m[sourceID] = targetID
	}
	return m, rows.Err()
}

// --- Child Item Progress ---

// GetItemProgress counts total and done child items linked to a parent via item_links.
// "Done" means the child item's done field (resolved from its collection's
// board_group_by, defaulting to status) matches one of that field's terminal
// options. Children from any collection count toward progress, and each
// child is evaluated against its own collection's done rules.
func (s *Store) GetItemProgress(parentItemID string) (total int, done int, err error) {
	filters := s.childrenDoneFiltersForParent(parentItemID)
	doneExpr, doneArgs := s.buildChildrenDoneExpr(filters, "i")
	args := append(doneArgs, parentItemID)
	err = s.db.QueryRow(s.q(fmt.Sprintf(`
		SELECT COUNT(*),
		       COUNT(CASE WHEN %s THEN 1 END)
		FROM items i
		JOIN item_links il ON il.source_id = i.id AND il.link_type IN (%s) AND il.target_id = ?
		WHERE i.deleted_at IS NULL
	`, doneExpr, childLinkTypeSQL())), args...).Scan(&total, &done)
	if err != nil {
		return 0, 0, fmt.Errorf("get item progress: %w", err)
	}
	return total, done, nil
}

// collectionDoneFilter describes how to evaluate "done" for a single child
// collection: which JSON key to read, and which values count as terminal.
type collectionDoneFilter struct {
	collectionID string
	doneKey      string
	values       []string
}

// childrenDoneFiltersForParent returns a filter per distinct child-item
// collection under the given parent. Each filter carries the child
// collection's resolved done field (honoring board_group_by) and terminal
// values so the caller can build a per-collection OR clause that evaluates
// each child against its own done rules.
//
// Soft-deleted collections are intentionally INCLUDED: progress-counting
// callers count items regardless of their collection's deleted_at, so
// excluding the collection here would leave those items without a
// matching per-collection clause and cause them to always evaluate as
// non-terminal. The collection row still carries valid schema + settings
// until a hard delete cascades, so the done rules remain applicable.
func (s *Store) childrenDoneFiltersForParent(parentItemID string) []collectionDoneFilter {
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT c.id, c.schema, c.settings
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		JOIN item_links il ON il.source_id = i.id AND il.link_type IN (%s) AND il.target_id = ?
		WHERE i.deleted_at IS NULL
	`, childLinkTypeSQL())), parentItemID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanCollectionDoneFilters(rows)
}

// doneFiltersForWorkspace returns a done-filter per collection in the
// workspace. Used by cross-collection queries (e.g. agent-role
// breakdowns) that need to evaluate "is done?" for every item regardless
// of which collection it belongs to.
//
// Includes soft-deleted collections: callers (e.g. GetRoleBreakdown)
// count items in the workspace without filtering by collection
// deleted_at, so excluding soft-deleted collections here would leave
// their items without a matching per-collection clause and cause them
// to always register as non-terminal.
func (s *Store) doneFiltersForWorkspace(workspaceID string) []collectionDoneFilter {
	rows, err := s.db.Query(
		s.q(`SELECT id, schema, settings FROM collections WHERE workspace_id = ?`),
		workspaceID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanCollectionDoneFilters(rows)
}

// childrenDoneFiltersForCollection is the batch version: it gathers one
// filter per distinct child-item collection across all parent→child links
// for parents in a given (workspace, collectionSlug).
//
// Includes soft-deleted child collections for the same reason as
// childrenDoneFiltersForParent — callers count items regardless of their
// collection's deleted_at, and we want items from soft-deleted
// collections to still be evaluated against their own done rules.
//
// includeArchived mirrors the same flag in GetAllItemProgress: when true,
// the parent-row join does NOT filter out archived parents. This matters
// because if a child collection's only parent links point to archived
// parents, the collection would be absent from the filter map under the
// live-only predicate — causing those children to fall back to default
// done semantics and producing wrong done counts in the main query.
func (s *Store) childrenDoneFiltersForCollection(workspaceID, collectionSlug string, includeArchived bool) []collectionDoneFilter {
	parentDeletedFilter := "AND p.deleted_at IS NULL"
	if includeArchived {
		parentDeletedFilter = ""
	}
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT c.id, c.schema, c.settings
		FROM items t
		JOIN collections c ON c.id = t.collection_id
		JOIN item_links il ON il.source_id = t.id AND il.link_type IN (%s)
		JOIN items p ON p.id = il.target_id %s
		JOIN collections pc ON pc.id = p.collection_id AND pc.slug = ?
		WHERE p.workspace_id = ?
		  AND t.deleted_at IS NULL
	`, childLinkTypeSQL(), parentDeletedFilter)), collectionSlug, workspaceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanCollectionDoneFilters(rows)
}

// scanCollectionDoneFilters consumes rows yielding (id, schema, settings)
// and resolves each into a collectionDoneFilter.
//
// When a collection's schema fails to parse we still emit a filter — one
// that falls back to the `status` field and the global default terminal
// list. Silently skipping the collection would leave its items without a
// matching per-collection clause in buildChildrenDoneExpr, so they'd
// always register as non-terminal in progress / role / starred queries
// (a malformed schema on one collection would skew counts on every
// parent-progress computation).
func scanCollectionDoneFilters(rows *sql.Rows) []collectionDoneFilter {
	var filters []collectionDoneFilter
	for rows.Next() {
		var id, schemaJSON, settingsJSON string
		if err := rows.Scan(&id, &schemaJSON, &settingsJSON); err != nil {
			continue
		}
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
			// Malformed schema → emit a default-fallback filter so the
			// collection's items still get evaluated against the status
			// column + global default terminals. This matches pre-TASK-604
			// behavior for those items.
			filters = append(filters, collectionDoneFilter{
				collectionID: id,
				doneKey:      "status",
				values:       models.DefaultTerminalStatuses,
			})
			continue
		}
		var settings models.CollectionSettings
		if settingsJSON != "" {
			_ = json.Unmarshal([]byte(settingsJSON), &settings)
		}
		key, values := models.TerminalValuesForDoneField(schema, settings)
		filters = append(filters, collectionDoneFilter{
			collectionID: id,
			doneKey:      key,
			values:       values,
		})
	}
	return filters
}

// buildChildrenDoneExpr compiles a set of per-collection done filters into
// a single SQL boolean expression plus ordered args. `itemAlias` is the
// item-table alias in the outer query (e.g. "i" for GetItemProgress, "t"
// for GetAllItemProgress).
//
// Expression shape:
//
//	((<alias>.collection_id = ? AND LOWER(COALESCE(<field_A>, '')) IN (?,?)) OR
//	 (<alias>.collection_id = ? AND LOWER(COALESCE(<field_B>, '')) IN (?,?)))
//
// The `<field_X>` JSON extract uses scalar text extraction; this works
// because DoneFieldKey in the models package only resolves done fields to
// `select` typed columns (see that function's doc). multi_select-backed
// done fields would store their values as a JSON array and scalar IN
// matching would silently miss them — hence the upstream restriction.
//
// If no filters were constructed (no child collections discovered, or all
// of their schemas failed to parse), falls back to checking <alias>.status
// against the global default terminal list — mirroring the legacy behavior
// so dashboards for untyped collections keep working.
func (s *Store) buildChildrenDoneExpr(filters []collectionDoneFilter, itemAlias string) (string, []any) {
	if len(filters) == 0 {
		statusExpr := s.dialect.JSONExtractText(itemAlias+".fields", "status")
		placeholders, args := models.DefaultTerminalStatusPlaceholders()
		return fmt.Sprintf("LOWER(COALESCE(%s, '')) IN (%s)", statusExpr, placeholders), args
	}
	clauses := make([]string, 0, len(filters))
	args := make([]any, 0, len(filters)*4)
	for _, f := range filters {
		fieldExpr := s.dialect.JSONExtractText(itemAlias+".fields", f.doneKey)
		placeholders := make([]string, len(f.values))
		args = append(args, f.collectionID)
		for i, v := range f.values {
			placeholders[i] = "?"
			args = append(args, strings.ToLower(v))
		}
		clauses = append(clauses, fmt.Sprintf(
			"(%s.collection_id = ? AND LOWER(COALESCE(%s, '')) IN (%s))",
			itemAlias, fieldExpr, strings.Join(placeholders, ","),
		))
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// nonTerminalFilter builds a WHERE fragment (plus ordered args) that keeps
// only items whose resolved done-field value is NOT one of their
// collection's terminal options. It reuses the same per-collection done
// machinery as parent-progress (doneFiltersForWorkspace +
// buildChildrenDoneExpr): buildChildrenDoneExpr yields an expression that
// is TRUE when an item is terminal, so negating it selects the
// non-terminal set.
//
// Each collection is evaluated against its OWN terminal_options (with the
// global DefaultTerminalStatuses fallback for schemas that declare none),
// so collections with custom status vocabularies are handled correctly
// rather than against a hardcoded global allowlist (BUG-2001). This is the
// server-side default that both the CLI (`pad item list` with no --status/
// --all) and the MCP `pad_item.action=list` inherit.
//
// itemAlias is the item-table alias in the outer query (e.g. "i").
func (s *Store) nonTerminalFilter(workspaceID, itemAlias string) (string, []any) {
	filters := s.doneFiltersForWorkspace(workspaceID)
	doneExpr, args := s.buildChildrenDoneExpr(filters, itemAlias)
	return "NOT " + doneExpr, args
}

// ItemProgress holds child item completion counts for a single parent item.
type ItemProgress struct {
	ItemID string `json:"item_id"`
	Total  int    `json:"total"`
	Done   int    `json:"done"`
}

// GetAllItemProgress returns child item completion counts for every item in
// the given collection within a workspace.
//
// includeArchived controls whether soft-deleted parent items contribute rows.
// When false (the default for /plans-progress) only live parents are returned.
// When true (used by /child-progress with include_archived=true) archived
// parents also appear — matching the archived-toggle semantics on the
// collection page (mirrors CollectionCheckboxProgress's includeArchived param).
func (s *Store) GetAllItemProgress(workspaceID, collectionSlug string, includeArchived bool) ([]ItemProgress, error) {
	filters := s.childrenDoneFiltersForCollection(workspaceID, collectionSlug, includeArchived)
	doneExpr, doneArgs := s.buildChildrenDoneExpr(filters, "t")
	args := append(doneArgs, workspaceID, collectionSlug)
	parentDeletedFilter := "AND p.deleted_at IS NULL"
	if includeArchived {
		parentDeletedFilter = ""
	}
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT p.id,
		       COUNT(t.id),
		       COUNT(CASE WHEN t.id IS NOT NULL AND %s THEN 1 END)
		FROM items p
		JOIN collections pc ON pc.id = p.collection_id
		LEFT JOIN item_links il ON il.link_type IN (%s) AND il.target_id = p.id
		LEFT JOIN items t ON t.id = il.source_id
		                  AND t.deleted_at IS NULL
		WHERE p.workspace_id = ?
		  AND pc.slug = ?
		  %s
		GROUP BY p.id
	`, doneExpr, childLinkTypeSQL(), parentDeletedFilter)), args...)
	if err != nil {
		return nil, fmt.Errorf("get all item progress: %w", err)
	}
	defer rows.Close()

	var result []ItemProgress
	for rows.Next() {
		var ip ItemProgress
		if err := rows.Scan(&ip.ItemID, &ip.Total, &ip.Done); err != nil {
			return nil, fmt.Errorf("scan item progress: %w", err)
		}
		result = append(result, ip)
	}
	if result == nil {
		result = []ItemProgress{}
	}
	return result, rows.Err()
}

// GetChildItems returns all non-deleted child items linked to the given parent
// via item_links. Returns children from any collection.
func (s *Store) GetChildItems(parentItemID string) ([]models.Item, error) {
	return s.getChildItems(s.db, parentItemID)
}

// GetChildItemsTx is the in-transaction variant of GetChildItems. The
// underlying query is the same as GetChildItems; using a *sql.Tx ties
// the read to the caller's transaction so it sees the same snapshot
// the subsequent UPDATE will write against (IDEA-1494 R2).
//
// Atomicity vs. concurrent child mutations is provided by the caller's
// transaction-scoped locking:
//
//   - SQLite: db-wide BEGIN IMMEDIATE write lock (set globally via
//     `_txlock=immediate`) serializes all writers, so any concurrent
//     child insert / child update blocks until this tx commits or
//     rolls back. No additional locking is needed.
//   - Postgres: the caller is expected to hold a parent-keyed advisory
//     lock (see AcquireParentChildrenLocks below) so concurrent
//     mutations on the same parent's children are serialized against
//     this read.
//
// FOR UPDATE is intentionally NOT used — the underlying SELECT carries
// DISTINCT (necessary because item_links can carry both `parent` and
// the legacy `plan` link_type for the same edge), and Postgres rejects
// `SELECT DISTINCT … FOR UPDATE`. The advisory-lock pattern sidesteps
// that constraint while still giving us a serialized snapshot.
func (s *Store) GetChildItemsTx(tx *sql.Tx, parentItemID string) ([]models.Item, error) {
	if tx == nil {
		return s.GetChildItems(parentItemID)
	}
	return s.getChildItems(tx, parentItemID)
}

// acquireParentChildrenLocksForUpdate is the in-tx helper UpdateItem
// uses to serialize itself against the open-children guard
// (IDEA-1494 R2 / R4). It acquires the parent-children advisory lock
// for:
//
//  1. EVERY parent the item is currently a child of (via item_links
//     of type ∈ childLinkTypes — `parent` AND `implements`).
//     childLinkTypes is the inclusion rule GetChildItems walks, so
//     this set is exactly the parents whose guard precheck could see
//     this item as a child.
//  2. THIS item itself, as a parent — so any concurrent precheck
//     running against this item's own children list waits.
//
// Codex round-4 P1: pre-fix this helper used `LIMIT 1` and only
// locked one parent. A child of TWO parents (one via `parent`, one
// via `implements`) would let a status flip race against the
// un-locked parent's precheck. The query now returns ALL distinct
// parent target_ids and we lock every one.
//
// All keys (parents + self) are funnelled through
// AcquireParentChildrenLocks so they're acquired in a single,
// canonical sorted order — round-4 P2's deadlock-avoidance contract.
// No call site outside this helper takes pad:parent-children:* locks
// in any other order.
func (s *Store) acquireParentChildrenLocksForUpdate(tx *sql.Tx, itemID string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	parentIDs, err := s.listParentChildLockKeys(tx, itemID)
	if err != nil {
		return err
	}
	keys := append(parentIDs, itemID)
	return s.AcquireParentChildrenLocks(tx, keys...)
}

// listParentChildLockKeys returns every target_id this item is the
// `source_id` of under a childLinkTypes link — i.e. every parent
// whose children-set includes this item. Used wherever we need to
// lock all of an item's parents at once (UpdateItem, RestoreItem,
// link-mutation paths).
//
// IMPORTANT: this MUST stay in lockstep with childLinkTypes (the
// inclusion rule GetChildItems uses). If a new link type joins the
// children-set, both the query here and the read query must add it
// together so lock coverage matches read coverage.
func (s *Store) listParentChildLockKeys(tx *sql.Tx, itemID string) ([]string, error) {
	rows, err := tx.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT target_id FROM item_links
		WHERE source_id = ? AND link_type IN (%s)
	`, childLinkTypeSQL())), itemID)
	if err != nil {
		return nil, fmt.Errorf("list parent lock keys: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan parent lock key: %w", err)
		}
		if id == "" || id == itemID {
			continue
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AcquireParentChildrenLocks is the CANONICAL helper for taking
// `pad:parent-children:<id>` advisory locks. Every call site that
// needs to serialize against the open-children guard MUST go through
// this function — UpdateItemWithPreCheck precheck, MoveItemWithPreCheck
// precheck, RestoreItem, SetParentLink, ClearParentLink,
// CreateItemLink (child-link types), DeleteItemLink (child-link
// types). Ad-hoc single-key acquisition outside this helper is
// FORBIDDEN — two call sites taking distinct keys in different
// orders WILL deadlock under contention (the classic AB/BA shape).
//
// The contract this helper enforces:
//
//  1. Deduplicate. Repeated IDs in the input collapse to one lock.
//  2. Drop empties. "" / nil entries don't get locked.
//  3. Acquire in canonical sorted order (string-sort by ID).
//     Two concurrent callers that share any subset of IDs always
//     grab the overlap in the same order → no deadlock.
//
// SQLite is a no-op because the global BEGIN IMMEDIATE write lock
// (set via _txlock=immediate in store.go) already serializes every
// writer; advisory locks would add no protection there.
//
// Per Codex round-3 P1 (link-mutations bypass) + round-4 P2
// (lock-order asymmetry). If you find yourself writing
// `pg_advisory_xact_lock(... 'pad:parent-children:' ...)` anywhere
// outside this helper, route it through here instead.
func (s *Store) AcquireParentChildrenLocks(tx *sql.Tx, parentItemIDs ...string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	seen := make(map[string]struct{}, len(parentItemIDs))
	keys := make([]string, 0, len(parentItemIDs))
	for _, id := range parentItemIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('pad:parent-children:' || $1))", k); err != nil {
			return fmt.Errorf("acquire parent-children lock %q: %w", k, err)
		}
	}
	return nil
}

// isChildLinkType reports whether the given link type is one the
// open-children guard counts toward the children-set (i.e. matches
// the inclusion rule baked into `childLinkTypes` and used by
// GetChildItems via `childLinkTypeSQL()`). Single source of truth so
// link-writer lock acquisition can't drift from the read query's set.
func isChildLinkType(linkType string) bool {
	for _, t := range childLinkTypes {
		if t == linkType {
			return true
		}
	}
	return false
}

// childQueryer is the small surface the children-list query needs from
// either *sql.DB or *sql.Tx. Lets getChildItems serve both the unlocked
// and tx-bound paths from one implementation.
type childQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *Store) getChildItems(q childQueryer, parentItemID string) ([]models.Item, error) {
	rows, err := q.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''), i.deleted_at
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		JOIN item_links il ON il.source_id = i.id AND il.link_type IN (%s) AND il.target_id = ?
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.deleted_at IS NULL
		ORDER BY i.sort_order ASC, i.created_at ASC
	`, childLinkTypeSQL())), parentItemID)
	if err != nil {
		return nil, fmt.Errorf("get child items: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

// PopulateHasChildren sets HasChildren=true on items that have at least one
// child linked via parent link_type. Operates in-place on the slice.
func (s *Store) PopulateHasChildren(items []models.Item) {
	if len(items) == 0 {
		return
	}

	// Build ID list and index
	ids := make([]string, len(items))
	idx := make(map[string]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
		idx[item.ID] = i
	}

	// Batch query: which of these IDs are targets of a parent link?
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT DISTINCT il.target_id FROM item_links il
		JOIN items child ON child.id = il.source_id AND child.deleted_at IS NULL
		WHERE il.link_type IN (%s) AND il.target_id IN (%s)
	`, childLinkTypeSQL(), strings.Join(placeholders, ","))

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return // best-effort; don't fail the whole request
	}
	defer rows.Close()

	for rows.Next() {
		var targetID string
		if err := rows.Scan(&targetID); err != nil {
			continue
		}
		if i, ok := idx[targetID]; ok {
			items[i].HasChildren = true
		}
	}
}

// MoveItem moves an item to a different collection within the same workspace.
// It updates the collection_id and fields JSON. The item_number is preserved
// because numbering is workspace-global — the number stays the same, only the
// collection prefix changes (e.g. IDEA-42 → BUG-42).
//
// The move also bumps the workspace-scoped seq so delta-sync clients
// see the collection change (PLAN-1343 / TASK-1352). Without it a
// client polling /items-changes?since=cursor would render the item
// under its old collection until a full refresh.
func (s *Store) MoveItem(itemID, targetCollectionID, newFieldsJSON string) (*models.Item, error) {
	return s.MoveItemWithPreCheck(itemID, targetCollectionID, newFieldsJSON, nil)
}

// MoveItemWithPreCheck is MoveItem with the same precheck escape hatch
// UpdateItemWithPreCheck offers. Codex round-3 P1: a `pad item move
// ... --field status=done` writes a terminal done-field value through
// MoveItem, bypassing the open-children guard wired into the regular
// UpdateItem path. This variant runs the caller's invariant check
// inside the move's transaction, after acquiring the workspace seq
// lock + the parent-children lock for this item's own parent (it CAN
// itself be a parent — children stay attached across collection
// changes — so we lock for itself too, matching
// acquireParentChildrenLocksForUpdate's shape).
//
// The precheck receives a fresh in-tx snapshot of the item, same as
// UpdateItemWithPreCheck (the pre-tx `existing` is replaced).
func (s *Store) MoveItemWithPreCheck(
	itemID, targetCollectionID, newFieldsJSON string,
	precheck func(tx *sql.Tx, existing *models.Item) error,
) (*models.Item, error) {
	existing, err := s.GetItem(itemID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, sql.ErrNoRows
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := s.acquireWorkspaceSeqLock(tx, existing.WorkspaceID); err != nil {
		return nil, err
	}

	if err := s.acquireParentChildrenLocksForUpdate(tx, itemID); err != nil {
		return nil, err
	}

	if precheck != nil {
		freshExisting, ferr := s.getItemTx(tx, itemID)
		if ferr != nil {
			return nil, fmt.Errorf("re-read item under lock: %w", ferr)
		}
		if freshExisting == nil {
			return nil, sql.ErrNoRows
		}
		if err := precheck(tx, freshExisting); err != nil {
			return nil, err
		}
		existing = freshExisting
	}

	// Capture the pre-move status under lock before the UPDATE, mirroring the
	// UpdateItemWithPreCheck path: `existing` is the fresh in-tx snapshot when
	// a precheck ran, otherwise re-read so a concurrent write doesn't make
	// from_status stale. The done field resolves against the TARGET collection
	// (where the item now lives and which reports group by); for a move that
	// also crosses to a collection with a different done field, the old value
	// read through that key may be empty, which correctly reads as "entered".
	moveDoneKey := s.doneFieldKey(targetCollectionID)
	oldFields := existing.Fields
	if precheck == nil {
		if fresh, ferr := s.getItemTx(tx, itemID); ferr == nil && fresh != nil {
			oldFields = fresh.Fields
		}
	}

	moveTS := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.Exec(s.q(`
		UPDATE items
		SET collection_id = ?, fields = ?, updated_at = ?, seq = `+nextWorkspaceSeqSubquery+`
		WHERE id = ? AND deleted_at IS NULL`),
		targetCollectionID, newFieldsJSON, moveTS, existing.WorkspaceID, itemID)
	if err != nil {
		return nil, fmt.Errorf("move item: %w", err)
	}

	// A move can carry a status-changing field override (e.g.
	// `pad item move ... --field status=done`), which rewrites `fields`
	// outside the UpdateItemWithPreCheck path. Record the transition here
	// too so status_transitions stays the canonical source for reports
	// (PLAN-1628 / TASK-1637). collection_id reflects the TARGET collection
	// the item now lives in. Same tx, not debounced.
	oldStatus := extractFieldValue(oldFields, moveDoneKey)
	newStatus := extractFieldValue(newFieldsJSON, moveDoneKey)
	// Record any done-field change, including a clear (X → "") — see the
	// UpdateItemWithPreCheck hook for the rationale.
	if newStatus != oldStatus {
		if _, err = tx.Exec(s.q(`
			INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+nextTransitionSeqSubquery+`)
		`), newID(), itemID, existing.WorkspaceID, targetCollectionID, moveDoneKey, oldStatus, newStatus, moveTS); err != nil {
			return nil, fmt.Errorf("record status transition on move: %w", err)
		}
	}

	// Durable cross-collection move record (BUG-1675), written in the
	// SAME tx as the move so /items-changes' moved-out tombstone can't
	// depend on the best-effort post-commit activity row. Skip same-
	// collection no-ops (move callers reject those upstream, but guard
	// anyway). The seq is the value the UPDATE just assigned — read it
	// back under the still-held lock so the tombstone seq matches what
	// /items-changes sees for the item.
	if targetCollectionID != existing.CollectionID {
		var moveSeq int64
		if err = tx.QueryRow(s.q(`SELECT seq FROM items WHERE id = ?`), itemID).Scan(&moveSeq); err != nil {
			return nil, fmt.Errorf("read post-move seq: %w", err)
		}
		if _, err = tx.Exec(s.q(`
			INSERT INTO item_collection_moves (id, workspace_id, item_id, from_collection_id, to_collection_id, seq, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`), newID(), existing.WorkspaceID, itemID, existing.CollectionID, targetCollectionID, moveSeq, moveTS); err != nil {
			return nil, fmt.Errorf("record collection move: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetItem(itemID)
}

// --- Helpers ---

// validSortField matches safe field names (alphanumeric + underscore, starting with a letter).
var validSortField = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

func buildItemSort(sort string, dialect Dialect) string {
	if sort == "" {
		return " ORDER BY i.pinned DESC, i.updated_at DESC"
	}

	var parts []string
	for _, seg := range strings.Split(sort, ",") {
		seg = strings.TrimSpace(seg)
		tokens := strings.SplitN(seg, ":", 2)
		col := tokens[0]
		dir := "ASC"
		if len(tokens) == 2 && strings.ToUpper(tokens[1]) == "DESC" {
			dir = "DESC"
		}

		switch col {
		case "title":
			parts = append(parts, fmt.Sprintf("i.title %s", dir))
		case "created_at":
			parts = append(parts, fmt.Sprintf("i.created_at %s", dir))
		case "updated_at":
			parts = append(parts, fmt.Sprintf("i.updated_at %s", dir))
		case "sort_order":
			parts = append(parts, fmt.Sprintf("i.sort_order %s", dir))
		default:
			// For field-based sorting, use dialect JSON extract — validate the field name
			// to prevent SQL injection via crafted sort parameters.
			if !validSortField.MatchString(col) {
				continue // skip invalid field names
			}
			parts = append(parts, fmt.Sprintf("%s %s", dialect.JSONExtractText("i.fields", col), dir))
		}
	}

	if len(parts) == 0 {
		return " ORDER BY i.pinned DESC, i.updated_at DESC"
	}
	return " ORDER BY " + strings.Join(parts, ", ")
}

// shouldCreateItemVersion mirrors ShouldCreateVersion but queries item_versions.
func (s *Store) shouldCreateItemVersion(itemID, actor, source string) (bool, error) {
	var createdBy, src, createdAt string
	err := s.db.QueryRow(s.q(`
		SELECT created_by, source, created_at
		FROM item_versions
		WHERE item_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`), itemID).Scan(&createdBy, &src, &createdAt)
	if err == sql.ErrNoRows {
		return true, nil // No versions yet
	}
	if err != nil {
		return false, err
	}

	// Actor or source changed — always snapshot
	if createdBy != actor || src != source {
		return true, nil
	}

	// Throttle
	lastTime := parseTime(createdAt)
	return time.Since(lastTime) >= VersionThrottleInterval, nil
}

// ListItemVersionsResolved returns versions with full content (diffs resolved).
// Requires the current item content to reconstruct diff-based versions.
func (s *Store) ListItemVersionsResolved(itemID, currentContent string) ([]models.Version, error) {
	versions, err := s.ListItemVersions(itemID)
	if err != nil {
		return nil, err
	}

	// Resolve diffs: walk from newest to oldest, applying reverse patches.
	content := currentContent
	for i := range versions {
		if !versions[i].IsDiff {
			content = versions[i].Content
			continue
		}
		resolved, applyErr := diff.ApplyPatch(content, versions[i].Content)
		if applyErr != nil {
			versions[i].Content = fmt.Sprintf("[patch error: %v]", applyErr)
			versions[i].IsDiff = false
			continue
		}
		versions[i].Content = resolved
		versions[i].IsDiff = false
		content = resolved
	}
	return versions, nil
}

// GetItemVersionResolved returns a single version with its diff resolved to
// full content. Reverse-patch versions can only be reconstructed by walking the
// chain from current content newest→oldest, so this resolves the whole chain and
// returns the requested row. Used by the timeline's lazy "resolve on expand" path
// (BUG-1612) — the paginated timeline serves raw patch text, so the card fetches
// real content only when a diff version is expanded. Returns nil if not found.
func (s *Store) GetItemVersionResolved(itemID, versionID, currentContent string) (*models.Version, error) {
	versions, err := s.ListItemVersionsResolved(itemID, currentContent)
	if err != nil {
		return nil, err
	}
	for i := range versions {
		if versions[i].ID == versionID {
			return &versions[i], nil
		}
	}
	return nil, nil
}

// ListItemVersionsBeforeTime returns versions for an item created before the given time,
// ordered newest-first, limited to `limit` results. Used for cursor-based timeline pagination.
//
// When beforeID is empty (first page / no cursor), the secondary id tie-breaker
// is omitted. See ListCommentsBeforeTime for the rationale (BUG-1086).
func (s *Store) ListItemVersionsBeforeTime(itemID string, before time.Time, beforeID string, limit int) ([]models.Version, error) {
	ts := before.Format(time.RFC3339)
	const selectCols = `id, item_id, content, change_summary, created_by, source, is_diff, created_at`
	const orderLimit = `ORDER BY created_at DESC, id DESC LIMIT ?`

	var rows *sql.Rows
	var err error
	if beforeID == "" {
		rows, err = s.db.Query(s.q(`
			SELECT `+selectCols+`
			FROM item_versions
			WHERE item_id = ? AND created_at < ?
			`+orderLimit), itemID, ts, limit)
	} else {
		rows, err = s.db.Query(s.q(`
			SELECT `+selectCols+`
			FROM item_versions
			WHERE item_id = ? AND (created_at < ? OR (created_at = ? AND id < ?))
			`+orderLimit), itemID, ts, ts, beforeID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []models.Version
	for rows.Next() {
		var v models.Version
		var createdAt string
		var isDiff bool
		if err := rows.Scan(&v.ID, &v.DocumentID, &v.Content, &v.ChangeSummary, &v.CreatedBy, &v.Source, &isDiff, &createdAt); err != nil {
			return nil, err
		}
		v.IsDiff = isDiff
		v.CreatedAt = parseTime(createdAt)
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// ListItemVersions returns all versions for an item.
func (s *Store) ListItemVersions(itemID string) ([]models.Version, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, item_id, content, change_summary, created_by, source, is_diff, created_at
		FROM item_versions
		WHERE item_id = ?
		ORDER BY created_at DESC
	`), itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []models.Version
	for rows.Next() {
		var v models.Version
		var createdAt string
		var isDiff bool
		if err := rows.Scan(&v.ID, &v.DocumentID, &v.Content, &v.ChangeSummary, &v.CreatedBy, &v.Source, &isDiff, &createdAt); err != nil {
			return nil, err
		}
		v.IsDiff = isDiff
		v.CreatedAt = parseTime(createdAt)
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func scanItems(rows *sql.Rows) ([]models.Item, error) {
	var items []models.Item
	for rows.Next() {
		var item models.Item
		var createdAt, updatedAt string
		var deletedAt *string
		var pinned bool
		if err := rows.Scan(
			&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
			&item.Content, &item.Fields, &item.Tags,
			&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
			&item.CreatedBy, &item.LastModifiedBy, &item.Source,
			&item.ItemNumber, &item.Seq, &createdAt, &updatedAt,
			&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
			&item.AssignedUserName, &item.AssignedUserEmail,
			&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
			&deletedAt,
		); err != nil {
			return nil, err
		}
		item.Pinned = pinned
		item.CreatedAt = parseTime(createdAt)
		item.UpdatedAt = parseTime(updatedAt)
		item.DeletedAt = parseTimePtr(deletedAt)
		hydrateItemComputedMetadata(&item)
		items = append(items, item)
	}
	return items, rows.Err()
}

// ItemsModifiedSince returns items in a workspace that were updated after the
// given timestamp. Used for incremental sync on tab resume. Also returns IDs of
// items that were deleted (hard-deleted or archived) since the timestamp.
//
// The updated list includes both active AND recently archived items (those with
// deleted_at > since). This lets the frontend update archived views correctly —
// an item that was just archived needs its full data to appear in archived views,
// not just its ID in the deleted list.
func (s *Store) ItemsModifiedSince(workspaceID string, since time.Time) (updated []models.Item, deletedIDs []string, err error) {
	sinceStr := since.UTC().Format(time.RFC3339)

	// Fetch updated items: active items modified since the timestamp,
	// PLUS items archived since the timestamp (so archived views can update).
	query := s.q(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''), i.deleted_at
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.workspace_id = ?
		  AND i.updated_at > ?
		  AND (i.deleted_at IS NULL OR i.deleted_at > ?)
		ORDER BY i.updated_at ASC
	`)

	rows, err := s.db.Query(query, workspaceID, sinceStr, sinceStr)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	updated, err = scanItems(rows)
	if err != nil {
		return nil, nil, err
	}

	// Fetch IDs of items deleted since the timestamp.
	delQuery := s.q(`
		SELECT id FROM items
		WHERE workspace_id = ?
		  AND deleted_at IS NOT NULL
		  AND deleted_at > ?
	`)
	delRows, err := s.db.Query(delQuery, workspaceID, sinceStr)
	if err != nil {
		return updated, nil, err
	}
	defer delRows.Close()
	for delRows.Next() {
		var id string
		if err := delRows.Scan(&id); err != nil {
			return updated, nil, err
		}
		deletedIDs = append(deletedIDs, id)
	}

	return updated, deletedIDs, delRows.Err()
}

// ItemCollectionRef is a minimal item reference with collection ID, used for
// filtering deleted items by collection visibility.
type ItemCollectionRef struct {
	ID           string
	CollectionID string
}

// GetDeletedItemsWithCollection returns minimal item info (ID + CollectionID)
// for soft-deleted items, used to filter deleted item IDs by collection visibility.
//
// Despite the name, this is just GetItemCollectionRefs under the hood — the
// underlying query has no deleted_at filter, so it works for any item state.
// The name is kept for this call site's existing meaning (its caller only
// ever passes already-known-deleted IDs); BUG-1928 needed the same
// state-agnostic lookup for live-or-deleted item grants, hence the rename
// of the shared implementation to the more accurate GetItemCollectionRefs.
func (s *Store) GetDeletedItemsWithCollection(workspaceID string, itemIDs []string) ([]ItemCollectionRef, error) {
	return s.GetItemCollectionRefs(workspaceID, itemIDs)
}

// GetItemCollectionRefs returns minimal item info (ID + CollectionID) for the
// given item IDs, scoped to the workspace. State-agnostic: the query has no
// deleted_at filter, so it resolves live and soft-deleted items alike. Used
// wherever a caller needs an item_id → collection_id mapping without paying
// for a full item fetch (e.g. bulk visibility filtering).
func (s *Store) GetItemCollectionRefs(workspaceID string, itemIDs []string) ([]ItemCollectionRef, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(itemIDs))
	args := []interface{}{workspaceID}
	for i, id := range itemIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT id, collection_id FROM items
		WHERE workspace_id = ? AND id IN (%s)
	`, strings.Join(placeholders, ","))), args...)
	if err != nil {
		return nil, fmt.Errorf("get item collection refs: %w", err)
	}
	defer rows.Close()
	var results []ItemCollectionRef
	for rows.Next() {
		var r ItemCollectionRef
		if err := rows.Scan(&r.ID, &r.CollectionID); err != nil {
			return nil, fmt.Errorf("scan item collection ref: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// WorkspaceHasAgentActivity reports whether any non-deleted item VISIBLE
// to the caller was created via an agent surface — direct CLI invocation
// or the Remote MCP transport. Used by the dashboard to auto-hide the
// "connect an agent" banner once a workspace's agent loop is wired up.
//
// "Agent activity" is the union of two source values:
//
//   - source='cli': set by both the direct `pad` CLI and the
//     HTTPHandlerDispatcher used by the Remote MCP transport, which
//     deliberately mirrors CLI attribution (see dispatch_http_test.go).
//     Today, this single value covers both surfaces.
//   - source='mcp': reserved for future code paths that may want to
//     distinguish MCP attribution from CLI. Included here defensively so
//     the query keeps working if attribution is later split.
//
// Visibility filtering matches the dashboard's existing model (see
// handleGetDashboard): an item counts when its collection is in
// collectionIDs OR its id is in itemIDs (union — guest item-level grants
// can expose items in otherwise-hidden collections). Pass nil for both to
// run unfiltered (full-visibility caller). A non-nil empty
// collectionIDs slice with no itemIDs means "no visible collections" and
// returns false without hitting the DB — symmetric with ListItems.
//
// Backed by EXISTS so it short-circuits on the first match.
func (s *Store) WorkspaceHasAgentActivity(workspaceID string, collectionIDs, itemIDs []string) (bool, error) {
	// Symmetric early-exit with ListItems: caller signaled no visibility.
	if collectionIDs != nil && len(collectionIDs) == 0 && len(itemIDs) == 0 {
		return false, nil
	}

	query := `
		SELECT EXISTS(
			SELECT 1 FROM items
			WHERE workspace_id = ? AND source IN ('cli', 'mcp') AND deleted_at IS NULL
	`
	args := []interface{}{workspaceID}

	if len(collectionIDs) > 0 && len(itemIDs) > 0 {
		collPlaceholders := make([]string, len(collectionIDs))
		for i, id := range collectionIDs {
			collPlaceholders[i] = "?"
			args = append(args, id)
		}
		itemPlaceholders := make([]string, len(itemIDs))
		for i, id := range itemIDs {
			itemPlaceholders[i] = "?"
			args = append(args, id)
		}
		query += " AND (collection_id IN (" + strings.Join(collPlaceholders, ",") + ") OR id IN (" + strings.Join(itemPlaceholders, ",") + "))"
	} else if len(collectionIDs) > 0 {
		placeholders := make([]string, len(collectionIDs))
		for i, id := range collectionIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND collection_id IN (" + strings.Join(placeholders, ",") + ")"
	} else if len(itemIDs) > 0 {
		placeholders := make([]string, len(itemIDs))
		for i, id := range itemIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND id IN (" + strings.Join(placeholders, ",") + ")"
	}

	query += ")"

	var has bool
	if err := s.db.QueryRow(s.q(query), args...).Scan(&has); err != nil {
		return false, fmt.Errorf("workspace has agent activity: %w", err)
	}
	return has, nil
}

// WorkspaceHasUserCreatedItems reports whether ANY non-deleted item
// in the workspace was created by something other than template
// seeding. Used by the agent bootstrap to compute the
// `needs_onboarding` flag — true when zero user-created items exist,
// false the moment the user (or an agent on their behalf) creates
// the first real item. PLAN-1496 / TASK-1504.
//
// "User-created" is the inverse of "template seed": items written
// during workspace init via SeedCollectionsFromTemplate carry
// source="template" + created_by="system". Everything else — CLI,
// web UI, MCP, future surfaces — is treated as user activity. The
// query filters on `source != 'template'` rather than enumerating
// the user-side values so new surfaces (mcp, api, etc.) are
// included automatically without code changes here.
//
// Visibility filtering is intentionally omitted: needs_onboarding
// is a workspace-level state signal, not a per-user view. Two
// callers reading bootstrap concurrently should see the same answer
// regardless of their individual collection-access grants — the
// flag describes whether the WORKSPACE has been activated, not
// whether the calling user has done so. Server already gates the
// bootstrap endpoint on workspace membership, so unauthorized
// callers never reach this code path.
//
// Backed by EXISTS so it short-circuits on the first match.
func (s *Store) WorkspaceHasUserCreatedItems(workspaceID string) (bool, error) {
	const query = `
		SELECT EXISTS(
			SELECT 1 FROM items
			WHERE workspace_id = ?
			  AND deleted_at IS NULL
			  AND (source IS NULL OR source != 'template')
		)
	`
	var has bool
	if err := s.db.QueryRow(s.q(query), workspaceID).Scan(&has); err != nil {
		return false, fmt.Errorf("workspace has user-created items: %w", err)
	}
	return has, nil
}

func hydrateItemComputedMetadata(item *models.Item) {
	if item == nil {
		return
	}
	item.ComputeRef()
	item.CodeContext = models.ExtractItemCodeContext(item.Fields)
	item.Convention = models.ExtractItemConventionMetadata(item.Fields)
	item.ImplementationNotes = models.ExtractItemImplementationNotes(item.Fields)
	item.DecisionLog = models.ExtractItemDecisionLog(item.Fields)
}
