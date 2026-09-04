package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Referent validation for `relation` field values (PLAN-2857 U1 / TASK-2878).
//
// WHY THIS IS IN `store` AND NOT IN `items` OR `server`.
//
// `internal/items` is DB-free by construction — `validate.go` keeps the SHAPE
// check for a relation ("must be a string") and cannot do more, because
// deciding whether a string names a live item in a particular collection is a
// database question.
//
// `internal/server` cannot own it either, even though six of the eight doors
// live there: the eighth is `store.migrateFieldsForCopy`, and `store` does not
// import `server` (nor could it). Putting the resolver here is what lets the
// cross-workspace copy door and the preflight door reach the SAME function
// rather than two implementations of one rule — the drift those two already
// carry a warning about, since they sit in different packages
// (`items_cross_workspace_copy.go`'s note above its `CoerceFields` call).
//
// VISIBILITY IS DELIBERATELY NOT HERE. "Can this requester see that item" is a
// request-scoped question needing the user, their role and the auth mode; the
// server layer adds it on top via `checkItemVisible`. A resolver that tried to
// answer it would need a second copy of that rule set, which is the same
// mistake in a different place.

// RelationIssueReason says WHY a relation value did not resolve. The wire
// vocabulary is deliberately small: callers render it, and a reason nobody can
// act on differently is not worth a distinct value.
type RelationIssueReason string

const (
	// RelationTargetNotFound — nothing in this workspace answers to the value.
	RelationTargetNotFound RelationIssueReason = "not_found"
	// RelationTargetWrongCollection — the value names a live item, but not one
	// in the collection the field declares. This is the case a workspace-wide
	// lookup silently accepts, and it is the defect the design pass recorded
	// against `ResolveItem` (R11).
	RelationTargetWrongCollection RelationIssueReason = "wrong_collection"
	// RelationTargetMissing — the field declares no target collection at all,
	// so nothing can be checked against it. A schema problem, surfaced rather
	// than treated as permission to store anything.
	RelationTargetMissing RelationIssueReason = "target_missing"
)

// RelationIssue is one unresolvable relation value, carrying everything a
// caller needs to render a message without re-deriving anything: which field,
// what the caller supplied, and which collection it was checked against.
type RelationIssue struct {
	Key    string
	Value  string
	Target string
	Reason RelationIssueReason
}

// Message renders the issue the way every door reports it. One function so the
// six refusing doors cannot phrase the same refusal three ways.
func (ri RelationIssue) Message() string {
	switch ri.Reason {
	case RelationTargetWrongCollection:
		return fmt.Sprintf("field %q: %q is not an item in collection %q", ri.Key, ri.Value, ri.Target)
	case RelationTargetMissing:
		return fmt.Sprintf("field %q declares no target collection, so %q cannot be resolved", ri.Key, ri.Value)
	default:
		return fmt.Sprintf("field %q: %q does not name an item in collection %q", ri.Key, ri.Value, ri.Target)
	}
}

// ResolveRelationReferents canonicalises every `relation` value in fieldMap to
// the target item's ID and reports the ones that cannot be resolved.
//
// It MUTATES fieldMap: a value supplied as an issue ref ("COLO-3") is replaced
// by the item's UUID, so callers downstream store one canonical form. Values
// that fail to resolve are left EXACTLY as supplied — the caller decides
// whether that is a refusal or a drop, and either way the original string is
// what the message has to quote.
//
// Scope, and what is deliberately absent:
//   - Same workspace only. A value naming an item in another workspace is
//     `not_found` here, which is the honest answer: cross-workspace relation
//     targets are out of PLAN-2857 v1.
//   - Empty and absent values are not issues. Clearing a relation is a
//     legitimate write, and a required-field check is `ValidateFields`'s job.
//   - Exact-title resolution is U6. A ref or a UUID resolve; nothing else does.
//   - Soft-deleted targets do not resolve (`ResolveItem` and `GetItem` exclude
//     them), so writing a reference to a deleted item is refused while an
//     ALREADY-STORED one still renders honestly on read — the read half U2
//     shipped.
func (s *Store) ResolveRelationReferents(
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
) ([]RelationIssue, error) {
	return s.ResolveRelationReferentsQ(s.Q(), workspaceID, schema, fieldMap)
}

