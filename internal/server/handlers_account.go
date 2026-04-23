package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/xarmian/pad/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// --- Account Deletion (GDPR Article 17 — Right to Erasure) ---

// handleDeleteAccount handles POST /api/v1/auth/delete-account.
// Requires password confirmation. Deletes the user and all owned data.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		user = s.validateSessionCookie(r)
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	var input struct {
		Password string `json:"password"`
		Confirm  bool   `json:"confirm"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Verify identity
	fullUser, err := s.store.GetUser(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	if input.Password != "" {
		// Password confirmation (normal flow)
		if err := bcrypt.CompareHashAndPassword([]byte(fullUser.PasswordHash), []byte(input.Password)); err != nil {
			writeError(w, http.StatusForbidden, "forbidden", "Incorrect password")
			return
		}
	} else if input.Confirm && s.cloudMode {
		// Cloud mode only: allow confirm-only deletion for OAuth-registered users
		// who never set a password. The session itself is the proof of identity.
		// In self-hosted mode, password is always required to prevent accidental
		// or coerced account deletion.
	} else {
		writeError(w, http.StatusBadRequest, "bad_request", "Password is required to delete your account")
		return
	}

	// Delete all owned workspaces, sessions, and the user atomically.
	// If any workspace deletion fails, the entire operation is aborted.
	workspaces, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	var ownedSlugs []string
	for _, ws := range workspaces {
		if ws.OwnerID == user.ID {
			ownedSlugs = append(ownedSlugs, ws.Slug)
		}
	}

	// Cascade Stripe cancel BEFORE the local delete. If this ordering is
	// reversed, a mid-flight failure would wipe the user's StripeCustomerID
	// from our DB while leaving the Stripe subscription active — the user
	// would keep getting charged with no way for us to find and cancel
	// it. TASK-690 / PLAN-645.
	//
	// Skipped when:
	//   - the user has no Stripe customer (free plan, OAuth-only, never paid)
	//   - the sidecar isn't configured (self-hosted deploys with no billing)
	//
	// Failure strategy: any non-nil error aborts the delete with a 500.
	// pad-cloud normalizes Stripe 404/resource_missing to a 200 success on
	// its side (see pad-cloud stripe.go isStripeAlreadyGone), so every
	// non-2xx we see here is actually a real failure — 400/403 indicate
	// ops misconfig (bad secret, malformed call), 500 indicates upstream
	// breakage. In none of those cases is it correct to "log and
	// continue": doing so would wipe the user's StripeCustomerID while
	// leaving the Stripe subscription billing, which is the exact
	// regression this task exists to prevent.
	stripeWasCancelled := false
	if fullUser.StripeCustomerID != "" && s.cloudSidecar != nil {
		if err := s.cloudSidecar.CancelCustomer(fullUser.StripeCustomerID); err != nil {
			slog.Error("delete account: sidecar cancel-customer failed, aborting delete",
				"user_id", user.ID,
				"customer_id", fullUser.StripeCustomerID,
				"error", err)
			writeError(w, http.StatusInternalServerError, "billing_cancel_failed",
				"We couldn't cancel your subscription. Your account was NOT deleted and you have NOT been charged anything new. Please try again in a few minutes or contact support.")
			return
		}
		stripeWasCancelled = true
	}

	if err := s.store.DeleteAccountAtomic(user.ID, ownedSlugs); err != nil {
		// Cross-system danger zone: if Stripe was already cancelled, the
		// user's billing is gone but their account data is still present.
		// Stripe cancel is NOT reversible programmatically. Operator must
		// manually restore the Stripe subscription + customer for this
		// user, or hard-delete the local row. Log loudly so monitoring
		// catches it; the response message tells the user the truth.
		if stripeWasCancelled {
			slog.Error("delete account: STRIPE CANCELLED BUT LOCAL DELETE FAILED — manual operator intervention required",
				"user_id", user.ID,
				"email", user.Email,
				"stripe_customer_id", fullUser.StripeCustomerID,
				"error", err)
			writeError(w, http.StatusInternalServerError, "partial_delete",
				"Your billing was cancelled but we couldn't delete your account data. Please contact support — our team has been notified.")
			return
		}
		slog.Error("delete account: atomic deletion failed", "user_id", user.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Account deletion failed. No data was removed. Please try again or contact support.")
		return
	}

	// Clear session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(s.secureCookies),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	clearCSRFCookie(w)

	s.logAuditEventForUser(models.ActionAccountDeleted, r, user.ID, auditMeta(map[string]string{
		"email": user.Email,
	}))

	slog.Info("account deleted", "user_id", user.ID, "email", user.Email)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// --- Data Export (GDPR Article 20 — Right to Portability) ---

// handleExportAccount handles GET /api/v1/auth/export.
// Streams user data as JSON, processing one workspace at a time to avoid
// loading everything into memory. Enforces a 60-second timeout.
func (s *Server) handleExportAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		user = s.validateSessionCookie(r)
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	// Enforce a 60-second timeout for the entire export
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Prefetch workspace list (small) before starting the streaming response
	workspaces, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Stream the response — once we start writing, we can't send error status codes
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"pad-export.json\"")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)

	// Write opening structure
	w.Write([]byte("{\n  \"user\": "))
	enc.Encode(map[string]interface{}{
		"id":           user.ID,
		"email":        user.Email,
		"username":     user.Username,
		"name":         user.Name,
		"role":         user.Role,
		"plan":         user.Plan,
		"totp_enabled": user.TOTPEnabled,
		"created_at":   user.CreatedAt,
		"updated_at":   user.UpdatedAt,
	})

	w.Write([]byte(",\n  \"workspaces\": [\n"))

	for i, ws := range workspaces {
		// Check timeout between workspaces
		if ctx.Err() != nil {
			slog.Warn("export timeout", "user_id", user.ID, "workspaces_exported", i)
			break
		}

		if i > 0 {
			w.Write([]byte(",\n"))
		}

		wsData := map[string]interface{}{
			"id":         ws.ID,
			"name":       ws.Name,
			"slug":       ws.Slug,
			"role":       "owner",
			"created_at": ws.CreatedAt,
		}

		// Only export full data for owned workspaces
		if ws.OwnerID == user.ID {
			collections, _ := s.store.ListCollections(ws.ID)
			wsData["collections"] = collections

			// Stream items per workspace (each workspace loaded individually, then GC'd)
			items, err := s.store.ListItems(ws.ID, models.ItemListParams{IncludeArchived: true})
			if err != nil {
				slog.Error("export: failed to list items", "workspace", ws.Slug, "error", err)
				wsData["items"] = []interface{}{}
				wsData["export_error"] = "failed to export items"
			} else {
				wsData["items"] = items
			}
		}

		w.Write([]byte("    "))
		enc.Encode(wsData)

		// Flush after each workspace to free memory and show progress
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	w.Write([]byte("\n  ]\n}\n"))

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
