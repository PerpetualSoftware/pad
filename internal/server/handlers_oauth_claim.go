package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// POST /api/v1/oauth/claim — claim-code redemption endpoint
// (PLAN-1519 / TASK-1521 / IDEA-1517 §4).
//
// Lets the user grant an existing OAuth connection access to one
// specific workspace by redeeming a 6-digit stateless HMAC code the
// user generated in the web UI's "Connect project" modal (Phase E).
//
// Flow (the agent does steps 2-4):
//  1. User generates a code in the web UI for workspace W.
//  2. User reads the code to the agent.
//  3. Agent calls `pad_workspace.action: claim` with {workspace, code}.
//  4. Dispatcher hits POST /api/v1/oauth/claim with the same payload.
//
// The endpoint:
//   - Requires authentication. PAT and OAuth bearers both reach this
//     handler via the standard /api/v1 auth chain; the OAuth-only
//     side effect (adding the workspace to oauth_connection_workspaces)
//     no-ops for PAT auth (it has no request_id), leaving the endpoint
//     usable but inert from a CLI session token.
//   - Resolves the workspace by slug, checks the user is a member
//     (404 → workspace not visible).
//   - Re-derives the claim code from (user_id, workspace_id, current
//     OR previous 5-min bucket) using the server's claim secret and
//     constant-time compares. Mismatch → 401.
//   - On success, INSERTs into oauth_connection_workspaces with
//     added_by='claim'. Idempotent — re-claiming a workspace already
//     in the allow-list is a no-op (returns 200, "already_added": true).
//
// **No scope check on the calling grant.** The user generating + handing
// over the code IS the consent — IDEA-1517 §4 explicitly: "Claim works
// regardless of scope flags (orthogonal to the three toggles)." A grant
// with `all_current_workspaces=true` simply doesn't NEED the row — the
// claim still succeeds but the row is redundant; we insert anyway so
// the audit trail is uniform.
//
// **Error envelope.**
//   - 400 bad_request — missing workspace or code, malformed body.
//   - 401 invalid_code — code didn't verify against either bucket.
//   - 404 not_found — workspace doesn't exist OR user isn't a member.
//     (Single 404 vocabulary so the endpoint doesn't leak which
//     workspaces the user knows about vs. is a member of.)
//   - 412 precondition_failed — claim secret not configured on this
//     deployment (shouldn't happen in production; self-host without
//     cloud-mode OAuth doesn't mount this route in the first place,
//     but the guard is defense in depth for cases where the route
//     mounts before the secret is wired).
//   - 500 internal_error — DB I/O failure.
func (s *Server) handleOAuthClaim(w http.ResponseWriter, r *http.Request) {
	// Refuse if the claim secret hasn't been wired. The route is
	// only mounted when the secret is set (see registerOAuthClaimRoute),
	// but checking again is cheap and prevents a stray request from
	// reaching DeriveClaimCode with a too-short secret.
	if len(s.claimSecret) < 16 {
		writeError(w, http.StatusPreconditionFailed, "claim_disabled",
			"Claim-code redemption is not enabled on this deployment.")
		return
	}

	user := currentUser(r)
	if user == nil {
		// /api/v1/* mounts RequireAuth, so this is defense in depth.
		writeError(w, http.StatusUnauthorized, "auth_required", "Authentication required.")
		return
	}

	var input struct {
		Workspace string `json:"workspace"`
		Code      string `json:"code"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	input.Workspace = strings.TrimSpace(input.Workspace)
	input.Code = strings.TrimSpace(input.Code)
	if input.Workspace == "" || input.Code == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"Both workspace (slug) and code (6 digits) are required.")
		return
	}

	ws, err := s.store.GetWorkspaceBySlug(input.Workspace)
	if err != nil || ws == nil {
		// Don't distinguish "doesn't exist" from "you're not a
		// member" — 404 uniform so the endpoint can't be used to
		// probe workspace existence.
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found.")
		return
	}
	// Verify the requesting user is actually a member of the workspace
	// before the code is even checked. Non-member redemption would
	// grant access to a workspace the user doesn't belong to — a
	// privilege escalation. The same 404 envelope ensures non-members
	// can't probe.
	member, err := s.store.GetWorkspaceMember(ws.ID, user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if member == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found.")
		return
	}

	// Verify the code. VerifyClaimCode checks the current AND previous
	// 5-min bucket (sliding 5–10 min lifetime per IDEA-1517 §4) and
	// constant-time compares to prevent timing oracles from leaking
	// which bucket matched.
	if !VerifyClaimCode(s.claimSecret, user.ID, ws.ID, input.Code, time.Now()) {
		writeError(w, http.StatusUnauthorized, "invalid_code",
			"Claim code is invalid or expired. Generate a fresh code in the workspace's Connect modal.")
		return
	}

	// Resolve the calling connection. PAT auth has no request_id and
	// the side effect simply doesn't apply — return 200 with a hint so
	// the caller knows the code was good but the grant wasn't an OAuth
	// connection. Matches the IDEA's "CLI / skill users do NOT need
	// this guidance — they have full workspace access via session
	// tokens already" — but lets a confused PAT caller see a clear
	// outcome rather than a silent success.
	kind, requestID := MCPTokenIdentityFromContext(r.Context())
	if kind != "oauth" || requestID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"workspace":     ws.Slug,
			"workspace_id":  ws.ID,
			"already_added": false,
			"note": "Code verified, but this caller is not an OAuth connection " +
				"(PAT or CLI session token). Allow-list state lives per-OAuth-grant; " +
				"PATs and CLI session tokens already see every workspace you belong to.",
		})
		return
	}

	// Was it already in the allow-list? Idempotent claim — re-claiming
	// a workspace already covered is a no-op (still returns 200, but
	// reports already_added so the agent can phrase the user-facing
	// reply accordingly).
	already, err := s.store.IsConnectionWorkspaceAllowed(requestID, ws.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	if !already {
		// Pre-check the parent oauth_connections row before attempting
		// the join-table insert. The FK constraint in
		// oauth_connection_workspaces would otherwise reject the INSERT
		// with a wrapped driver error, which is awkward to translate
		// into a clean 412 here. Phase A's tables stay empty until
		// Phase C wires the consent-screen write path, so the "no
		// connection row" case is expected on Phase-B-only deployments
		// — return 412 with a hint to re-authorize instead of 500.
		if _, err := s.store.GetOAuthConnection(requestID); err != nil {
			if errors.Is(err, store.ErrOAuthConnectionNotFound) {
				writeError(w, http.StatusPreconditionFailed, "connection_not_persisted",
					"This OAuth grant predates per-connection storage; re-authorize to enable claim-code redemption.")
				return
			}
			writeInternalError(w, err)
			return
		}
		if err := s.store.AddConnectionWorkspace(requestID, ws.ID, store.AddedByClaim); err != nil {
			writeInternalError(w, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"workspace":     ws.Slug,
		"workspace_id":  ws.ID,
		"already_added": already,
	})
}
