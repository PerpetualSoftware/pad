package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// handleListMyInvitations answers GET /api/v1/me/invitations (PLAN-3002 U4b /
// TASK-3277): the pending, unexpired invitations addressed to the caller's
// email, to live workspaces the caller is not already a member of.
//
// Only a VERIFIED email is matched. An account's email is just a string it
// typed at signup until it is verified, so listing by an unverified address
// would show whoever registered with someone else's address that person's
// invitations. Every self-hosted account is verified (migration 070's
// backfill and CreateUser's default); the only unverified accounts are Pad
// Cloud self-serve signups that have not confirmed yet, so this filter is the
// one that matters on cloud. An unverified caller gets an empty list with
// email_verified false, never an error, so the web client can say why.
func (s *Server) handleListMyInvitations(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	if !user.IsEmailVerified() {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"invitations":    []store.MyInvitation{},
			"email_verified": false,
		})
		return
	}
	invs, err := s.store.ListPendingInvitationsForEmail(user.Email, user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"invitations":    invs,
		"email_verified": true,
	})
}

// handleAcceptMyInvitation answers POST /api/v1/me/invitations/{id}/accept:
// accepting an invitation from the list above, which cannot carry the join
// code because only the code's hash is stored.
//
// It shares acceptInvitationCore with the by-code door, so membership, role,
// the access-gained publish and accepted_at are the same by construction.
// What differs is the admission, and one deliberate omission:
//   - An id that is not a pending invitation addressed to the caller answers
//     404, identical to an unknown id, so ids cannot be probed.
//   - The caller must ALREADY be verified. The by-code door verifies an
//     unverified account on accept, because holding the emailed code proves
//     the address; an id read from this list proves nothing, so this door
//     never sets email_verified_at.
func (s *Server) handleAcceptMyInvitation(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	// A bot is no signed-in person (TASK-3392).
	if user == nil || user.IsApp() {
		writeError(w, http.StatusUnauthorized, "unauthorized", "You must be logged in to accept an invitation")
		return
	}
	if !user.IsEmailVerified() {
		writeError(w, http.StatusForbidden, "email_not_verified",
			"Verify your email address to accept invitations from this list, or use the link in the invitation email.")
		return
	}
	inv, err := s.store.GetPendingInvitationForEmail(chi.URLParam(r, "id"), user.Email)
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
	role, ok := s.acceptInvitationCore(w, r, inv, user)
	if !ok {
		return
	}
	s.writeInvitationAccepted(w, inv, role)
}

// handleDeclineMyInvitation answers POST /api/v1/me/invitations/{id}/decline
// (BUG-2136): the invitee says no from the in-app list. Same admission as the
// accept beside it: a signed-in person with a VERIFIED email, and an id that is
// not a pending invitation addressed to them answers 404 identical to an
// unknown id. Declining deletes the invitation, as an owner's cancel does, so
// every pending list drops it and its code stops working; the audit event is
// its trail. An expired invitation may be declined too, which only tidies it.
func (s *Server) handleDeclineMyInvitation(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.IsApp() {
		writeError(w, http.StatusUnauthorized, "unauthorized", "You must be logged in to decline an invitation")
		return
	}
	if !user.IsEmailVerified() {
		writeError(w, http.StatusForbidden, "email_not_verified",
			"Verify your email address to decline invitations from this list, or use the link in the invitation email.")
		return
	}
	inv, err := s.store.GetPendingInvitationForEmail(chi.URLParam(r, "id"), user.Email)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "not_found", "Invitation not found or already accepted")
		return
	}
	s.declineInvitation(w, r, inv)
}