// ResolveRelationReferentsQ is ResolveRelationReferents parameterised over its
// read executor, following the store's own `...Q` convention (`GetItemQ`,
// `uniqueSlugQ`, `getCollectionInWorkspaceTx`).
//
// This is not a convenience. `migrateCopyFields` runs inside
// `copyItemAcrossWorkspacesTx`, which opens a transaction as its second
// statement — so a resolver that read from the POOL there would issue pool
// reads while holding a tx, the deadlock this repo keeps a deterministic test
// for. Threading the Queryer is what lets ONE function serve the copy doors
// and the write doors instead of three-and-a-half.
func (s *Store) ResolveRelationReferentsQ(
	q Queryer,
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
) ([]RelationIssue, error) {
	var issues []RelationIssue
	// Cache per target slug: a schema with several relations aimed at one
	// collection should cost one lookup, not one per field.
	targets := map[string]string{}

	// Schema order, not map order, so repeated calls on equal input produce
	// identical output — the same determinism `ValidateFieldsDetailed`
	// documents for the copy preflight, which is one of this function's
	// callers and is specified to be safe to call repeatedly.
	for _, def := range schema.Fields {
		if def.Type != "relation" {
			continue
		}
		raw, exists := fieldMap[def.Key]
		if !exists || raw == nil {
			continue
		}
		value, isStr := raw.(string)
		if !isStr {
			// Shape is `ValidateFields`'s to reject, and it runs at every one
			// of these doors. Skipping here rather than reporting keeps one
			// error for one defect instead of two describing it differently.
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		if def.Collection == "" {
			issues = append(issues, RelationIssue{Key: def.Key, Value: value, Reason: RelationTargetMissing})
			continue
		}

		targetID, cached := targets[def.Collection]
		if !cached {
			var err error
			targetID, err = s.collectionIDBySlugQ(q, workspaceID, def.Collection)
			if err != nil {
				return nil, err
			}
			targets[def.Collection] = targetID
		}
		if targetID == "" {
			// The declared target names no collection in this workspace.
			// BUG-2873 propagates renames into relation FieldDefs, so this is
			// a genuinely broken schema rather than the ordinary rename case.
			issues = append(issues, RelationIssue{
				Key: def.Key, Value: value, Target: def.Collection, Reason: RelationTargetMissing,
			})
			continue
		}

		item, err := s.resolveRelationTargetQ(q, workspaceID, value)
		if err != nil {
			return nil, err
		}
		if item == nil {
			issues = append(issues, RelationIssue{
				Key: def.Key, Value: value, Target: def.Collection, Reason: RelationTargetNotFound,
			})
			continue
		}
		if item.CollectionID != targetID {
			issues = append(issues, RelationIssue{
				Key: def.Key, Value: value, Target: def.Collection, Reason: RelationTargetWrongCollection,
			})
			continue
		}
		// Canonicalise. A ref that resolved is stored as the ID.
		fieldMap[def.Key] = item.ID
	}
	return issues, nil
}

// resolveRelationTarget looks a value up by UUID or by issue ref/slug, scoped
// to the workspace. Returns (nil, nil) when nothing answers to it.
//
// The workspace assert on the UUID branch is load-bearing and is the reason
// this is not a bare `GetItem`: that lookup is GLOBAL, so without the check a
// UUID from another workspace would resolve and be stored — the shape
// `extractParentLink` guards with its own `resolvedParent.WorkspaceID !=
// workspaceID` test.
//
// NO SLUG FALLBACK, and this is a deliberate divergence from `ResolveItem`,
// which tries UUID, then ref, then slug. A relation field's contract is that
// it stores an item ID, and a slug is neither an ID nor a stable identifier:
// "red" would resolve to whatever item happens to be slugged `red` TODAY, and
// slugs are mutable, so the same stored value could point somewhere else
// tomorrow. Accepting it here would also make the corruption this unit exists
// to stop indistinguishable from a legitimate write — "red" is exactly the
// free-text value the pre-U2 editor was writing into these fields, and the
// client already refuses to render such a match for the same reason
// (`FieldEditor`'s relationRow narrowing, TASK-2868). Exact-TITLE resolution
// is U6 and is a different question with its own collection-scoping rule.
//
// Both lookups exclude soft-deleted rows — `ResolveItem` by contrast with
// `ResolveItemIncludeDeleted`, and `GetItem` via `getItemScanQ`, which appends
// `AND i.deleted_at IS NULL` unless asked otherwise. Checked at those lines
// rather than assumed, because the whole read half of U2 depends on a deleted
// target being distinguishable from a value that never resolved.
func (s *Store) resolveRelationTargetQ(q Queryer, workspaceID, value string) (*models.Item, error) {
	if isUUID(value) {
		item, err := s.GetItemQ(q, value)
		if err != nil {
			return nil, err
		}
		if item == nil || item.WorkspaceID != workspaceID {
			return nil, nil
		}
		return item, nil
	}
	prefix, number, ok := parseItemRef(value)
	if !ok {
		// Not a UUID and not a ref, so nothing here resolves it. In
		// particular NOT a slug — see the note above.
		return nil, nil
	}
	item, err := s.itemByRefQ(q, workspaceID, prefix, number)
	if err != nil {
		return nil, err
	}
	return item, nil
}

// collectionIDBySlugQ returns the collection's ID, or "" when the workspace
// has no live collection with that slug. ID only: the referent check compares
// `item.CollectionID`, and loading the whole model would pull in per-collection
// counts this has no use for.
func (s *Store) collectionIDBySlugQ(q Queryer, workspaceID, slug string) (string, error) {
	var id string
	err := q.QueryRow(s.q(`
		SELECT id FROM collections
		WHERE workspace_id = ? AND slug = ? AND deleted_at IS NULL
	`), workspaceID, slug).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("collection id by slug: %w", err)
	}
	return id, nil
}

