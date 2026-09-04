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
		if !isStr || id == "" {
			continue
		}
		if issuesContainKey(issues, def.Key) {
			continue // already unresolvable; no second complaint about it
		}
		item, err := s.store.GetItem(id)
		if err != nil {
			return nil, err
		}
		if item == nil {
			continue // resolved a moment ago; treat a race as someone else's 404
		}
		visible, err := s.checkItemVisible(workspaceID, item, currentUser(r), workspaceRole(r), isBearerAuth(r))
		if err != nil {
			return nil, err
		}
		if !visible {
			issues = append(issues, store.RelationIssue{
				Key: def.Key, Value: id, Target: def.Collection, Reason: store.RelationTargetNotFound,
			})
		}
	}
	return issues, nil
}

func issuesContainKey(issues []store.RelationIssue, key string) bool {
	for _, ri := range issues {
		if ri.Key == key {
			return true
		}
	}
	return false
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
func relationIssuesMessage(issues []store.RelationIssue) string {
	parts := make([]string, 0, len(issues))
	for _, ri := range issues {
		parts = append(parts, ri.Message())
	}
	return strings.Join(parts, "; ")
}
