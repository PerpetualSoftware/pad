package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// handleListMembers returns all members of a workspace.
// Requires at least viewer role (guests are blocked).
// Invitation details are only included for workspace owners.
func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "viewer") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	members, err := s.store.ListWorkspaceMembers(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	type invWithURL struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		Role      string `json:"role"`
		Code      string `json:"code"`
		JoinURL   string `json:"join_url,omitempty"`
		CreatedAt string `json:"created_at"`
	}

	// Only owners can see pending invitations (which contain emails and join codes)
	var enrichedInvs []invWithURL
	if requireRole(r, "owner") {
		invitations, err := s.store.ListWorkspaceInvitations(workspaceID)
		if err != nil {
			writeInternalError(w, err)
			return
		}

		enrichedInvs = make([]invWithURL, len(invitations))
		for i, inv := range invitations {
			// For hashed invitations (code == id placeholder), the plaintext
			// code is not recoverable — only show code/join_url for legacy invites.
			code := inv.Code
			if code == inv.ID {
				code = ""
			}
			enrichedInvs[i] = invWithURL{
				ID:        inv.ID,
				Email:     inv.Email,
				Role:      inv.Role,
				Code:      code,
				CreatedAt: inv.CreatedAt.Format("2006-01-02T15:04:05Z"),
			}
			if s.baseURL != "" && code != "" {
				enrichedInvs[i].JoinURL = s.baseURL + "/join/" + code
			}
		}
	}

	if enrichedInvs == nil {
		enrichedInvs = []invWithURL{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"members":     members,
		"invitations": enrichedInvs,
	})
}

// handleInviteMember creates an invitation or directly adds a user if they exist.
func (s *Server) handleInviteMember(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Only owners can invite
	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can invite members")
		return
	}

	var input struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Email == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "email is required")
		return
	}
	if input.Role == "" {
		input.Role = "editor"
	}

	// Enforce member count limit (workspace-scoped)
	if !s.enforcePlanLimit(w, workspaceID, "members_per_workspace") {
		return
	}

	inviterID := currentUserID(r)

	// Check if user with this email already exists
	existingUser, err := s.store.GetUserByEmail(input.Email)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	if existingUser != nil {
		// User exists — add them directly
		alreadyMember, _ := s.store.IsWorkspaceMember(workspaceID, existingUser.ID)
		if alreadyMember {
			writeError(w, http.StatusConflict, "conflict", "User is already a member of this workspace")
			return
		}
		if err := s.store.AddWorkspaceMember(workspaceID, existingUser.ID, input.Role); err != nil {
			writeInternalError(w, err)
			return
		}
		s.logWorkspaceAuditEvent(workspaceID, models.ActionMemberInvited, r, auditMeta(map[string]string{"email": existingUser.Email, "role": input.Role, "added_directly": "true"}))
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"added":   true,
			"user_id": existingUser.ID,
			"email":   existingUser.Email,
			"name":    existingUser.Name,
			"role":    input.Role,
		})
		return
	}

	// User doesn't exist — create an invitation
	inv, err := s.store.CreateInvitation(workspaceID, input.Email, input.Role, inviterID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	resp := map[string]interface{}{
		"invited": true,
		"code":    inv.Code,
		"email":   inv.Email,
		"role":    inv.Role,
	}
	joinURL := ""
	if s.baseURL != "" {
		joinURL = s.baseURL + "/join/" + inv.Code
		resp["join_url"] = joinURL
	}

	s.logWorkspaceAuditEvent(workspaceID, models.ActionMemberInvited, r, auditMeta(map[string]string{"email": input.Email, "role": input.Role}))

	writeJSON(w, http.StatusCreated, resp)

	// Send invitation email asynchronously (fire-and-forget; tracked via
	// s.goAsync so test cleanup / shutdown can drain it — BUG-842).
	if s.email != nil && joinURL != "" {
		s.goAsync(func() {
			// Check if the recipient has opted out of emails
			if optedOut, err := s.store.IsEmailOptedOut(inv.Email); err == nil && optedOut {
				slog.Info("skipping invitation email: recipient opted out", "email", inv.Email)
				return
			}
			inviterName := "A team member"
			wsName := "a workspace"
			if user, err := s.store.GetUser(inviterID); err == nil && user != nil {
				inviterName = user.Name
			}
			if ws, err := s.store.GetWorkspaceByID(workspaceID); err == nil && ws != nil {
				wsName = ws.Name
			}
			unsubURL := email.UnsubscribeURL(s.baseURL, inv.Email, s.unsubscribeSecret())
			if err := s.email.SendInvitation(context.Background(), inv.Email, inviterName, wsName, joinURL, unsubURL); err != nil {
				slog.Error("failed to send invitation email", "error", err)
			}
		})
	}
}

