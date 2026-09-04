package store

import (
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
	var issues []RelationIssue
	// Cache per target slug: a schema with several relations aimed at one
	// collection should cost one lookup, not one per field.
	targets := map[string]*models.Collection{}

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

		target, cached := targets[def.Collection]
		if !cached {
			var err error
			target, err = s.GetCollectionBySlug(workspaceID, def.Collection)
			if err != nil {
				return nil, err
			}
			targets[def.Collection] = target
		}
		if target == nil {
			// The declared target names no collection in this workspace.
			// BUG-2873 propagates renames into relation FieldDefs, so this is
			// a genuinely broken schema rather than the ordinary rename case.
			issues = append(issues, RelationIssue{
				Key: def.Key, Value: value, Target: def.Collection, Reason: RelationTargetMissing,
			})
			continue
		}

		item, err := s.resolveRelationTarget(workspaceID, value)
		if err != nil {
			return nil, err
		}
		if item == nil {
			issues = append(issues, RelationIssue{
				Key: def.Key, Value: value, Target: def.Collection, Reason: RelationTargetNotFound,
			})
			continue
		}
		if item.CollectionID != target.ID {
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
func (s *Store) resolveRelationTarget(workspaceID, value string) (*models.Item, error) {
	if isUUID(value) {
		item, err := s.GetItem(value)
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
	item, err := s.GetItemByRef(workspaceID, prefix, number)
	if err != nil {
		return nil, err
	}
	return item, nil
}
