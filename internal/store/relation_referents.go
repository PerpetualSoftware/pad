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
	// RelationTargetInvalidShape — the value is not a string at all, so it
	// cannot name anything. Normally `ValidateFields` catches this and the
	// resolver deliberately stays out of it (one error per defect), but an
	// injected schema DEFAULT is never type-checked: ValidateFields assigns
	// it and `continue`s past its own validation. That is the one route by
	// which a non-string reaches a relation field unchallenged (codex round
	// 6), so the late-default pass reports it rather than skipping it.
	RelationTargetInvalidShape RelationIssueReason = "invalid_shape"
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
	case RelationTargetInvalidShape:
		return fmt.Sprintf("field %q has a default that is not a reference", ri.Key)
	default:
		return fmt.Sprintf("field %q: %q does not name an item in collection %q", ri.Key, ri.Value, ri.Target)
	}
}

// RelationIssueReasons is every value a RelationIssue's Reason can carry.
//
// It exists so consumers can ENUMERATE the vocabulary rather than grep for it.
// The copy preflight puts these strings on the wire, and the web dialog
// switches on them; a reason added here without a case there renders as the raw
// enum to a user, which is exactly how `referent_not_portable` shipped in
// BUG-2674 and sat unhandled until TASK-2878 made it routine. A function rather
// than a var so no caller can append to the package's own list.
func RelationIssueReasons() []RelationIssueReason {
	return []RelationIssueReason{
		RelationTargetNotFound,
		RelationTargetWrongCollection,
		RelationTargetMissing,
		RelationTargetNotPortable,
		RelationTargetInvalidShape,
	}
}