// handleRemoveMember removes a user from a workspace.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can remove members")
		return
	}

	userID := chi.URLParam(r, "userID")

	// Prevent removing yourself
	if userID == currentUserID(r) {
		writeError(w, http.StatusBadRequest, "bad_request", "Cannot remove yourself from the workspace")
		return
	}

	// Default to revoking grants on member removal to prevent the removed user
	// from silently becoming a guest with continued access.
	// ?revoke_grants=false → explicitly keep grants (user becomes a guest)
	// ?revoke_grants=true or omitted → delete all grants (full removal)
	revokeGrants := r.URL.Query().Get("revoke_grants") != "false"
	if revokeGrants {
		// Atomic: remove member and revoke all grants in one transaction
		if err := s.store.RemoveWorkspaceMemberAndRevokeGrants(workspaceID, userID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "Member not found")
			return
		}
	} else {
		if err := s.store.RemoveWorkspaceMember(workspaceID, userID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "Member not found")
			return
		}
	}

	meta := map[string]string{"user_id": userID}
	if revokeGrants {
		meta["revoked_grants"] = "true"
	}
	s.logWorkspaceAuditEvent(workspaceID, models.ActionMemberRemoved, r, auditMeta(meta))

	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateMemberRole changes a member's role in a workspace.
func (s *Server) handleUpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can change roles")
		return
	}

	userID := chi.URLParam(r, "userID")

	var input struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Role == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "role is required")
		return
	}

	if err := s.store.UpdateWorkspaceMemberRole(workspaceID, userID, input.Role); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Member not found")
		return
	}

	s.logWorkspaceAuditEvent(workspaceID, models.ActionRoleChanged, r, auditMeta(map[string]string{"user_id": userID, "role": input.Role}))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_id": userID,
		"role":    input.Role,
	})
}

// handleCancelInvitation deletes a pending invitation.
func (s *Server) handleCancelInvitation(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can cancel invitations")
		return
	}

	invID := chi.URLParam(r, "invID")
	if err := s.store.DeleteInvitation(workspaceID, invID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Invitation not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAcceptInvitation accepts a workspace invitation by code.
func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	inv, err := s.store.GetInvitationByCode(code)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "not_found", "Invitation not found or already accepted")
		return
	}
	if inv.IsExpired() {
		writeError(w, http.StatusGone, "expired", "This invitation has expired. Ask the inviter to send a new one.")
		return
	}

	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "You must be logged in to accept an invitation")
		return
	}
	// Bind the invitation to the invitee's email: the signed-in account must
	// match the address on the invite. Otherwise anyone who learns the code
	// (forwarded email, leaked screenshot, guessed URL) could claim the seat
	// from a different account.
	if !strings.EqualFold(strings.TrimSpace(user.Email), inv.Email) {
		writeError(w, http.StatusForbidden, "invitation_email_mismatch",
			"This invitation was sent to a different email address. Sign in with that account to accept.")
		return
	}

	// Add user to workspace
	if err := s.store.AddWorkspaceMember(inv.WorkspaceID, user.ID, inv.Role); err != nil {
		writeInternalError(w, err)
		return
	}

	// Mark invitation as accepted
	if err := s.store.AcceptInvitation(inv.ID); err != nil {
		writeInternalError(w, err)
		return
	}

	// DR-1: accepting an email-bound invitation proves control of that email
	// address, so verify the account if it's still unverified. This makes
	// "invited = verified" hold for later accepts too — an existing unverified
	// self-signup who accepts an invite becomes verified. Because currentUser
	// is re-read fresh from the DB per request (ValidateSession → GetUser),
	// this flip unblocks the user's subsequent mutations under
	// RequireVerifiedEmail immediately, in the same session. Best-effort: a
	// failure here doesn't undo the accepted membership.
	if !user.IsEmailVerified() {
		if err := s.store.SetUserEmailVerified(user.ID); err != nil {
			slog.Error("failed to mark email verified on invite accept", "error", err, "user_id", user.ID)
		} else {
			s.logAuditEventForUser(models.ActionEmailVerified, r, user.ID, auditMeta(map[string]string{
				"email":  user.Email,
				"method": "invitation_accept",
			}))
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"accepted":     true,
		"workspace_id": inv.WorkspaceID,
		"role":         inv.Role,
	})
}

