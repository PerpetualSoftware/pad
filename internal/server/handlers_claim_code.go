package server

import (
	"net/http"
	"time"
)

// GET /api/v1/workspaces/{slug}/claim-code — claim-code generation +
// smart-suppression endpoint (PLAN-1519 / TASK-1525 / IDEA-1517 §4).
//
// The "Connect a project" modal in the web UI calls this to render
// either:
//
//   - A 6-digit claim code the user reads to their agent, which then
//     calls `pad_workspace.action: claim` to add this workspace to
//     its OAuth grant's allow-list (handled by handleOAuthClaim).
//
//   - A "your agent can already see this workspace" hint when smart
//     suppression detects the workspace is already covered by one of
//     the user's active OAuth connections — IDEA-1517 §4: "if the
//     user has any active grant they personally own with
//     include_future_workspaces=on, the modal detects the workspace
//     is already auto-covered and replaces the code with…"
//
// We broaden the suppression predicate slightly from the strict IDEA
// reading to cover any active-and-covering grant — wildcard
// (all_current_workspaces=1) and explicit allow-list entries alike —
// because the user-facing answer to "would my existing agent already
// see this workspace today?" is identical regardless of which flag
// got it onto the allow-list. include_future_workspaces is the most
// common path but not the only one.
//
// **Why a workspace-scoped GET.** The route mounts under
// `/api/v1/workspaces/{slug}` so it inherits RequireWorkspaceAccess —
// the same membership gate the rest of the workspace-scoped surface
// uses. The handler then re-derives the code for (current user,
// workspace) so a viewer/editor/owner can each pull a fresh code
// for any workspace they belong to. Membership IS the consent —
// IDEA-1517 §4: "the user generating + handing over the code IS the
// consent."
//
// **Idempotency / freshness.** DeriveClaimCode is stateless: the
// same (user, workspace, 5-min bucket) always returns the same six
// digits. A page that polls this endpoint will see the digits roll
// over every 5 minutes. `expires_at` reports the END of the CURRENT
// bucket so the client UI can render a countdown; the verification
// path accepts the previous bucket too, so a code is usable for a
// sliding 5-10 minute window.
//
// **Error envelope.**
//   - 412 claim_disabled — deployment hasn't wired the claim secret
//     (self-host without cloud-mode OAuth). Endpoint exists but
//     can't produce a redeemable code.
//   - 401 auth_required — defense in depth (route is RequireAuth).
//   - 404 — RequireWorkspaceAccess handles non-members.
//   - 500 internal_error — DB I/O failure on the coverage query.
func (s *Server) handleWorkspaceClaimCode(w http.ResponseWriter, r *http.Request) {
	if len(s.claimSecret) < 16 {
		writeError(w, http.StatusPreconditionFailed, "claim_disabled",
			"Claim-code redemption is not enabled on this deployment.")
		return
	}

	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "Authentication required.")
		return
	}

	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}

	// Smart suppression: does the calling user already have an active
	// OAuth connection that covers this workspace? If so, the modal
	// has no claim code to offer — it points the user at
	// /console/connected-apps instead. Failure here is non-fatal for
	// the page (we'd still want to render SOMETHING rather than 500
	// over a non-critical hint) but we surface a 500 so the bug is
	// visible — silent suppression failures would be worse than a
	// loud one.
	connName, covered, err := s.store.IsWorkspaceCoveredForUser(user.ID, ws.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	now := time.Now()
	resp := map[string]any{
		"workspace":  ws.Slug,
		"suppressed": covered,
		// expires_at reports the end of the CURRENT 5-min bucket in
		// UTC RFC3339. Verification still accepts the previous bucket
		// for up to ~5 additional minutes (sliding window), so this
		// is the conservative "fresh through" timestamp the UI should
		// use to drive a countdown without over-promising lifetime.
		"expires_at": bucketEndTime(now).UTC().Format(time.RFC3339),
	}

	if covered {
		// Hand the connection name back so the UI can render
		// "your agent '<name>' can already see this workspace —
		// go to Connected apps." Empty string is fine (the
		// connection may not have been named at /authorize); the
		// UI falls back to a generic phrasing in that case.
		resp["suppression_grant_name"] = connName
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp["code"] = DeriveClaimCode(s.claimSecret, user.ID, ws.ID, now)
	writeJSON(w, http.StatusOK, resp)
}

// bucketEndTime returns the Unix timestamp at which the CURRENT
// 5-minute claim-code bucket rolls over to the next one. Used as
// the `expires_at` value in the generation response so the modal
// can show a countdown without re-implementing the bucket math.
//
// The claim verifier accepts the PREVIOUS bucket too, so a code is
// actually usable for ~5 minutes past this timestamp — but the
// "guaranteed fresh" promise ends here, and the UI shouldn't
// over-promise lifetime.
func bucketEndTime(at time.Time) time.Time {
	bucket := at.UTC().Unix() / claimBucketSeconds
	return time.Unix((bucket+1)*claimBucketSeconds, 0).UTC()
}
