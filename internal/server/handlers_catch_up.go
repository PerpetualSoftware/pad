package server

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// catchUpCeiling is how far back the catch-up digest ever reaches
// (TASK-3615, Dave's 10-17 answer). With no cursor the window IS the ceiling.
const catchUpCeiling = 14 * 24 * time.Hour

// catchUpWindow is the period the digest covers.
type catchUpWindow struct {
	Since string `json:"since"`
	Until string `json:"until"`
	// HasCursor is false until the viewer first marks the workspace caught up.
	HasCursor bool `json:"has_cursor"`
	// CappedAtCeiling says the window starts at the 14-day ceiling rather
	// than at the cursor: there is no cursor, or it is older than that.
	CappedAtCeiling bool `json:"capped_at_ceiling"`
}

type catchUpResponse struct {
	Window         catchUpWindow        `json:"window"`
	NeedsYou       store.CatchUpSection `json:"needs_you"`
	Finished       store.CatchUpSection `json:"finished"`
	NewAndReopened store.CatchUpSection `json:"new_and_reopened"`
	Comments       store.CatchUpSection `json:"comments"`
	Agents         store.CatchUpAgents  `json:"agents"`
	// Degraded names a section whose source failed and came back empty, so a
	// client can tell "nothing" from "could not read" (the dashboard's rule,
	// BUG-2014). Today only needs_you's decision source can degrade.
	Degraded []string `json:"degraded,omitempty"`
}

// handleGetCatchUp returns the per-user "catch me up" digest (TASK-3615):
// what others did in the workspace since the viewer last marked it caught
// up, at most 14 days back, plus what needs the viewer now. A GET never
// writes: with no cursor the window is computed, not stored, and only
// PUT /me/seen creates or moves the cursor.
//
//	GET /workspaces/{slug}/catch-up
func (s *Server) handleGetCatchUp(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to see what changed since your last visit")
		return
	}

	until := time.Now().UTC()
	ceiling := until.Add(-catchUpCeiling)
	cursor, found, err := s.store.ReadCursor(user.ID, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	since := ceiling
	if found && cursor.After(ceiling) {
		since = cursor
	}

	// The same visibility every list door applies: collection access (with
	// an app token's ceiling), and item grants for a guest or a restricted
	// member. Totals are counted under the same scope as the lines, so a
	// total cannot reveal an item the viewer cannot open.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	scope := store.CatchUpScope{CollectionIDs: visibleIDs}
	if len(grantedItemIDs) > 0 {
		scope = store.CatchUpScope{CollectionIDs: fullCollIDs, ItemIDs: grantedItemIDs}
	}
	// A nil CollectionIDs means "no restriction" to the store, so a grant
	// holder with no full-collection access must carry an empty, non-nil set.
	if scope.CollectionIDs == nil && len(grantedItemIDs) > 0 {
		scope.CollectionIDs = []string{}
	}

	cu, err := s.store.CatchUp(workspaceID, user.ID, since, until, scope)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	resp := catchUpResponse{
		Window: catchUpWindow{
			Since:           since.Format(time.RFC3339),
			Until:           until.Format(time.RFC3339),
			HasCursor:       found,
			CappedAtCeiling: !since.After(ceiling),
		},
		Finished:       cu.Finished,
		NewAndReopened: cu.NewAndReopened,
		Comments:       cu.Comments,
		Agents:         cu.Agents,
	}

	needs, degraded, err := s.catchUpNeedsYou(r, workspaceID, user.ID, visibleIDs, fullCollIDs, grantedItemIDs, scope)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp.NeedsYou = needs
	if degraded {
		resp.Degraded = []string{"needs_you"}
	}
	writeJSON(w, http.StatusOK, resp)
}

// catchUpNeedsYou is current state, not windowed: items the attention set
// says are waiting on a human, then open items assigned to the viewer that
// something visible and unfinished still blocks. Roles are agent roles, so
// "yours" means assigned to you (lead ruling, v1). A failing decision
// provider degrades the section rather than failing the digest.
func (s *Server) catchUpNeedsYou(r *http.Request, workspaceID, viewerID string, visibleIDs, fullCollIDs, grantedItemIDs []string, scope store.CatchUpScope) (store.CatchUpSection, bool, error) {
	var ids []string
	what := map[string]string{}
	degraded := false

	flags, failing, err := s.decisionRunner().WorkspaceFlags(workspaceID, decision.AttentionSetName, decision.AttentionThreshold)
	if err != nil {
		slog.Error("catch-up: attention decisions failed; needs_you degraded", "workspace_id", workspaceID, "error", err)
		degraded = true
	} else {
		if failing {
			slog.Warn("catch-up: decision provider is failing; attention answers may be out of date", "workspace_id", workspaceID)
			degraded = true
		}
		type flagged struct {
			id string
			p  float64
		}
		var fl []flagged
		for id, answers := range flags {
			if p, ok := answers[decision.AttentionNeedsHuman]; ok {
				fl = append(fl, flagged{id, p})
			}
		}
		sort.Slice(fl, func(a, b int) bool {
			if fl[a].p != fl[b].p {
				return fl[a].p > fl[b].p
			}
			return fl[a].id < fl[b].id
		})
		for _, f := range fl {
			ids = append(ids, f.id)
			what[f.id] = "needs a decision"
		}
	}

	if !(scope.CollectionIDs != nil && len(scope.CollectionIDs) == 0 && len(scope.ItemIDs) == 0) {
		collections, err := s.store.ListCollectionsMinimal(workspaceID)
		if err != nil {
			return store.CatchUpSection{}, false, err
		}
		ctxMap := buildDoneContextMap(collections)
		blockers, err := s.firstActiveBlockers(r, workspaceID, visibleIDs, fullCollIDs, grantedItemIDs, ctxMap)
		if err != nil {
			return store.CatchUpSection{}, false, err
		}
		if len(blockers) > 0 {
			mine, err := s.store.ListItems(workspaceID, models.ItemListParams{
				AssignedUserID: viewerID,
				CollectionIDs:  scope.CollectionIDs,
				ItemIDs:        scope.ItemIDs,
			})
			if err != nil {
				return store.CatchUpSection{}, false, err
			}
			for _, it := range mine {
				if _, blocked := blockers[it.ID]; !blocked {
					continue
				}
				// Reference items (docs) are not open work (PLAN-3535), as
				// the dashboard's blocked attention, which reads workItems.
				if isReferenceCollection(it.CollectionID, ctxMap) || isItemDone(it.Fields, it.CollectionID, ctxMap) {
					continue
				}
				if _, dup := what[it.ID]; dup {
					continue
				}
				ids = append(ids, it.ID)
				what[it.ID] = "blocked"
			}
		}
	}

	sec, err := s.store.CatchUpItemLines(workspaceID, ids, what, scope)
	if err != nil {
		return store.CatchUpSection{}, false, err
	}
	return sec, degraded, nil
}

// handleMarkCaughtUp moves the viewer's cursor for the workspace to now. It
// takes no body: a client-supplied time could move the cursor into the
// future and hide what has not been seen. The cursor never moves backwards.
//
//	PUT /workspaces/{slug}/me/seen
func (s *Server) handleMarkCaughtUp(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to mark this workspace caught up")
		return
	}
	seen, err := s.store.MarkCaughtUp(user.ID, workspaceID, time.Now())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"seen_at": seen.UTC().Format(time.RFC3339)})
}
