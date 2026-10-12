package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
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
	Holder string `json:"holder,omitempty"`
	// WorkerID is the caller's lease worker id (TASK-3613); it falls back to
	// defaultLeaseWorker.
	WorkerID   string `json:"worker_id,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// handleClaimItem acquires (or, for the live holder, refreshes) the
// execution lease on an item.
func (s *Server) handleClaimItem(w http.ResponseWriter, r *http.Request) {
	item, holder, worker, input, ok := s.resolveLeaseRequest(w, r)
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

	lease, err := s.store.ClaimItemLease(item.ID, holder, worker, currentUserID(r), ttl)
	if err != nil {
		var held *store.LeaseHeldError
		if errors.As(err, &held) {
			writeLeaseHeldError(w, item.Ref, held, holder, worker)
			return
		}
		writeInternalError(w, err)
		return
	}

	// Structured success body (the BUG-1081 pattern): enough to be the
	// caller's next source of truth without a follow-up read.
	// worker_id is the caller's OWN full id, returned so a client that did
	// not mint it (a remote MCP session's derived id) can pass it back to
	// resume the lease from another connection (TASK-3613). Reads only
	// ever show the short form.
	body := map[string]any{
		"ref":   item.Ref,
		"lease": lease,
	}
	if worker != "" {
		body["worker_id"] = worker
	}
	writeJSON(w, http.StatusOK, body)
}

// handleReleaseItem clears the caller's lease. Idempotent: releasing an
// absent or expired lease answers released=false, never an error.
func (s *Server) handleReleaseItem(w http.ResponseWriter, r *http.Request) {
	item, holder, worker, _, ok := s.resolveLeaseRequest(w, r)
	if !ok {
		return
	}

	released, err := s.store.ReleaseItemLease(item.ID, holder, worker, currentUserID(r))
	if err != nil {
		var held *store.LeaseHeldError
		if errors.As(err, &held) {
			writeLeaseHeldError(w, item.Ref, held, holder, worker)
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
// decode the optional body. holder falls back to defaultLeaseHolder: the
// request's agent name, then the account's email, then its id.
func (s *Server) resolveLeaseRequest(w http.ResponseWriter, r *http.Request) (item *resolvedLeaseItem, holder, worker string, input itemLeaseInput, ok bool) {
	workspaceID, wok := s.getWorkspaceID(w, r)
	if !wok {
		return nil, "", "", input, false
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	resolved, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return nil, "", "", input, false
	}
	if resolved == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return nil, "", "", input, false
	}
	if !s.requireItemVisible(w, r, workspaceID, resolved) {
		return nil, "", "", input, false
	}
	// A lease is a write to the item's execution state: claiming one blocks
	// others from working it, and releasing one ends someone's claim. It
	// takes edit permission, grant-aware like any item edit (BUG-3341).
	if !s.requireEditPermission(w, r, workspaceID, resolved.ID, resolved.CollectionID) {
		return nil, "", "", input, false
	}

	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return nil, "", "", input, false
	}

	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
			return nil, "", "", input, false
		}
	}

	holder = input.Holder
	if holder == "" {
		holder = defaultLeaseHolder(r, user)
	}
	worker = strings.TrimSpace(input.WorkerID)
	if worker == "" {
		worker = defaultLeaseWorker(r)
	}
	if worker != "" && !models.ValidWorkerID(worker) {
		writeError(w, http.StatusBadRequest, "invalid_worker_id",
			`worker_id must be "wk_" followed by 32 lowercase hex characters (128 random bits, minted once per agent session)`)
		return nil, "", "", input, false
	}

	return &resolvedLeaseItem{ID: resolved.ID, Ref: resolved.Ref}, holder, worker, input, true
}

// defaultLeaseHolder is the holder a claim or release names when the caller
// passes none, and the identity `pad project next` / `ready` compare leases
// against (TASK-1809): for a remote MCP call, its client name plus a short
// session id (BUG-2772); else the request's agent name (X-Pad-Agent, which
// the CLI sets from the session registry); else the account's email, else its
// id.
//
// The agent name comes first because several agents commonly share ONE
// account. With the email default, a second agent's claim matched the first
// one's label and account, so it silently refreshed that lease and both
// "won" (measured on TASK-1809). The header is self-declared, but the lease
// is still bound to the authenticated account (BUG-3341), so it can only
// split the caller's own claims, never reach into another account's.
func defaultLeaseHolder(r *http.Request, user *models.User) string {
	// A remote MCP caller holds under its declared client name. Two
	// connections of one client are told apart by their worker ids
	// (defaultLeaseWorker, TASK-3613), which replaced BUG-2772's "#xxxxxxxx"
	// label suffix: the label is display only (lead ruling).
	if c, ok := remoteMCPCaller(r); ok && c.LeaseHolder != "" {
		return c.LeaseHolder
	}
	if name := agentNameFromRequest(r); name != "" {
		return name
	}
	if user == nil {
		return ""
	}
	if user.Email != "" {
		return user.Email
	}
	return user.ID
}

// defaultLeaseWorker is the worker id a claim, release or queue read uses
// when the request body names none (TASK-3613): the X-Pad-Worker header,
// which the CLI sets from its session registry, else a `worker_id` query
// parameter, else a remote MCP caller's session-derived id. "" means a name-only caller. The value is validated by
// the caller of this function (claim/release answer 400; the queue filter
// ignores a malformed one rather than failing a read).
func defaultLeaseWorker(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Pad-Worker")); v != "" {
		return v
	}
	// A read with no body (the dashboard, so `next` / `ready`) can name one
	// in the query, which is how a remote MCP caller resuming by worker_id
	// reaches the queue filter.
	if v := strings.TrimSpace(r.URL.Query().Get("worker_id")); v != "" {
		return v
	}
	if c, ok := remoteMCPCaller(r); ok {
		return c.WorkerID
	}
	return ""
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
//
// holder and worker are the CALLER's. When a name-only caller collides with a
// worker-bound lease under its own name, the message says how to get a
// worker id (lead ruling, TASK-3613): that refusal is the collision fix, and
// a caller that hits it needs the way out more than the bare fact.
func writeLeaseHeldError(w http.ResponseWriter, ref string, held *store.LeaseHeldError, holder, worker string) {
	msg := held.Error()
	if worker == "" && held.Worker != "" && held.Holder == holder {
		msg += ". Another session holds it under the same name; leases are told apart by worker id, which this request did not send." +
			" The pad CLI sends one for a registered session (pad session register); over the API or MCP, pass worker_id (\"wk_\" + 32 hex, minted once per session)"
	}
	details := map[string]any{
		"ref":         ref,
		"holder":      held.Holder,
		"acquired_at": held.AcquiredAt.UTC().Format(time.RFC3339),
		"expires_at":  held.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if held.Worker != "" {
		details["worker"] = held.Worker
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]any{
			"code":    "lease_held",
			"message": msg,
			"details": details,
		},
	})
}
