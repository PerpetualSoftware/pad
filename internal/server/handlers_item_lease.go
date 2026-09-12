package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// Item execution lease endpoints (#1221):
//
//	POST /api/v1/workspaces/{slug}/items/{itemSlug}/claim
//	POST /api/v1/workspaces/{slug}/items/{itemSlug}/release
//
// A lease answers "who is actively executing this right now" — distinct
// from assignment's longer-term ownership. The store's conditional UPDATE
// is the arbiter (see store/item_lease.go), so two callers racing on the
// same "unclaimed" snapshot produce one winner and one structured 409.
//
// Deliberately NOT wired: an SSE/activity event, an updated_at bump, or a
// version entry. A lease is runtime coordination state, not content — a
// claim must never 409 a concurrent editor's expected_updated_at token,
// and a heartbeat re-claim every few minutes must not spam the feed.

const (
	defaultLeaseTTL = 15 * time.Minute
	maxLeaseTTL     = 24 * time.Hour
)

// itemLeaseInput is the request body for claim and release. Everything is
// optional: holder defaults to the authenticated identity, ttl_seconds to
// defaultLeaseTTL (claim only).
type itemLeaseInput struct {
	Holder     string `json:"holder,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// handleClaimItem acquires (or, for the live holder, refreshes) the
// execution lease on an item.
func (s *Server) handleClaimItem(w http.ResponseWriter, r *http.Request) {
	item, holder, input, ok := s.resolveLeaseRequest(w, r)
	if !ok {
		return
	}

	ttl := defaultLeaseTTL
	if input.TTLSeconds != 0 {
		if input.TTLSeconds < 0 || time.Duration(input.TTLSeconds)*time.Second > maxLeaseTTL {
			writeError(w, http.StatusBadRequest, "bad_request",
				"ttl_seconds must be between 1 and 86400 (omit it for the 15-minute default)")
			return
		}
		ttl = time.Duration(input.TTLSeconds) * time.Second
	}

	lease, err := s.store.ClaimItemLease(item.ID, holder, ttl)
	if err != nil {
		var held *store.LeaseHeldError
		if errors.As(err, &held) {
			writeLeaseHeldError(w, item.Ref, held)
			return
		}
		writeInternalError(w, err)
		return
	}

	// Structured success body (the BUG-1081 pattern): enough to be the
	// caller's next source of truth without a follow-up read.
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":   item.Ref,
		"lease": lease,
	})
}

// handleReleaseItem clears the caller's lease. Idempotent: releasing an
// absent or expired lease answers released=false, never an error.
func (s *Server) handleReleaseItem(w http.ResponseWriter, r *http.Request) {
	item, holder, _, ok := s.resolveLeaseRequest(w, r)
	if !ok {
		return
	}

	released, err := s.store.ReleaseItemLease(item.ID, holder)
	if err != nil {
		var held *store.LeaseHeldError
		if errors.As(err, &held) {
			writeLeaseHeldError(w, item.Ref, held)
			return
		}
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ref":      item.Ref,
		"released": released,
	})
}

// resolveLeaseRequest performs the shared claim/release preamble: resolve
// workspace + item, check visibility, require an authenticated user, and
// decode the optional body. holder falls back to the authenticated
// user's email (their durable, human-readable identity; the #879 named
// profiles become the natural source once layer 2 lands) and then to the
// user id when the account has no email.
func (s *Server) resolveLeaseRequest(w http.ResponseWriter, r *http.Request) (item *resolvedLeaseItem, holder string, input itemLeaseInput, ok bool) {
	workspaceID, wok := s.getWorkspaceID(w, r)
	if !wok {
		return nil, "", input, false
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	resolved, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return nil, "", input, false
	}
	if resolved == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return nil, "", input, false
	}
	if !s.requireItemVisible(w, r, workspaceID, resolved) {
		return nil, "", input, false
	}

	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return nil, "", input, false
	}

	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
			return nil, "", input, false
		}
	}

	holder = input.Holder
	if holder == "" {
		holder = user.Email
	}
	if holder == "" {
		holder = user.ID
	}

	return &resolvedLeaseItem{ID: resolved.ID, Ref: resolved.Ref}, holder, input, true
}

// resolvedLeaseItem is the slice of the resolved item the lease handlers
// need — id for the store call, ref for the response envelope.
type resolvedLeaseItem struct {
	ID  string
	Ref string
}

// writeLeaseHeldError emits the structured 409 for a live foreign lease —
// the same envelope discipline as update_conflict: a stable `code` plus
// details a client can act on (wait until expires_at, skip, or escalate)
// without parsing the message.
func writeLeaseHeldError(w http.ResponseWriter, ref string, held *store.LeaseHeldError) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]any{
			"code":    "lease_held",
			"message": held.Error(),
			"details": map[string]any{
				"ref":         ref,
				"holder":      held.Holder,
				"acquired_at": held.AcquiredAt.UTC().Format(time.RFC3339),
				"expires_at":  held.ExpiresAt.UTC().Format(time.RFC3339),
			},
		},
	})
}
