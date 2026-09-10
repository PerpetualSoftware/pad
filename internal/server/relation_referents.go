package server

import (
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/items"
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
			// U6, lead ruling: title resolution is scoped to what the caller
			// can SEE. Among live exact-title matches in the declared
			// collection, only VISIBLE ones count — zero is not_found, one
			// resolves, two or more is ambiguous — so a hidden match never
			// changes the answer a caller gets. The store cannot apply that
			// (it has no requester), so its `ambiguous` is provisional and is
			// re-decided here against the candidates it carried up.
			if ri.Reason == store.RelationTargetAmbiguous {
				resolvedID, rerr := s.narrowAmbiguousRelationTitle(r, workspaceID, role, ri)
				if rerr != nil {
					return nil, rerr
				}
				switch resolvedID {
				case ambiguityStands:
					// Two or more visible matches: the caller really cannot be
					// handed one, and "be more specific" is theirs to act on.
				case ambiguityNoneVisible:
					// Every match is hidden. Saying `ambiguous` would tell the
					// caller that two or more items they cannot see carry the
					// title they guessed, which is the same oracle the
					// wrong_collection collapse closes — and titles are
					// guessable in a way refs are not.
					collapseIssue(issues, def.Key, store.RelationTargetNotFound)
				default:
					// Exactly one visible match. It is not ambiguous FOR THIS
					// CALLER, so it resolves and the issue goes away entirely.
					fieldMap[def.Key] = resolvedID
					issues = removeIssue(issues, def.Key)
				}
				continue
			}
			if ri.Reason != store.RelationTargetWrongCollection {
				continue
			}
			// U6: a TITLE-derived issue carries the id of the item the store
			// matched, and MUST be judged on that item. Re-resolving it here
			// would run the value through ResolveRelationTarget's UUID-or-ref
			// ladder, which does not speak titles: it would find nothing, take
			// the `target == nil` arm below, and collapse EVERY title-derived
			// wrong_collection to not_found — including the visible ones this
			// rule exists to keep specific. A test that only checks the
			// invisible leg cannot see that happen.
			//
			// The ref path keeps its re-resolve unchanged. It is re-resolvable
			// by construction, and the TOCTOU its second lookup opens is filed
			// separately rather than fixed inside this unit.
			var target *models.Item
			var terr error
			if ri.MatchedID != "" {
				target, terr = s.store.GetItem(ri.MatchedID)
			} else {
				target, terr = s.store.ResolveRelationTarget(workspaceID, ri.Value)
			}
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

// ambiguityStands and ambiguityNoneVisible are the two non-id outcomes of
// narrowAmbiguousRelationTitle. Sentinels rather than a second return value
// because there are exactly three answers and a (string, bool, bool) signature
// invites callers to check them in the wrong order.
const (
	ambiguityStands      = "\x00stands"
	ambiguityNoneVisible = "\x00none"
)

// narrowAmbiguousRelationTitle re-decides a store-provisional `ambiguous`
// against what THIS requester can see (PLAN-2857 U6, lead ruling).
//
// Returns the single visible match's id, or one of the two sentinels above.
//
// An ambiguous issue with NO candidates means the store's match count exceeded
// its cap and it declined to hand up ids; the answer stands for everyone, which
// is both the safe direction and the right one — nobody can be meaningfully
// handed one of that many same-titled items.
func (s *Server) narrowAmbiguousRelationTitle(
	r *http.Request,
	workspaceID, role string,
	ri store.RelationIssue,
) (string, error) {
	if ri.TitleCollectionID == "" {
		// Not a title-derived ambiguity, so there is nothing to narrow.
		return ambiguityStands, nil
	}
	user, bearer := currentUser(r), isBearerAuth(r)

	visible := ""
	count := 0
	after := 0
	for {
		ids, numbers, err := s.store.RelationTitleCandidatesQ(
			s.store.Q(), workspaceID, ri.TitleCollectionID, ri.Value, after, relationTitleCandidatePage)
		if err != nil {
			return "", err
		}
		if len(ids) == 0 {
			break
		}
		for i, id := range ids {
			after = numbers[i]
			item, err := s.store.GetItem(id)
			if err != nil {
				return "", err
			}
			if item == nil {
				// Deleted between the resolver's read and this one. Not a match
				// any more, so it does not count towards ambiguity — the same
				// treatment the vanished-target arm gives elsewhere.
				continue
			}
			seen, err := s.checkItemVisible(workspaceID, item, user, role, bearer)
			if err != nil {
				return "", err
			}
			if !seen {
				continue
			}
			count++
			if count > 1 {
				// Two visible matches is the answer; nothing later can change
				// it, so the remaining pages are never read.
				return ambiguityStands, nil
			}
			visible = item.ID
		}
		if len(ids) < relationTitleCandidatePage {
			break
		}
	}
	if count == 0 {
		return ambiguityNoneVisible, nil
	}
	return visible, nil
}

// relationTitleCandidatePage is the page size for that walk. It bounds MEMORY,
// not the answer: the loop stops at the second visible match or at the end of
// the rows, so no match count changes what a caller is told. A cap on the
// ANSWER was the first shape and it was an oracle — "one visible plus cap
// hidden" read differently from "one visible plus cap+1 hidden".
//
// The worst case is a scan of every live item sharing one exact title in one
// collection, which is a set the workspace's own writers created and is
// normally 1.
const relationTitleCandidatePage = 100

// removeIssue drops the issue raised for key. Used when a refusal turns out not
// to apply to this requester after all — an `ambiguous` that resolves for a
// caller who can see exactly one of the matches.
func removeIssue(issues []store.RelationIssue, key string) []store.RelationIssue {
	out := issues[:0]
	for _, ri := range issues {
		if ri.Key != key {
			out = append(out, ri)
		}
	}
	return out
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
// Returns the issues that SURVIVE. An `ambiguous` that resolved for this
// caller is not a drop and must not be counted as one — returning the slice
// rather than marking entries in place makes every call site reassign, so the
// compiler enforces what a "remember to filter afterwards" helper would only
// ask for.
func (s *Server) collapseInvisibleRelationIssues(r *http.Request, workspaceID, role string, issues []store.RelationIssue, fieldMap map[string]any) ([]store.RelationIssue, error) {
	var resolved []string
	for i := range issues {
		// A late-default `ambiguous` gets the same narrowing AND the same
		// resolution the main pass applies. Downgrading a single visible match
		// to not_found here — which is what this did first — still leaks the
		// hidden twin: the caller sees a default RESOLVE when they are the only
		// match and FAIL when a twin they cannot see exists, so the outcome
		// itself answers the question the reason was careful not to (codex
		// round 2 on U6).
		//
		// The field map is threaded in for exactly this. ResolveLateRelationDefaults
		// has already deleted the key, so resolving means putting it back.
		if issues[i].Reason == store.RelationTargetAmbiguous {
			narrowed, nerr := s.narrowAmbiguousRelationTitle(r, workspaceID, role, issues[i])
			if nerr != nil {
				return nil, nerr
			}
			switch narrowed {
			case ambiguityStands:
				// Two or more visible: genuinely ambiguous for this caller.
			case ambiguityNoneVisible:
				issues[i].Reason = store.RelationTargetNotFound
			default:
				if fieldMap != nil {
					fieldMap[issues[i].Key] = narrowed
					resolved = append(resolved, issues[i].Key)
					break
				}
				// No field map to restore into: the honest answer is the one
				// that discloses least.
				issues[i].Reason = store.RelationTargetNotFound
			}
			continue
		}
		if issues[i].Reason != store.RelationTargetWrongCollection {
			continue
		}
		// U6: a TITLE-derived issue carries the id the store matched and must
		// be judged on THAT item. ResolveRelationTarget speaks a UUID-or-ref
		// ladder and cannot resolve a title, so re-resolving one here finds
		// nothing, takes the `target == nil` arm below, and collapses every
		// title-derived wrong_collection — including the VISIBLE ones this
		// rule exists to keep specific. The main pass was fixed for exactly
		// this; the late pass is a second road to the same defect, which is
		// why it is fixed here rather than only there (codex round 1 on U6).
		var target *models.Item
		var terr error
		if issues[i].MatchedID != "" {
			target, terr = s.store.GetItem(issues[i].MatchedID)
		} else {
			target, terr = s.store.ResolveRelationTarget(workspaceID, issues[i].Value)
		}
		if terr != nil {
			return nil, terr
		}
		if target == nil {
			// Vanished between the two reads. The reason still SAYS the value
			// named something a moment ago, which is the same disclosure.
			issues[i].Reason = store.RelationTargetNotFound
			continue
		}
		seen, verr := s.checkItemVisible(workspaceID, target, currentUser(r), role, isBearerAuth(r))
		if verr != nil {
			return nil, verr
		}
		if !seen {
			issues[i].Reason = store.RelationTargetNotFound
		}
	}
	if len(resolved) == 0 {
		return issues, nil
	}
	kept := make([]store.RelationIssue, 0, len(issues))
	for _, ri := range issues {
		if !slices.Contains(resolved, ri.Key) {
			kept = append(kept, ri)
		}
	}
	return kept, nil
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

// notDefaultKeys delegates to store.NotDefaultKeys.
//
// The rule moved into `store` because the cross-workspace COPY needs it from
// inside its transaction, and the lead's day-58 ruling is that both doors run
// ONE classifier rather than two kept in step by hand. Kept as a wrapper so
// the four server call sites read unchanged.
func notDefaultKeys(supplied, carried map[string]any) map[string]bool {
	return store.NotDefaultKeys(supplied, carried)
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
// dropInvisibleRelationDefaults removes destination-schema defaults whose
// target this requester cannot see, reporting each as a `not_found` drop.
//
// The body moved to store.DropInvisibleRelationDefaultsQ so the cross-workspace
// COPY — which resolves its late defaults inside its own transaction and could
// not reach a *Server method — runs the SAME code rather than a second
// implementation of the same rule. That door was the one site of five that
// never got this pass, because round 15's sweep enumerated the sites reachable
// from `internal/server` and the class is one wider than the package
// (night-11 finding, lead ruling day 58).
//
// The request-scoped part stays here, as the closure: who is asking, their role
// in this workspace, and whether the call is bearer-authenticated.
func (s *Server) dropInvisibleRelationDefaults(
	r *http.Request,
	workspaceID string,
	role string,
	schema models.CollectionSchema,
	fieldMap map[string]any,
	notADefault map[string]bool,
) ([]store.RelationIssue, error) {
	return s.store.DropInvisibleRelationDefaultsQ(s.store.Q(), workspaceID,
		s.relationVisibility(r, role), schema, fieldMap, notADefault)
}

// relationVisibility binds the request-scoped half of the visibility rule into
// a callback the store can run on ITS executor. This is what lets the copy
// apply the rule inside the transaction that holds both workspaces' advisory
// locks — the shape `checkItemVisibleQ` is parameterised for (BUG-2409).
func (s *Server) relationVisibility(r *http.Request, role string) store.RelationVisibilityFunc {
	user := currentUser(r)
	bearer := isBearerAuth(r)
	return func(q store.Queryer, workspaceID string, item *models.Item) (bool, error) {
		return s.checkItemVisibleQ(q, workspaceID, item, user, role, bearer)
	}
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
	lateDropped, cerr := s.collapseInvisibleRelationIssues(r, workspaceID, role, lateDropped, fieldMap)
	if cerr != nil {
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

// structuralOverrideError classifies the problems with a caller's field
// overrides that are about the REQUEST rather than about the workspace's
// state, and is the one classifier both copy doors run before any semantic
// check (lead ruling, day 58).
//
// Two kinds, in this order:
//
//	malformed_override — an override names a field the destination schema does
//	                     not declare.
//	invalid_override   — an override's VALUE fails the destination's type for
//	                     that field.
//
// STRUCTURAL BEFORE SEMANTIC, and the preflight's order is the contract. The
// copy used to run refuseInvisibleRelationOverrides — a question about what
// this caller may SEE — before the store call that performs these checks, so
// `{"owner_ref":"<invisible>","ghost":"x"}` returned `malformed_override` from
// the preflight and `validation_error` from the copy, and `{"owner_ref":42}`
// returned `invalid_override` from the preflight and `validation_error` from
// the copy. One body, two answers, which is the DR-6 divergence this pair
// exists to prevent (codex round 19).
//
// One function rather than two orderings kept in step by hand: the previous
// arrangement was not that anyone chose the wrong order, it was that the order
// was an emergent property of where each check happened to live.
//
// Returns ok=true when nothing structural is wrong.
func structuralOverrideError(overrides map[string]any, targetSchema models.CollectionSchema) (code, message string, ok bool) {
	if len(overrides) == 0 {
		return "", "", true
	}
	schema := items.SchemaForMigratedFields(targetSchema)
	if bad := items.UndeclaredOverrideKeys(overrides, schema.Fields); len(bad) > 0 {
		return "malformed_override", "Destination collection has no field(s): " + summarizeKeys(bad), false
	}
	// Validated in ISOLATION — the overrides alone, never merged over the
	// migrated map — so the answer cannot depend on the source item's
	// contents. That is the same property the undeclared check has, and it is
	// what lets this run before anything reads the source.
	probe := make(map[string]any, len(overrides))
	for k, v := range overrides {
		if v == nil {
			// An explicit null is a CLEAR, not a value, and has no type to
			// fail. Validating it here would refuse the documented way to
			// blank a field.
			continue
		}
		probe[k] = v
	}
	if len(probe) == 0 {
		return "", "", true
	}
	// COERCED FIRST, exactly as both doors coerce before validating (BUG-2850).
	// Without this the classifier refuses a value the copy would happily
	// accept — `{"cost":"42"}` against a number field is coercible, and
	// TestCopyAndPreflightCoerceIdentically pins that it must be accepted at
	// both doors. Hoisting a check above the coercion that makes it pass is
	// the same class of error as the ordering this function exists to fix.
	probe = items.CoerceFields(probe, schema)
	var bad []string
	for _, iss := range items.ValidateFieldsDetailed(probe, schema) {
		if iss.Kind != items.IssueInvalid {
			// IssueRequired and friends are about the FINAL map, which this
			// probe is not — a required field the overrides do not mention is
			// not an override problem.
			continue
		}
		if _, overridden := overrides[iss.Key]; !overridden {
			continue
		}
		bad = append(bad, iss.Message)
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		// Bounded for the same reason the malformed_override message is:
		// validateFieldType quotes the offending VALUE verbatim, so a single
		// large override string would otherwise be reflected back in full.
		return "invalid_override", "Invalid override value(s): " + summarizeMessages(bad), false
	}
	return "", "", true
}
