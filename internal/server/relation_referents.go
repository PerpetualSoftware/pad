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
