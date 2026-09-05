package server

import (
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Server-side half of referent validation for `relation` values (PLAN-2857 U1
// / TASK-2878). The store owns the question "does this value name a live item
// in the declared target collection"; this file adds the one part that is
// request-scoped — whether the caller may SEE that item — and turns the result
// into whatever each door owes its caller.
//
// Eight doors reach `items.CoerceFields`, and they do NOT all owe the same
// thing, which is why this is a value-returning helper rather than a
// write-the-response one in the `extractParentLink` mould:
//
//   - The six WRITE doors (create, update-fields, update-fields_patch,
//     same-workspace move, bulk update, bulk move) refuse: a caller who
//     supplied a value asserted it, and an unresolvable assertion is a 400.
//   - The two COPY doors report instead. A relation value CARRIED from a
//     source item points at a source-workspace row and cannot resolve in the
//     destination by construction; that is an unportable referent, not a bad
//     write, and the copy already drops such things (`github_pr`, reason
//     `referent_not_portable`). Refusing would make every copy of a related
//     item start failing and would reintroduce the cross-workspace relation
//     case PLAN-2857 excludes from v1. An override SUPPLIED to a copy is a
//     write like any other and still refuses.

// resolveRelationReferents runs the store resolver and then drops any target
// the requester cannot see, reporting it as `not_found`.
//
// Visibility is folded into the SAME issue vocabulary rather than given its
// own reason on purpose: telling a caller "that item exists but you may not
// see it" is an existence oracle, which this codebase has a standing rule
// against. The store's `not_found` and a visibility failure must be
// indistinguishable on the wire.
func (s *Server) resolveRelationReferents(
	r *http.Request,
	workspaceID string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
) ([]store.RelationIssue, error) {
	return s.resolveRelationReferentsAs(r, workspaceID, workspaceRole(r), schema, fieldMap)
}

// resolveRelationReferentsAs is resolveRelationReferents with the requester's
// effective role passed EXPLICITLY rather than read from the request.
//
// The cross-workspace copy and its preflight need this. `workspaceRole(r)` is
// the role the middleware stashed for the workspace in the URL — the SOURCE —
// and a relation override on a copy names an item in the DESTINATION, where
// the caller's role can be different or absent. `CrossWorkspaceAccess.Role`
// is that role, derived fresh from membership and grants, and its own doc says
// in as many words never to substitute `workspaceRole(r)` for it.
func (s *Server) resolveRelationReferentsAs(
	r *http.Request,
	workspaceID string,
	role string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
) ([]store.RelationIssue, error) {
	// The ORIGINAL values, captured before the store resolver rewrites a ref
	// into its target's UUID. Every issue this function raises quotes what the
	// CALLER sent, never the canonical form: a refusal for an item the
	// requester may not see must not hand back that item's UUID, which would
	// confirm both its existence and its canonical identity — the existence
	// oracle the `not_found` collapse exists to prevent, reopened by the
	// message (codex round 2).
	supplied := make(map[string]string, len(fieldMap))
	for k, v := range fieldMap {
		if str, isStr := v.(string); isStr {
			supplied[k] = str
		}
	}
	quoted := func(key, canonical string) string {
		if orig, ok := supplied[key]; ok && orig != "" {
			return orig
		}
		return canonical
	}

	issues, err := s.store.ResolveRelationReferents(workspaceID, schema, fieldMap)
	if err != nil {
		return nil, err
	}

	// Everything the store resolved is now a canonical ID in fieldMap. Check
	// each one against the requester before it is allowed to stand.
	for _, def := range schema.Fields {
		if def.Type != "relation" {
			continue
		}
		raw, exists := fieldMap[def.Key]
		if !exists || raw == nil {
			continue
		}
		id, isStr := raw.(string)
		// TRIMMED, matching the store resolver: it ignores a whitespace-only
		// value as "no reference", so an untrimmed check here refuses a value
		// the store never objected to — and since the vanished-target arm
		// below turns a missing lookup into a refusal, `"   "` became a
		// not_found instead of an empty field (codex round 6).
		if !isStr || strings.TrimSpace(id) == "" {
			continue
		}
		if ri, already := issueForKey(issues, def.Key); already {
			// `wrong_collection` is the one issue that names a LIVE item, so
			// its message ("is not an item in collection X") tells the caller
			// the value EXISTS — distinguishable from the `not_found` a
			// nonexistent value gets, and therefore an existence oracle for
			// anyone who cannot see that item (codex round 3). Collapse it to
			// `not_found` when the requester may not see the target; keep the
			// specific message when they may, because "you linked a task
			// where a person belongs" is the useful half of this reason.
			//
			// Any other issue is already `not_found`-shaped and needs nothing.
			if ri.Reason != store.RelationTargetWrongCollection {
				continue
			}
			target, terr := s.store.ResolveRelationTarget(workspaceID, ri.Value)
			if terr != nil {
				return nil, terr
			}
			if target == nil {
				// Deleted between the resolver's lookup and this one. The
				// issue still SAYS `wrong_collection`, and that message
				// reveals the value named something a moment ago — the same
				// disclosure for a caller who cannot see it (codex round 4).
				// My first comment here read "already the safe answer", which
				// was wrong: nothing had rewritten the reason.
				collapseIssue(issues, def.Key, store.RelationTargetNotFound)
				continue
			}
			seen, verr := s.checkItemVisible(workspaceID, target, currentUser(r), role, isBearerAuth(r))
			if verr != nil {
				return nil, verr
			}
			if !seen {
				collapseIssue(issues, def.Key, store.RelationTargetNotFound)
			}
			continue
		}
		item, err := s.store.GetItem(id)
		if err != nil {
			return nil, err
		}
		if item == nil {
			// It resolved moments ago and is gone now — soft-deleted between
			// the two reads. Treated as unresolvable rather than waved
			// through: this whole unit exists to stop a dangling referent
			// reaching the blob, and "the target vanished mid-request" is the
			// one case where letting it through would be a deliberate one.
			// Same `not_found` the resolver would have given a moment later,
			// so a retry reports it identically.
			issues = append(issues, store.RelationIssue{
				Key: def.Key, Value: quoted(def.Key, id), Target: def.Collection,
				Reason: store.RelationTargetNotFound,
			})
			continue
		}
		visible, err := s.checkItemVisible(workspaceID, item, currentUser(r), role, isBearerAuth(r))
		if err != nil {
			return nil, err
		}
		if !visible {
			issues = append(issues, store.RelationIssue{
				Key: def.Key, Value: quoted(def.Key, id), Target: def.Collection,
				Reason: store.RelationTargetNotFound,
			})
		}
	}
	return issues, nil
}

// issueForKey returns the issue already raised for key, if any.
func issueForKey(issues []store.RelationIssue, key string) (store.RelationIssue, bool) {
	for _, ri := range issues {
		if ri.Key == key {
			return ri, true
		}
	}
	return store.RelationIssue{}, false
}

// collapseIssue rewrites the reason of the issue already raised for key. In
// place, because the slice is what the caller renders.
func collapseIssue(issues []store.RelationIssue, key string, reason store.RelationIssueReason) {
	for i := range issues {
		if issues[i].Key == key {
			issues[i].Reason = reason
			return
		}
	}
}

// collapseInvisibleRelationIssues rewrites `wrong_collection` to `not_found`
// on any issue whose target the requester cannot see — the same collapse
// resolveRelationReferentsAs applies to the MAIN pass, hoisted so the LATE
// pass gets it too.
//
// `store.ResolveLateRelationDefaults` is a store function and cannot know who
// is asking, so every issue it returns carries the raw reason. Those issues
// reach a caller: each door feeds them to RequiredRelationIssues and renders
// the result into a 400 or a preflight `needs_value` row. `wrong_collection`
// is the one reason that names a LIVE item, so an invisible target announced
// that way is the existence oracle round 3 closed, reopened through the door
// round 10 added (codex round 15).
//
// Reviewer named ONE site; this is applied at all five late-default sites,
// per CONVE-18 — the class is "a store-resolved issue reaching a caller
// without passing the visibility collapse", not the one call it was spotted at.
func (s *Server) collapseInvisibleRelationIssues(r *http.Request, workspaceID, role string, issues []store.RelationIssue) error {
	for i := range issues {
		if issues[i].Reason != store.RelationTargetWrongCollection {
			continue
		}
		target, terr := s.store.ResolveRelationTarget(workspaceID, issues[i].Value)
		if terr != nil {
			return terr
		}
		if target == nil {
			// Vanished between the two reads. The reason still SAYS the value
			// named something a moment ago, which is the same disclosure.
			issues[i].Reason = store.RelationTargetNotFound
			continue
		}
		seen, verr := s.checkItemVisible(workspaceID, target, currentUser(r), role, isBearerAuth(r))
		if verr != nil {
			return verr
		}
		if !seen {
			issues[i].Reason = store.RelationTargetNotFound
		}
	}
	return nil
}

// refuseRelationIssues writes the 400 a write door owes and reports whether it
// did. One function so the six refusing doors cannot phrase the same refusal
// three different ways, and so a client matching on the error code sees one
// answer from every door.
//
// The message names the field, the value AS SUPPLIED, and the target
// collection — the three things a caller needs to fix it without a second
// request. Every issue is rendered, joined; a schema has a handful of relation
// fields at most, and reporting one at a time would make fixing a bulk import
// an N-round-trip exercise.
//
// Deliberately the ORDINARY `validation_error` shape, with no new details key.
// Two reasons: the MCP stdio transport classifies errors by matching CLI
// stderr PROSE, so a structured field it cannot see would help nobody there;
// and a new error shape is a contract change for every client, which is not
// what this unit is for.
func refuseRelationIssues(w http.ResponseWriter, issues []store.RelationIssue) bool {
	if len(issues) == 0 {
		return false
	}
	writeError(w, http.StatusBadRequest, "validation_error", relationIssuesMessage(issues))
	return true
}

// relationIssuesMessage is the same rendering for the doors that RETURN an
// error instead of writing a response — `createItemChecked` (*itemCreateError)
// and the two bulk operations (*bulkOpError). Split out rather than duplicated
// so a caller cannot accidentally produce a different sentence for the same
// refusal depending on which door it came through.
//
// DELEGATES to store.RelationIssuesMessage rather than joining here (TASK-2878).
// The eighth door refuses inside `internal/store` — the cross-workspace copy,
// through *FieldValidationError — so the sentence has to be reachable from
// there too. Two joins in two packages is how one refusal acquires two
// phrasings, which is the drift this unit exists to remove.
func relationIssuesMessage(issues []store.RelationIssue) string {
	return store.RelationIssuesMessage(issues)
}

// refuseInvisibleRelationOverrides is the visibility check the MIGRATE doors
// owe their SUPPLIED half.
//
// The four write doors go through resolveRelationReferents, which adds
// checkItemVisible on top of the store resolver. The migrate doors call
// store.MigrateRelationReferents directly — it is a store function and cannot
// answer a request-scoped question — so without this their supplied overrides
// resolved against the database alone. This unit's own rule says a supplied
// value is an ordinary write; an ordinary write cannot name an item the
// requester may not see, and a caller able to edit both collections could
// otherwise point a relation at a hidden one.
//
// Only relation keys are probed, on a COPY of the values: the store call that
// follows does the canonicalising write, and a helper that also mutated would
// leave two functions writing one map.
//
// `role` is explicit for the reason resolveRelationReferentsAs documents — at a
// cross-workspace copy the relevant role is the caller's in the DESTINATION.
func (s *Server) refuseInvisibleRelationOverrides(
	r *http.Request,
	workspaceID string,
	role string,
	schema models.CollectionSchema,
	supplied map[string]any,
) ([]store.RelationIssue, error) {
	if len(supplied) == 0 {
		return nil, nil
	}
	probe := make(map[string]any, len(supplied))
	for _, def := range schema.Fields {
		if def.Type != "relation" {
			continue
		}
		if v, ok := supplied[def.Key]; ok && v != nil {
			probe[def.Key] = v
		}
	}
	if len(probe) == 0 {
		return nil, nil
	}
	return s.resolveRelationReferentsAs(r, workspaceID, role, schema, probe)
}

// notDefaultKeys is the set a migrate door hands
// dropInvisibleRelationDefaults: the caller's own values plus the values
// carried from the source item. Everything else in the map came from the
// destination schema's defaults.
// A NIL value is not a value. `ValidateFields` treats a present-but-nil key as
// absent and injects the destination default in its place, so a key holding
// nil in `supplied` or `carried` names a value that CAME FROM THE DEFAULT —
// and counting it here excludes it from the very check it needs (codex round
// 14). Third time nil has done this in one unit: rounds 4 and 5 were the same
// distinction in the origin label.
func notDefaultKeys(supplied, carried map[string]any) map[string]bool {
	out := make(map[string]bool, len(supplied)+len(carried))
	for _, m := range []map[string]any{supplied, carried} {
		for k, v := range m {
			if v == nil {
				continue
			}
			out[k] = true
		}
	}
	return out
}

// dropInvisibleRelationDefaults is the visibility check a LATE-RESOLVED
// DEFAULT owes (codex round 11).
//
// `ResolveLateRelationDefaults` lives in `store` and resolves against the
// database alone. At the write doors the caller's own values are filtered out
// of the refusal set before it runs — correctly, since a default is not caller
// input — but that also removed the visibility issues raised against those
// keys, and the store then re-resolved them with no visibility layer at all.
// The write's RESPONSE carries the item's fields, so the caller received the
// canonical id of an item they cannot see.
//
// DROPPED, not refused, because the origin has not changed: the schema author
// chose the value and the caller can neither fix it nor be blamed for it. What
// they must not get is the id.
//
// Reported through the same `not_found` reason every other visibility failure
// collapses to, so a caller cannot tell "the default names something you may
// not see" from "the default names nothing".
//
// `notADefault` is every key that is NOT a destination default — the caller's
// own values and the values carried from a source item. Both are excluded
// deliberately and for different reasons: a caller's value is refused
// elsewhere by the visibility check on its own door, and a CARRIED value must
// never be refused or dropped for visibility at all, because that would make
// an item referencing something the mover cannot see unmovable. The first
// version of this took "present before the late pass", which at a migrate door
// includes the defaults MigrateFields injected — so exactly the values it
// exists to check were the ones it skipped (codex round 13).
func (s *Server) dropInvisibleRelationDefaults(
	r *http.Request,
	workspaceID string,
	role string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	notADefault map[string]bool,
) ([]store.RelationIssue, error) {
	var dropped []store.RelationIssue
	for _, def := range schema.Fields {
		if def.Type != "relation" || notADefault[def.Key] {
			continue
		}
		id, isStr := fieldMap[def.Key].(string)
		if !isStr || strings.TrimSpace(id) == "" {
			continue
		}
		item, err := s.store.GetItem(id)
		if err != nil {
			return nil, err
		}
		if item == nil || item.WorkspaceID != workspaceID {
			// Resolved a moment ago and gone now, or resolved into another
			// workspace: either way the id in the map names nothing this
			// requester can be shown, and leaving it there keeps a dangling
			// canonical id AND skips the required-field handling below (codex
			// round 14). Dropped like any other default that does not stand.
			dropped = append(dropped, store.RelationIssue{
				Key: def.Key, Target: def.Collection, Reason: store.RelationTargetNotFound,
			})
			delete(fieldMap, def.Key)
			continue
		}
		visible, err := s.checkItemVisible(workspaceID, item, currentUser(r), role, isBearerAuth(r))
		if err != nil {
			return nil, err
		}
		if visible {
			continue
		}
		dropped = append(dropped, store.RelationIssue{
			Key: def.Key, Target: def.Collection, Reason: store.RelationTargetNotFound,
		})
		delete(fieldMap, def.Key)
	}
	return dropped, nil
}

// resolveRelationsForWrite is the whole relation decision for a WRITE door —
// create and the full-`fields` update, which ran the identical four steps.
//
// Extracted after codex round 10 showed the same rule stated at a third site
// (CONVE-139: consolidate when the move is mechanical, do not defer for
// effort). The rule is: the CALLER's values are refused, and everything
// validation filled in is dropped and reported. Four steps expressed that, and
// two doors each spelled all four out:
//
//  1. resolve the whole map, which is caller input plus injected defaults;
//  2. keep only the issues on keys the caller actually supplied — a default is
//     asserted by nobody, so an unresolvable one must not refuse the write;
//  3. resolve the defaults validation injected AFTER the main pass, dropping
//     what does not resolve — except in a REQUIRED field, where dropping
//     would store the item with that field absent;
//  4. drop a default whose target the caller cannot see, so the response does
//     not hand back its id.
//
// NOT extended to the migrate doors, and not for effort: they reach the same
// rule through `store.MigrateRelationReferents`, which is in `internal/store`
// and cannot call this — the visibility layer here is request-scoped by
// construction, and `store` cannot import `server`. Unifying the two families
// needs a caller-supplied visibility predicate on the store API; that is
// IDEA-2886, with its own door table.
//
// `refusals` are the caller's to fix; `dropped` are keys the write discarded
// and must report.
func (s *Server) resolveRelationsForWrite(
	r *http.Request,
	workspaceID string,
	role string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	presentBefore map[string]bool,
	posture relationPosture,
) (refusals []store.RelationIssue, dropped []string, err error) {
	issues, err := s.resolveRelationReferents(r, workspaceID, schema, fieldMap)
	if err != nil {
		return nil, nil, err
	}
	callerIssues := store.IssuesForCallerInput(issues, presentBefore)
	if len(callerIssues) > 0 && posture == relationsRefuse {
		// The write is about to be REFUSED, so steps 3 and 4 would only
		// prepare a field map nobody stores.
		return callerIssues, nil, nil
	}
	// A CARRYING door does not stop here, and this is the round-15 defect:
	// the early return was written when every caller of this path refused, and
	// stayed when the artifact-import door began carrying instead. An import
	// holding ONE unresolvable caller value would skip the late-default pass
	// AND the default-visibility drop entirely, storing whatever the
	// destination schema injected — including a default the caller cannot see
	// — and reporting none of it. The refusals are still returned; they are
	// the carry report, not a stop.

	lateDropped, err := s.store.ResolveLateRelationDefaults(workspaceID, schema, fieldMap, presentBefore)
	if err != nil {
		return nil, nil, err
	}
	if cerr := s.collapseInvisibleRelationIssues(r, workspaceID, role, lateDropped); cerr != nil {
		return nil, nil, cerr
	}
	if required := store.RequiredRelationIssues(schema, lateDropped); len(required) > 0 {
		return append(callerIssues, required...), nil, nil
	}

	invisible, err := s.dropInvisibleRelationDefaults(r, workspaceID, role, schema, fieldMap, presentBefore)
	if err != nil {
		return nil, nil, err
	}
	// The required check covers the INVISIBLE drops too. It was written for
	// `lateDropped` and a sibling list was added beside it a round later —
	// deleting a key after validation has passed leaves a required field
	// absent regardless of which list recorded it (codex round 13).
	if required := store.RequiredRelationIssues(schema, invisible); len(required) > 0 {
		return append(callerIssues, required...), nil, nil
	}
	for _, ri := range append(lateDropped, invisible...) {
		dropped = append(dropped, ri.Key)
	}
	// callerIssues is EMPTY on a refusing door — it returned above — and holds
	// the carry report on a carrying one. Returning it here rather than `nil`
	// is what keeps an import's unresolvable values named in the response now
	// that the carrying door runs to the end.
	return callerIssues, dropped, nil
}