// handlePreviewInvitation returns non-consuming metadata about an invitation
// so the /join page can prefill the invited email (read-only) and pick
// register-vs-login mode. Unlike handleAcceptInvitation it never mutates
// state — no membership add, no accepted_at write.
//
// Enumeration safety: this endpoint is public (pre-auth) and reveals whether
// a code maps to a live invitation, so it is engineered against invite-code
// enumeration on two fronts:
//   - Always HTTP 200. Invalid, expired, missing, or dangling-workspace codes
//     all return the SAME {"found": false} shape — no 404/410/403 status
//     signal to distinguish "wrong code" from "valid code" (matches the
//     always-200 posture of the auth reset/verify endpoints). A genuine DB
//     fault still 500s, but that outcome is code-independent (identical for
//     every code) so it leaks nothing about which codes are valid.
//   - Rate limited. The route is wired to a dedicated per-IP limiter in
//     middleware_ratelimit.go so an attacker can't grind the code space.
//     (Codes are 128-bit random anyway — see CreateInvitation — so brute
//     force is already infeasible; the limiter is defense in depth.)
func (s *Server) handlePreviewInvitation(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	notFound := func() {
		writeJSON(w, http.StatusOK, map[string]interface{}{"found": false})
	}

	inv, err := s.store.GetInvitationByCode(code)
	if err != nil {
		// Genuine backend fault (DB down, etc.) — not a code-validity signal.
		writeInternalError(w, err)
		return
	}
	// Treat missing and expired invitations identically to an unknown code so
	// the response can't be used to probe which codes were ever real.
	if inv == nil || inv.IsExpired() {
		notFound()
		return
	}

	// A live invitation to a since-deleted workspace has nothing to join.
	ws, err := s.store.GetWorkspaceByID(inv.WorkspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		notFound()
		return
	}

	// has_account: does an account already exist for the invited address?
	// Lets the client default to login instead of register. GetUserByEmail
	// returns (nil, nil) when no such user exists.
	existingUser, err := s.store.GetUserByEmail(inv.Email)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"found":          true,
		"email":          inv.Email,
		"workspace_name": ws.Name,
		"has_account":    existingUser != nil,
	})
}

// handleGetMemberCollectionAccess returns a member's collection access mode and granted IDs.
func (s *Server) handleGetMemberCollectionAccess(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Only workspace owners or the user themselves can view collection access
	userID := chi.URLParam(r, "userID")
	if !requireRole(r, "owner") && currentUserID(r) != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can view other members' collection access")
		return
	}
	member, err := s.store.GetWorkspaceMember(workspaceID, userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if member == nil {
		writeError(w, http.StatusNotFound, "not_found", "Member not found")
		return
	}

	grants, err := s.store.GetMemberCollectionAccess(workspaceID, userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"collection_access": member.CollectionAccess,
		"collection_ids":    grants,
	})
}