// RelationIssuesMessage renders a set of issues as the ONE sentence every
// refusing door reports. It lives here rather than in `internal/server`
// because the store's own copy door refuses too (through
// `FieldValidationError`), and a second join in a second package is how the
// same refusal comes to be phrased two ways — the drift this whole unit
// exists to remove. `internal/server`'s `relationIssuesMessage` delegates to
// this.
func RelationIssuesMessage(issues []RelationIssue) string {
	parts := make([]string, 0, len(issues))
	for _, ri := range issues {
		parts = append(parts, ri.Message())
	}
	return strings.Join(parts, "; ")
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

// ResolveRelationTarget resolves ONE relation value to its item, or (nil, nil)
// when nothing in the workspace answers to it.
//
// Exported for the server's visibility layer. `wrong_collection` names a LIVE
// item, so the message distinguishes "exists, elsewhere" from "does not
// exist" — an existence oracle unless the server can first check whether the
// requester may see that item, which needs the item. Same UUID-or-ref rule as
// everything else here: no slug fallback.
func (s *Store) ResolveRelationTarget(workspaceID, value string) (*models.Item, error) {
	return s.resolveRelationTargetQ(s.Q(), workspaceID, value)
}

// RequiredRelationIssues returns the subset of issues whose field the schema
// declares REQUIRED.
//
// A dropped value in a required relation field cannot be left as a drop: the
// key is deleted after validation has already passed, so nothing re-checks it
// and the item lands with a required field absent. Callers turn these into
// their own door's missing-required refusal.
func RequiredRelationIssues(schema models.CollectionSchema, issues []RelationIssue) []RelationIssue {
	required := map[string]bool{}
	for _, def := range schema.Fields {
		if def.Type == "relation" && def.Required {
			required[def.Key] = true
		}
	}
	var out []RelationIssue
	for _, ri := range issues {
		if required[ri.Key] {
			out = append(out, ri)
		}
	}
	return out
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

// RelationOrigin says where a relation value in a migrate door's field map
// came from. THREE origins, not two, and the third is the one that is easy to
// miss: `items.MigrateFields` injects the DESTINATION schema's defaults into
// the map it returns, so a value can be present having been chosen by the
// destination rather than carried from the source.
type RelationOrigin int

const (
	// RelationOriginSupplied — an explicit override on the move or copy. The
	// caller asserted it, so it is a write.
	RelationOriginSupplied RelationOrigin = iota
	// RelationOriginCarried — present on the SOURCE item. Asserted by nobody.
	RelationOriginCarried
	// RelationOriginDestinationDefault — absent from the source item and not
	// supplied, so `MigrateFields` filled it from the destination schema's
	// `default`. It never pointed at the source workspace, which is why it
	// must not be dropped as `referent_not_portable`.
	RelationOriginDestinationDefault
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
// ORIGIN, not door, decides the outcome:
//
//   - SUPPLIED (present in `supplied`, i.e. an explicit `--field` override on
//     the move or copy): the caller asserted this value, so it is a write and
//     an unresolvable one is REFUSED. Returned in `refusals`.
//   - CARRIED (present in `sourceFields`): not asserted by anyone, so refusing
//     it would make a legacy item — and `internal/items` has accepted any
//     string for a relation all along, so most of them are legacy — unmovable
//     and uncopyable. Dropped and REPORTED instead, through the same
//     `dropped_fields` channel BUG-2674 established and the same
//     `referent_not_portable` bucket the copy already uses for `github_pr`.
//   - DESTINATION DEFAULT (neither): `MigrateFields` filled it in from the
//     destination schema. Resolved against the destination REGARDLESS of mode,
//     because a value the destination chose is not a source-workspace referent
//     and dropping it as "not portable" would be a false statement about where
//     it came from. A default that does not resolve is a schema problem, so it
//     is dropped and reported with the resolver's own reason rather than
//     refused — nobody in this request typed it.
//
// EMPTY VALUES ARE NOT REFERENTS and are skipped at every origin. Reporting a
// blank field as dropped tells a user they lost something they never had.
//
// Mutates fieldMap: dropped keys are deleted, and values that survive are
// canonicalised to their target's ID.
func (s *Store) MigrateRelationReferents(
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	supplied map[string]any,
	sourceFields map[string]any,
	mode RelationCarryMode,
) (refusals []RelationIssue, dropped []RelationIssue, err error) {
	return s.MigrateRelationReferentsQ(s.Q(), workspaceID, schema, fieldMap, supplied, sourceFields, mode)
}

// MigrateRelationReferentsQ is MigrateRelationReferents on a caller-supplied
// read executor — see ResolveRelationReferentsQ for why the copy door needs it.
func (s *Store) MigrateRelationReferentsQ(
	q Queryer,
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	supplied map[string]any,
	sourceFields map[string]any,
	mode RelationCarryMode,
) (refusals []RelationIssue, dropped []RelationIssue, err error) {
	// Split the map by ORIGIN first, so nothing below can confuse the three.
	// Schema order, for the determinism the preflight promises its callers.
	byOrigin := map[RelationOrigin]map[string]any{
		RelationOriginSupplied:           {},
		RelationOriginCarried:            {},
		RelationOriginDestinationDefault: {},
	}
	for _, def := range schema.Fields {
		if def.Type != "relation" {
			continue
		}
		raw, exists := fieldMap[def.Key]
		if !exists || raw == nil {
			continue
		}
		// An empty value is a cleared relation, not a referent. Skipping it
		// here keeps it out of every bucket, so it is neither refused nor
		// reported as a drop the user cannot act on.
		if str, isStr := raw.(string); isStr && strings.TrimSpace(str) == "" {
			continue
		}
		switch {
		case hasKey(supplied, def.Key):
			byOrigin[RelationOriginSupplied][def.Key] = raw
		case hasKey(sourceFields, def.Key):
			byOrigin[RelationOriginCarried][def.Key] = raw
		default:
			byOrigin[RelationOriginDestinationDefault][def.Key] = raw
		}
	}

	// Supplied values are ordinary writes.
	if suppliedRelations := byOrigin[RelationOriginSupplied]; len(suppliedRelations) > 0 {
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

	// Destination defaults resolve against the DESTINATION in both modes — the
	// destination picked the value, so the mode says nothing about it.
	if defaults := byOrigin[RelationOriginDestinationDefault]; len(defaults) > 0 {
		issues, resolveErr := s.ResolveRelationReferentsQ(q, workspaceID, schema, defaults)
		if resolveErr != nil {
			return nil, nil, resolveErr
		}
		for _, ri := range issues {
			dropped = append(dropped, ri)
			delete(fieldMap, ri.Key)
		}
		for k, v := range defaults {
			if _, survived := fieldMap[k]; !survived {
				continue
			}
			fieldMap[k] = v
		}
	}

	carried := byOrigin[RelationOriginCarried]
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
			value, isStr := raw.(string)
			if !isStr {
				// Not a reference at all, so "cannot cross a workspace
				// boundary" is a false account of why it is going. Left in
				// place for ValidateFields to reject on shape — which is what
				// the SAME-workspace branch already does with it, and the two
				// modes disagreeing about one malformed value was the defect
				// (codex round 6).
				continue
			}
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

// RelationKeysPresent snapshots which relation keys a field map holds, for
// callers that must tell "this value was here before validation" from "this
// value appeared because validation injected a schema default".
func RelationKeysPresent(schema models.CollectionSchema, fieldMap map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, def := range schema.Fields {
		if def.Type != "relation" {
			continue
		}
		if v, exists := fieldMap[def.Key]; exists && v != nil {
			out[def.Key] = true
		}
	}
	return out
}

// ResolveLateRelationDefaults resolves relation values that appeared in
// fieldMap only AFTER validation ran — i.e. schema defaults `ValidateFields`
// injected for keys that were missing or nil.
//
// WHY A SECOND PASS EXISTS AT ALL. The migrate doors validate after they
// resolve, because the required-field check has to see a value that referent
// resolution dropped: without that order, dropping an unresolvable value in a
// REQUIRED relation field would store the item with the field absent instead
// of refusing. But `ValidateFields` injects defaults, so a default lands after
// the resolver has finished and reaches the row uncanonicalised — and an
// invalid one that the resolver deleted is put straight back, with
// `StillDropped` then suppressing the warning about it (codex round 2).
//
// Reordering the two would trade this defect for the required-field one. A
// narrow pass over exactly the keys validation added costs one lookup in the
// rare case a relation field declares a default, and nothing otherwise.
//
// A default is asserted by nobody, so an unresolvable one is DROPPED and
// reported, never refused — the same disposition
// MigrateRelationReferents gives RelationOriginDestinationDefault, which is
// what this is: the same origin, arriving late.
func (s *Store) ResolveLateRelationDefaults(
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	before map[string]bool,
) (dropped []RelationIssue, err error) {
	return s.ResolveLateRelationDefaultsQ(s.Q(), workspaceID, schema, fieldMap, before)
}

// ResolveLateRelationDefaultsQ is ResolveLateRelationDefaults on a
// caller-supplied read executor — the copy door runs inside a transaction.
func (s *Store) ResolveLateRelationDefaultsQ(
	q Queryer,
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	before map[string]bool,
) (dropped []RelationIssue, err error) {
	late := map[string]any{}
	for _, def := range schema.Fields {
		if def.Type != "relation" || before[def.Key] {
			continue
		}
		raw, exists := fieldMap[def.Key]
		if !exists || raw == nil {
			continue
		}
		str, isStr := raw.(string)
		if !isStr {
			// A default validation injected without type-checking it. The
			// resolver cannot use it and nothing else will complain, so it is
			// dropped and reported here.
			dropped = append(dropped, RelationIssue{
				Key: def.Key, Target: def.Collection, Reason: RelationTargetInvalidShape,
			})
			delete(fieldMap, def.Key)
			continue
		}
		if strings.TrimSpace(str) == "" {
			continue
		}
		late[def.Key] = raw
	}
	if len(late) == 0 {
		// `dropped` may already carry non-string defaults rejected above, so
		// this returns it rather than nil — an early `return nil, nil` here
		// silently discarded them.
		return dropped, nil
	}
	issues, resolveErr := s.ResolveRelationReferentsQ(q, workspaceID, schema, late)
	if resolveErr != nil {
		return nil, resolveErr
	}
	for _, ri := range issues {
		dropped = append(dropped, ri)
		delete(fieldMap, ri.Key)
	}
	for k, v := range late {
		if _, survived := fieldMap[k]; !survived {
			continue
		}
		fieldMap[k] = v
	}
	return dropped, nil
}

// IssuesForCallerInput drops the issues whose key was NOT present before
// validation ran — i.e. the ones raised against a schema DEFAULT that
// `ValidateFields` injected.
//
// The write doors resolve the whole field map, which is caller input plus
// whatever validation just filled in, and refuse anything that does not
// resolve. That is right for the caller's own values and wrong for a default:
// by this unit's rule a default is asserted by nobody, so an unresolvable one
// is dropped and reported, never refused — otherwise a single bad default in a
// collection's schema makes every write into that collection fail, on a defect
// its author has to fix somewhere else entirely (codex round 10). The migrate
// doors already behaved this way; the write doors did not.
func IssuesForCallerInput(issues []RelationIssue, presentBefore map[string]bool) []RelationIssue {
	out := make([]RelationIssue, 0, len(issues))
	for _, ri := range issues {
		if presentBefore[ri.Key] {
			out = append(out, ri)
		}
	}
	return out
}

// CarriedSourceValues narrows a source item's field map to the values that
// actually SURVIVED migration.
//
// The migrate doors need "which values carried", and the source map alone
// answers a different question — "which keys the source had" (codex round 9).
// When MigrateFields cannot convert a source value it DROPS the key and its
// default loop then fills the same key from the DESTINATION schema, so the
// value sitting in the map came from the destination while the key is still
// present in the source. Classified as carried, a cross-workspace copy then
// discards the destination's own default as non-portable — the round-1 defect
// again, one level finer: presence of the KEY is not survival of the VALUE.
//
// `dropped` is MigrateFields' own Dropped list, which is exactly the set of
// keys whose source value did not make it.
func CarriedSourceValues(sourceFields map[string]any, dropped []string) map[string]any {
	if len(dropped) == 0 {
		return sourceFields
	}
	lost := make(map[string]bool, len(dropped))
	for _, k := range dropped {
		lost[k] = true
	}
	out := make(map[string]any, len(sourceFields))
	for k, v := range sourceFields {
		if lost[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// hasKey reports whether m declares key. A nil map has no keys, which is how a
// door with no overrides (bulk move) or no source item says so.
func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}