// itemByRefQ is GetItemByRef on a caller-supplied executor. It keeps that
// helper's FALLBACK — item numbers are workspace-unique, so a ref whose prefix
// no longer matches still resolves by number — because a relation written as
// COLO-3 must keep resolving after the target's collection is renamed, which
// is precisely what BUG-2873 made possible.
func (s *Store) itemByRefQ(q Queryer, workspaceID, prefix string, number int) (*models.Item, error) {
	var id string
	err := q.QueryRow(s.q(`
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ? AND i.deleted_at IS NULL
	`), workspaceID, prefix, number).Scan(&id)
	if err == nil {
		return s.GetItemQ(q, id)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("item by ref: %w", err)
	}
	err = q.QueryRow(s.q(`
		SELECT id FROM items
		WHERE workspace_id = ? AND item_number = ? AND deleted_at IS NULL
	`), workspaceID, number).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("item by number: %w", err)
	}
	return s.GetItemQ(q, id)
}

// RelationTargetNotPortable — a carried relation value that cannot cross a
// workspace boundary. Not a defect in the value: it names a live item in the
// SOURCE workspace, and PLAN-2857 v1 excludes cross-workspace relation
// targets, so there is nothing in the destination it could mean. Reported with
// the same reason `github_pr` uses, because it is the same fact about the same
// kind of value.
const RelationTargetNotPortable RelationIssueReason = "referent_not_portable"

// RelationCarryMode says what a migrate door should do with relation values
// CARRIED from a source item.
type RelationCarryMode int

const (
	// RelationCarryWithinWorkspace — a same-workspace move or bulk move. The
	// targets are still in this workspace, so a valid relation SURVIVES; only
	// a value that does not resolve is dropped.
	RelationCarryWithinWorkspace RelationCarryMode = iota
	// RelationCarryCrossWorkspace — a cross-workspace copy, or its preflight.
	// Every carried relation is dropped by construction, without a lookup:
	// the value names a source-workspace row, and no amount of resolving in
	// the destination changes that.
	RelationCarryCrossWorkspace
)