// handleSetMemberCollectionAccess updates a member's collection visibility.
// Only workspace owners can change other members' access.
func (s *Server) handleSetMemberCollectionAccess(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can manage collection access")
		return
	}

	userID := chi.URLParam(r, "userID")
	member, err := s.store.GetWorkspaceMember(workspaceID, userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if member == nil {
		writeError(w, http.StatusNotFound, "not_found", "Member not found")
		return
	}

	var input struct {
		Mode          string   `json:"mode"`           // "all" or "specific"
		CollectionIDs []string `json:"collection_ids"` // only for "specific"
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Mode != "all" && input.Mode != "specific" {
		writeError(w, http.StatusBadRequest, "validation_error", "Mode must be 'all' or 'specific'")
		return
	}

	if !s.requireCallerCanSetCollectionAccess(w, r, workspaceID, input.Mode, input.CollectionIDs) {
		return
	}

	if err := s.store.SetMemberCollectionAccess(workspaceID, userID, input.Mode, input.CollectionIDs); err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"collection_access": input.Mode,
		"collection_ids":    input.CollectionIDs,
	})
}

// requireCallerCanSetCollectionAccess denies a restricted caller (their own
// collection_access is "specific") from using this endpoint to escalate
// beyond their own visibility (BUG-1925). requireRole(r, "owner") above only
// checks WORKSPACE ROLE; member_collection_access has no role exclusion
// (BUG-1920), so a workspace-role "owner" can independently be restricted,
// and without this guard they could PATCH themselves (or another member,
// including one they can't see) to mode="all" or into a hidden collection,
// bypassing the entire BUG-1917/1918/1920/1921 family plus the BUG-1922
// export gate in one authenticated request. Unrestricted callers (mode
// "all", or admin) are untouched — visibleCollectionIDs returns nil for
// them and this is a no-op.
//
// A restricted caller may never grant mode="all" to ANY target (self or
// another member) — that would hand out unrestricted access this caller
// doesn't have themselves. For mode="specific", every requested collection
// ID must be within the caller's own visible set, checked against
// isCollectionVisible's semantics.
//
// The membership check narrows to guestResourceFilter's fullCollIDs
// (the strict, full-access-only set) rather than the raw nav-lenient
// visibleCollectionIDs whenever the caller holds any item-level grants —
// mirroring requireCollectionFullyVisible (BUG-1920 codex R2). Without
// this narrowing, a restricted caller with only an item-level grant into a
// collection could designate that collection as a mode="specific" target,
// minting themselves (or another member) full collection-wide
// member_collection_access from a single-item grant.
//
// Hidden-collection-ID failures return 404 (not 403), matching the rest of
// the visibility family's hidden-resource convention (e.g. share-links) so
// the response doesn't confirm the collection's existence. The mode="all"
// rejection returns 403 since there's no resource identity to hide.
func (s *Server) requireCallerCanSetCollectionAccess(w http.ResponseWriter, r *http.Request, workspaceID, mode string, collectionIDs []string) bool {
	callerVisibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if callerVisibleIDs == nil {
		// Unrestricted caller (mode "all", or admin) — full power, no change.
		return true
	}

	if mode == "all" {
		writeError(w, http.StatusForbidden, "forbidden", "Restricted members cannot grant unrestricted collection access")
		return false
	}

	checkIDs := callerVisibleIDs
	fullCollIDs, grantedItemIDs, gErr := s.guestResourceFilter(r, workspaceID)
	if gErr != nil {
		writeInternalError(w, gErr)
		return false
	}
	if len(grantedItemIDs) > 0 {
		checkIDs = fullCollIDs
	}

	for _, id := range collectionIDs {
		if !isCollectionVisible(id, checkIDs) {
			writeError(w, http.StatusNotFound, "not_found", "Collection not found")
			return false
		}
	}
	return true
}
