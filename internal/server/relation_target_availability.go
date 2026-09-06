package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// relationTargetsUnavailable reports which relation TARGET collections declared
// by a destination schema this caller cannot actually use (IDEA-2899).
//
// The copy dialog offers a picker for a `needs_value` relation row as soon as
// the row names a target collection (TASK-2869). Naming one is not the same as
// having one: the slug can name a collection that has been deleted, or one this
// caller cannot read. In both cases the dialog mounts a picker that can return
// nothing, and because the row is not blocked, Confirm stays disabled with only
// the generic required-field message — the user is told a value is missing and
// never told that no value is reachable.
//
// THE CLIENT CANNOT ANSWER THIS ITSELF, which is why it is here. The dialog's
// destination collection list is filtered through `canEditCollection`, because
// it drives the copy-INTO picker. A relation TARGET needs only READ access, so
// a perfectly usable target routinely does not appear in that list; testing
// against it would refuse rows the user could have filled in. Over-blocking is
// the worse failure — it is invisible to the person hitting it — so the
// decision belongs where the read-scoped view exists.
//
// `visibleCollectionIDs` is that view, and its NAV-LENIENT shape is right here
// rather than merely tolerable: it includes a collection reachable only through
// an item-level grant, and the question this answers is "could a picker here
// return anything at all". One granted item is a picker with one row, which is
// usable. The stricter full-access set would reject it and be wrong.
//
// Costs nothing for the overwhelming majority of calls: a schema declaring no
// relation field runs no query at all.
func (s *Server) relationTargetsUnavailable(
	r *http.Request,
	workspaceID string,
	schema models.CollectionSchema,
) (map[string]bool, error) {
	targets := make(map[string]bool)
	for _, def := range schema.Fields {
		if def.Type == "relation" && def.Collection != "" {
			targets[def.Collection] = true
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}

	// nil means no filtering — an admin on a cookie session, or an
	// unauthenticated caller. Everything live is then readable.
	visible, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		return nil, err
	}
	var visibleSet map[string]bool
	if visible != nil {
		visibleSet = make(map[string]bool, len(visible))
		for _, id := range visible {
			visibleSet[id] = true
		}
	}

	unavailable := make(map[string]bool)
	for slug := range targets {
		coll, err := s.store.GetCollectionBySlug(workspaceID, slug)
		if err != nil {
			return nil, err
		}
		// GetCollectionBySlug already excludes soft-deleted rows, so a nil
		// result covers BOTH "never existed" and "was deleted". They are
		// different facts with the same consequence — no picker can be built —
		// and the wire deliberately does not distinguish them; see the
		// `CollectionUnavailable` doc comment.
		if coll == nil {
			unavailable[slug] = true
			continue
		}
		if visibleSet != nil && !visibleSet[coll.ID] {
			unavailable[slug] = true
		}
	}
	return unavailable, nil
}
