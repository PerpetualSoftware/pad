package server

import (
	"encoding/json"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// hydrateRelationTargets fills `relation_targets` on a batch of items
// (PLAN-2857 U6): for each `relation` field the collection declares, the item
// the stored id names, as {id, ref, title}.
//
// It is the READ-side counterpart to the write-side resolver, and it inherits
// that resolver's division of labour: the store answers what the items say, and
// this layer — the one that knows who is asking — decides what may be said back.
//
// TWO THINGS COLLAPSE A TARGET TO ID-ONLY, and neither omits the key:
//
//   - the value resolves to nothing live in this workspace (dangling, or an id
//     naming another workspace, which the store declines to resolve);
//   - the requester cannot see the target ITEM.
//
// THAT SECOND CHECK IS PER-ITEM, NOT PER-COLLECTION, and the difference is a
// leak rather than a nicety. `store.VisibleCollectionIDs` is deliberately
// NAV-LENIENT: it folds in a collection reachable only through an ITEM-level
// grant "so the collection appears in navigation", and leaves item-level
// filtering to the handlers — requireCollectionFullyVisible's comment says so,
// and exists because BUG-1920 walked into it. Filtering hydration on that set
// would hand a member with a grant on ONE item in a collection the ref and
// title of EVERY item any relation points at there. `checkItemVisible` is the
// authority, and it is asked once per DISTINCT TARGET, not once per item.
//
// Omitting the key instead would say "this item has no relation here", which is
// a different statement and a false one. An id-only entry says the true thing:
// the value is stored, and there is nothing more this requester may be told.
// The write side already collapses `wrong_collection` to `not_found` for an
// invisible target; hydrating that item's ref and title on the read would hand
// back exactly what that collapse withholds.
//
// BEST-EFFORT. A failure here leaves items unhydrated rather than failing the
// read — the same posture enrichItemsWithParent takes for parent info, and for
// the same reason: `relation_targets` is additive convenience, and a list read
// that 500s because a convenience field could not be built is a worse answer
// than one without it.
func (s *Server) hydrateRelationTargets(r *http.Request, workspaceID string, items []models.Item, visibleIDs ...[]string) {
	if len(items) == 0 || r == nil {
		return
	}

	schemas, err := s.relationSchemasForWorkspace(workspaceID)
	if err != nil || len(schemas) == 0 {
		return
	}

	targets, err := s.store.HydrateRelationTargetsQ(s.store.DB(), workspaceID, items, schemas)
	if err != nil || len(targets) == 0 {
		return
	}

	// workspaceRole(r) is the right role here, unlike in the cross-workspace
	// copy: every item being enriched belongs to the workspace in the URL,
	// which is the workspace that role was stashed for.
	user, role, bearer := currentUser(r), workspaceRole(r), isBearerAuth(r)

	// One decision per DISTINCT target, reused across every item pointing at
	// it. A list page of 50 items sharing one target asks once.
	allowed := map[string]bool{}
	visible := func(id string) bool {
		if seen, done := allowed[id]; done {
			return seen
		}
		item, err := s.store.GetItem(id)
		if err != nil || item == nil {
			allowed[id] = false
			return false
		}
		seen, err := s.checkItemVisible(workspaceID, item, user, role, bearer)
		if err != nil {
			// Fail CLOSED. An error here is not a licence to disclose.
			seen = false
		}
		allowed[id] = seen
		return seen
	}

	for i := range items {
		perField, ok := targets[items[i].ID]
		if !ok {
			continue
		}
		out := make(map[string]models.RelationTarget, len(perField))
		for key, target := range perField {
			if target.Ref != "" && !visible(target.ID) {
				// Resolved, but not for these eyes. Keep the id — the value IS
				// stored — and drop what would disclose the target.
				target = models.RelationTarget{ID: target.ID}
			}
			out[key] = target
		}
		items[i].RelationTargets = out
	}
}

// relationSchemasForWorkspace maps collection ID -> schema for every collection
// in the workspace that DECLARES a relation field.
//
// Narrowed to relation-declaring collections on purpose: the map is handed to
// the store's hydrator, which skips a collection it cannot find, so including
// the rest would only make a larger map for it to walk past. A collection with
// no relation field contributes no work either way.
func (s *Server) relationSchemasForWorkspace(workspaceID string) (map[string]models.CollectionSchema, error) {
	colls, err := s.store.ListCollections(workspaceID)
	if err != nil {
		return nil, err
	}
	out := map[string]models.CollectionSchema{}
	for i := range colls {
		if colls[i].Schema == "" {
			continue
		}
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(colls[i].Schema), &schema); err != nil {
			// A schema that will not parse is a different defect, reported
			// elsewhere; hydration declines to be the second voice.
			continue
		}
		for _, def := range schema.Fields {
			if def.Type == "relation" {
				out[colls[i].ID] = schema
				break
			}
		}
	}
	return out, nil
}