// MigrateRelationReferents is the ONE decision the four migrate doors share
// (PLAN-2857 U1 / TASK-2878). Same-workspace move, bulk move, cross-workspace
// copy and the copy preflight all call this; none of them reimplements it.
//
// That is not tidiness. The preflight lives in `internal/server` and the copy
// lives in `internal/store`, and the code already carries a warning that those
// two sit in different packages and that is how they drift unnoticed. A
// preflight that says "carried" while the copy drops — or the reverse — is one
// request answered two ways, which is the exact defect that comment describes.
//
// PROVENANCE, not door, decides the outcome:
//
//   - SUPPLIED (present in `supplied`, i.e. an explicit `--field` override on
//     the move or copy): the caller asserted this value, so it is a write and
//     an unresolvable one is REFUSED. Returned in `refusals`.
//   - CARRIED (everything else): not asserted by anyone, so refusing it would
//     make a legacy item — and `internal/items` has accepted any string for a
//     relation all along, so most of them are legacy — unmovable and
//     uncopyable. Dropped and REPORTED instead, through the same
//     `dropped_fields` channel BUG-2674 established for a move and the same
//     `referent_not_portable` bucket the copy already uses for `github_pr`.
//
// Mutates fieldMap: dropped keys are deleted, and carried values that survive
// are canonicalised to their target's ID.
func (s *Store) MigrateRelationReferents(
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	supplied map[string]any,
	mode RelationCarryMode,
) (refusals []RelationIssue, dropped []RelationIssue, err error) {
	return s.MigrateRelationReferentsQ(s.Q(), workspaceID, schema, fieldMap, supplied, mode)
}

// MigrateRelationReferentsQ is MigrateRelationReferents on a caller-supplied
// read executor — see ResolveRelationReferentsQ for why the copy door needs it.
func (s *Store) MigrateRelationReferentsQ(
	q Queryer,
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	supplied map[string]any,
	mode RelationCarryMode,
) (refusals []RelationIssue, dropped []RelationIssue, err error) {
	// Split the map by provenance FIRST, so the two halves cannot be confused
	// by anything below. Schema order, for the determinism the preflight
	// promises its callers.
	carried := map[string]any{}
	suppliedRelations := map[string]any{}
	for _, def := range schema.Fields {
		if def.Type != "relation" {
			continue
		}
		raw, exists := fieldMap[def.Key]
		if !exists || raw == nil {
			continue
		}
		if _, isSupplied := supplied[def.Key]; isSupplied {
			suppliedRelations[def.Key] = raw
			continue
		}
		carried[def.Key] = raw
	}

	// Supplied values are ordinary writes.
	if len(suppliedRelations) > 0 {
		issues, resolveErr := s.ResolveRelationReferentsQ(q, workspaceID, schema, suppliedRelations)
		if resolveErr != nil {
			return nil, nil, resolveErr
		}
		refusals = issues
		// Carry the canonicalised survivors back.
		for k, v := range suppliedRelations {
			fieldMap[k] = v
		}
	}

	switch mode {
	case RelationCarryCrossWorkspace:
		// No lookup: a source-workspace id cannot mean anything here.
		for _, def := range schema.Fields {
			if def.Type != "relation" {
				continue
			}
			raw, exists := carried[def.Key]
			if !exists {
				continue
			}
			value, _ := raw.(string)
			dropped = append(dropped, RelationIssue{
				Key: def.Key, Value: value, Target: def.Collection, Reason: RelationTargetNotPortable,
			})
			delete(fieldMap, def.Key)
		}
	default:
		// Same workspace: resolve, keep what resolves, drop what does not.
		issues, resolveErr := s.ResolveRelationReferentsQ(q, workspaceID, schema, carried)
		if resolveErr != nil {
			return nil, nil, resolveErr
		}
		for _, ri := range issues {
			dropped = append(dropped, ri)
			delete(fieldMap, ri.Key)
		}
		// Write the canonicalised survivors back. A dropped key was deleted
		// from fieldMap just above, so its absence there is what marks it —
		// checking that rather than re-scanning `issues` keeps the two loops
		// from disagreeing about which keys survived.
		for k, v := range carried {
			if _, survived := fieldMap[k]; !survived {
				continue
			}
			fieldMap[k] = v
		}
	}
	return refusals, dropped, nil
}
