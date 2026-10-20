package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/billing"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
	"golang.org/x/crypto/bcrypt"
)

// --- Account Deletion (GDPR Article 17 — Right to Erasure) ---

// writeAccountDisabledIf answers 403 account_disabled when err is the store's
// refusal to mint a credential for a disabled account (BUG-3349): a request
// admitted before a disable, reaching its insert after it. Reports whether it
// wrote.
func writeAccountDisabledIf(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, store.ErrUserDisabled) {
		return false
	}
	writeError(w, http.StatusForbidden, "account_disabled", "Your account has been disabled. Contact an administrator.")
	return true
}

// accountDeleteReauthWindow is how recently the session must have been
// signed in for a confirm-only (passwordless) account deletion (BUG-3336).
// Without a password to check, a recent sign-in is the proof of identity: a
// stolen or long-lived session is not enough, and the real user signs in
// again. A credential-change rotation does not restart the clock
// (CreateSessionIssuedAt). A CLI-kind session never satisfies it (TASK-3975):
// a CLI token approved from a web session has its own created_at, so its age
// says nothing about when anyone signed in.
const accountDeleteReauthWindow = 10 * time.Minute

// cookieKindAccepted reports whether a session may be used as a browser
// cookie: only a web session (BUG-3350). A CLI session presented as a cookie
// is not a credential, so it cannot pick up the cookie-only platform-admin
// bypass a bearer is denied (BUG-1616).
func cookieKindAccepted(info *store.SessionInfo) bool {
	return info != nil && info.Kind != store.SessionKindCLI
}

// isCLIClient reports whether the request marked itself as the pad CLI
// (X-Pad-Client: cli), whose sign-ins mint CLI sessions (BUG-3350). The
// marker can only narrow what a session may do, so it needs no proof.
func isCLIClient(r *http.Request) bool {
	return r.Header.Get("X-Pad-Client") == "cli"
}

// requestSessionInfo is the session the request was authenticated with, or
// nil. It follows the credential the middleware recorded (ctxAuthKind) rather
// than re-reading the wire, the way credentialLiveness does: a bearer that did
// not authenticate (an invalid one falls through to the cookie on /auth/*) is
// not the session, and a valid PAT is no session at all. A request no
// middleware authenticated (handlers that fall back to validateSessionCookie)
// is read from the cookie.
func (s *Server) requestSessionInfo(r *http.Request) *store.SessionInfo {
	token, fromCookie := "", false
	switch authKind(r) {
	case authKindAPIToken:
		return nil
	case authKindSessionBearer:
		token = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	default:
		for _, name := range []string{sessionCookieName(s.secureCookies), "pad_session"} {
			if c, err := r.Cookie(name); err == nil && c.Value != "" {
				token, fromCookie = c.Value, true
				break
			}
		}
	}
	if token == "" {
		return nil
	}
	info, err := s.store.ValidateSession(token)
	if err != nil || info == nil || info.User == nil {
		return nil
	}
	if fromCookie && !cookieKindAccepted(info) {
		return nil
	}
	return info
}

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

	// An API token never deletes the account it belongs to (BUG-3336): the
	// delete is irreversible and takes every owned workspace with it, so a
	// leaked token must not be enough. A browser session or a `pad auth
	// login` CLI session is required, the line BUG-2890 drew for minting.
	// Checked before the body, so a token cannot probe the password either.
	if isAPITokenAuth(r) {
		writeError(w, http.StatusForbidden, "session_required",
			"Deleting your account requires an interactive session, not an API token")
		return
	}

	var input struct {
		Password string `json:"password"`
		Confirm  bool   `json:"confirm"`
		TOTPCode string `json:"totp_code"`
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
	} else if input.Confirm && s.cloudMode && !fullUser.HasPassword() {
		// The flag alone is not proof the account has no password: an account
		// that signed up with one before password_set existed, and later
		// linked OAuth, reads false too. So confirm-only also needs a recent
		// sign-in (codex review).
		info := s.requestSessionInfo(r)
		if info == nil || info.User.ID != fullUser.ID || info.Kind == store.SessionKindCLI || time.Since(info.CreatedAt) > accountDeleteReauthWindow {
			writeError(w, http.StatusForbidden, "reauth_required",
				"For your security, sign in again to delete your account")
			return
		}
		// Cloud mode only: allow confirm-only deletion for OAuth-registered users
		// who never set a password. The session itself is the proof of identity.
		// An account WITH a password gives it (BUG-3336): confirm used to be
		// accepted from any cloud account, so a session alone could delete it.
		// In self-hosted mode, password is always required to prevent accidental
		// or coerced account deletion.
	} else {
		writeError(w, http.StatusBadRequest, "bad_request", "Password is required to delete your account")
		return
	}

	// Second factor: an irreversible delete must clear at least the same bar
	// as login. If the account has 2FA enabled, require and verify a current
	// TOTP code — run AFTER the password/confirm identity check above so
	// NEITHER path can skip it. This is the whole point: cloud confirm-only
	// needs no password, so a hijacked live session would otherwise be enough
	// to wipe a 2FA-protected account. Placed before the Stripe cancel below
	// so a failed code never leaks a cancel RPC.
	//
	// BUG-3482: the code goes through login's own check (checkSecondFactor):
	// the RecoveryCode limiter, six tries an hour counted per user, and the
	// single-use step claim. It used to be a bare totp.Validate with neither,
	// so a hijacked session inside the reauth window could grind the six
	// digits against this door. The per-user key is the one the 2FA-disable
	// door uses, so the two share one budget. Only a TOTP code is accepted
	// here, as before.
	if fullUser.TOTPEnabled {
		code := strings.TrimSpace(input.TOTPCode)
		if code == "" {
			writeError(w, http.StatusBadRequest, "totp_required", "A 2FA code is required to delete your account")
			return
		}
		verified, answered := s.checkSecondFactor(w, fullUser, "user:"+fullUser.ID, code, "")
		if answered {
			return
		}
		if !verified {
			time.Sleep(500 * time.Millisecond)
			writeError(w, http.StatusUnauthorized, "totp_invalid", "Invalid 2FA code")
			return
		}
	}

	// The sole active admin cannot delete their account (BUG-3811). Asked
	// here, before the Stripe cancel below, which cannot be undone; the
	// deletion's transaction asks again under the lock.
	if err := s.store.CheckActiveAdminLoss(user.ID); err != nil {
		if errors.Is(err, store.ErrLastAdmin) {
			writeLastActiveAdmin(w)
			return
		}
		writeInternalError(w, err)
		return
	}

	// Owned workspaces, sessions and the user are deleted atomically below.
	// The owned set is read INSIDE that transaction, under the user-row lock
	// (BUG-3099): read here, before the Stripe cancel, a workspace minted in
	// between survived as a live workspace of a deleted user.

	// Cascade Stripe cancel BEFORE the local delete. If this ordering is
	// reversed, a mid-flight failure would wipe the user's StripeCustomerID
	// from our DB while leaving the Stripe subscription active — the user
	// would keep getting charged with no way for us to find and cancel
	// it. TASK-690 / PLAN-645.
	//
	// Skipped when the sidecar isn't configured (self-hosted deploys with no
	// billing).
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
	//
	// BUG-3560: the sidecar is asked even when no customer is linked, with
	// the user id, because a user can own a Stripe customer stamped with
	// their id and not linked here (TASK-3549 keeps one link and unions the
	// rest); it cancels every customer the user owns. A sidecar that predates
	// user_id refuses an empty customer_id with 400: it could not have
	// cancelled anything, which is what not asking did before, so that one
	// answer lets the deletion proceed. Only that answer: the old sidecar's
	// exact refusal (olderSidecarRefusedEmptyCustomer). Any other 400 from a
	// current sidecar may come with stamped customers still billing, so it
	// aborts like every other failure (retro Codex review of #2036).
	stripeWasCancelled := false
	if s.cloudSidecar != nil {
		err := s.cloudSidecar.CancelCustomer(fullUser.StripeCustomerID, fullUser.ID)
		var sidecarErr *billing.SidecarError
		switch {
		case err == nil:
			// Possibly only a stamped, unlinked customer: still billing that
			// was cancelled, which the partial-delete alert below must know.
			stripeWasCancelled = true
		case fullUser.StripeCustomerID == "" && errors.As(err, &sidecarErr) && olderSidecarRefusedEmptyCustomer(sidecarErr):
			slog.Warn("delete account: sidecar predates user_id; no linked customer to cancel, continuing",
				"user_id", user.ID)
		default:
			slog.Error("delete account: sidecar cancel-customer failed, aborting delete",
				"user_id", user.ID,
				"customer_id", fullUser.StripeCustomerID,
				"error", err)
			writeError(w, http.StatusInternalServerError, "billing_cancel_failed",
				"We couldn't cancel your subscription. Your account was NOT deleted and you have NOT been charged anything new. Please try again in a few minutes or contact support.")
			return
		}
	}

	// TASK-3272: the deletion soft-deletes every live workspace this user
	// owns. Read who reaches each one BEFORE it happens, so they can be told.
	// A failed read costs only the hint, never the deletion.
	ownedAccess := map[string][]string{}
	if owned, err := s.store.ListOwnedLiveWorkspaceIDs(user.ID); err != nil {
		slog.Warn("delete account: could not read owned workspaces; no workspace_access_changed will be sent",
			"user_id", user.ID, "error", err)
	} else {
		for _, wsID := range owned {
			ownedAccess[wsID] = s.workspaceAccessUsers(wsID)
		}
	}

	// TASK-3365: the deletion also removes the grants this user ISSUED,
	// possibly in workspaces someone else owns. Their workspaces are reported
	// from the rows the transaction deleted, so a grant created after any
	// earlier read cannot be missed (codex r2 on PR2), and kicked below.
	issuedGrantWorkspaces, err := s.store.DeleteAccountAtomicReport(user.ID)
	if errors.Is(err, store.ErrLastAdmin) && !stripeWasCancelled {
		writeLastActiveAdmin(w)
		return
	}
	if err != nil {
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
	// TASK-3365: the account is gone, so its live connections close now.
	s.invalidateUserAccess(user.ID)
	for _, wsID := range issuedGrantWorkspaces {
		s.invalidateWorkspaceAccess(wsID)
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

	// Log the deletion with an EMPTY user_id: the user row is already gone,
	// and activities.user_id is now an ON DELETE SET NULL FK (migrations
	// 072/050), so an insert referencing the deleted id would fail the FK and
	// CreateActivity's error is swallowed — the audit row would vanish. Pass
	// "" (stored as NULL) and keep the deleted identity in metadata so the
	// account_deleted event is actually recorded on both dialects.
	s.logAuditEventForUser(models.ActionAccountDeleted, r, "", auditMeta(map[string]string{
		"deleted_user_id": user.ID,
		"email":           user.Email,
	}))

	for wsID, userIDs := range ownedAccess {
		s.publishWorkspaceAccessChangedFromRequest(r, wsID, watchevents.AccessDeleted, userIDs...)
	}

	slog.Info("account deleted", "user_id", user.ID, "email", user.Email)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// --- Data Export (GDPR Article 20 — Right to Portability) ---

// accountExportTimeout bounds one account export.
const accountExportTimeout = 60 * time.Second

// accountExportFormat and accountExportVersion open every account export, so a
// consumer can tell this artifact from a workspace bundle's pad-export.json
// and from a later shape (BUG-3626).
const (
	accountExportFormat  = "pad-account-export"
	accountExportVersion = 1
)

// accountExportHooks lets a test change the export's deadline, make one of
// its store reads fail or stall at exactly that point (a broken table fails an
// earlier query first and answers 500 before any byte, which is not the case
// under test), or act in a gap between reads. Zero values mean the real thing
// (BUG-3626, BUG-3659). A read hook replaces the snapshot read it stands for.
type accountExportHooks struct {
	timeout         time.Duration
	listCollections func(wsID string) ([]models.Collection, error)
	listItems       func(wsID string, p models.ItemListParams) ([]models.Item, error)
	// BUG-3659: the reads that ran outside the deadline.
	listWorkspaces  func(userID string) ([]models.Workspace, error)
	visibleColls    func(r *http.Request, wsID string) ([]string, error)
	itemAttribution func(itemIDs []string) (map[string]store.AppAttribution, error)
	// afterPreflight runs after the access preflight, before the body.
	afterPreflight func()
	// afterSnapshotCollections runs inside a workspace's snapshot, between
	// its collection and item reads.
	afterSnapshotCollections func(wsID string)
}

// errAccountExportRestricted is the snapshot's access re-check failing: the
// caller became restricted in the workspace after the export's preflight.
var errAccountExportRestricted = errors.New("workspace access became restricted during the export")

// accountExportWorkspace reads one owned workspace's collections, items and
// their app attribution from ONE read-only snapshot, after re-checking on that
// same snapshot that the caller's access is unrestricted (BUG-3659). Read
// apart, an item created between the collection and item reads could appear
// without its collection, and access restricted after the preflight would
// still export what it now hides. An attribution error is returned, never
// swallowed: an export without it would read as complete.
func (s *Server) accountExportWorkspace(ctx context.Context, r *http.Request, wsID string) ([]models.Collection, []models.Item, error) {
	tx, err := s.store.BeginReadSnapshot(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Every read below runs under the export's deadline (BUG-3632): the
	// snapshot was begun with ctx, but a read issued through tx itself would
	// run under context.Background() and outlive the deadline.
	q := store.WithContext(ctx, tx)

	var visible []string
	if f := s.accountExportHooks.visibleColls; f != nil {
		visible, err = f(r, wsID)
	} else {
		visible, err = s.visibleCollectionIDsQ(r, q, wsID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("access check: %w", err)
	}
	if visible != nil {
		return nil, nil, errAccountExportRestricted
	}
	var collections []models.Collection
	if f := s.accountExportHooks.listCollections; f != nil {
		collections, err = f(wsID)
	} else {
		collections, err = s.store.ListCollectionsQ(q, wsID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("listing collections: %w", err)
	}
	if f := s.accountExportHooks.afterSnapshotCollections; f != nil {
		f(wsID)
	}
	// IncludeDeletedCollections: a user's own export keeps the items of
	// collections they deleted, as it always has (BUG-3425).
	params := models.ItemListParams{IncludeArchived: true, IncludeDeletedCollections: true}
	var items []models.Item
	if f := s.accountExportHooks.listItems; f != nil {
		items, err = f(wsID, params)
	} else {
		items, err = s.store.ListItemsQ(q, wsID, params)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("listing items: %w", err)
	}
	// SPEC-6 U9c.
	if len(items) > 0 {
		ids := make([]string, len(items))
		for i := range items {
			ids[i] = items[i].ID
		}
		var attr map[string]store.AppAttribution
		if f := s.accountExportHooks.itemAttribution; f != nil {
			attr, err = f(ids)
		} else {
			attr, err = s.store.ItemsCreatedViaAppQ(q, ids)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading app attribution: %w", err)
		}
		for i := range items {
			if a, ok := attr[items[i].ID]; ok {
				items[i].ViaApp, items[i].ViaAppName = a.InstallID, a.Name
			}
		}
	}
	return collections, items, nil
}

func (s *Server) accountExportListWorkspaces(ctx context.Context, userID string) ([]models.Workspace, error) {
	if f := s.accountExportHooks.listWorkspaces; f != nil {
		return f(userID)
	}
	return s.store.GetUserWorkspacesQ(s.store.QContext(ctx), userID)
}

func (s *Server) accountExportVisibleCollections(ctx context.Context, r *http.Request, wsID string) ([]string, error) {
	if f := s.accountExportHooks.visibleColls; f != nil {
		return f(r, wsID)
	}
	// The BUG-3334 fail-closed seam visibleCollectionIDs honours, kept on
	// this ctx-bound read (BUG-3632 r2).
	if s.visibleCollectionIDsFault != nil {
		if err := s.visibleCollectionIDsFault(); err != nil {
			return nil, err
		}
	}
	return s.visibleCollectionIDsQ(r, s.store.QContext(ctx), wsID)
}

// refuseAccountExportBeforeBody answers an export that cannot start: no byte
// of the body has gone out, so a status is still possible. A timeout is 503
// unavailable; anything else is the usual 500.
func refuseAccountExportBeforeBody(w http.ResponseWriter, userID, what string, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		slog.Error("account export timed out before the body started", "user_id", userID, "while", what, "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "The account export timed out before it could start; try again")
		return
	}
	writeInternalError(w, err)
}

func (s *Server) accountExportDeadline() time.Duration {
	if d := s.accountExportHooks.timeout; d > 0 {
		return d
	}
	return accountExportTimeout
}

// withinExportDeadline runs one export read on its own goroutine and stops
// waiting when the deadline passes, so nothing can hold the request open past
// accountExportTimeout. The store reads are themselves bound to the deadline
// (store.WithContext, BUG-3632), so a stalled query is cancelled and its
// connection returned; this is the backstop for whatever is not, and an
// abandoned read's result is dropped (the channel is buffered, so it never
// blocks).
//
// A read that fails once the deadline has passed is reported as the
// deadline, whatever the driver's error says: a cancelled read races
// ctx.Done() here, and the export tells a timeout (503) from a failure (500)
// by errors.Is, which must not depend on how a driver wraps its cancel error
// (BUG-3632 r2).
func withinExportDeadline[T any](ctx context.Context, read func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := read()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		if r.err != nil && ctx.Err() != nil {
			return r.v, fmt.Errorf("%w: %w", ctx.Err(), r.err)
		}
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// abortAccountExport ends an account export that cannot be completed. The
// status line has already gone out, so no error status is possible; instead
// the connection is cut mid-body (http.ErrAbortHandler, which chi's Recoverer
// re-panics and net/http handles without a stack trace). The download then
// FAILS in the browser, and the body never reaches its "complete": true
// trailer. Before BUG-3626 a timeout closed the JSON early and a store error
// was swallowed or noted inside one workspace, so a user leaving Pad could
// save a short file that parsed as their whole account.
func abortAccountExport(userID, reason string, err error) {
	slog.Error("account export aborted; the download fails rather than arriving short",
		"user_id", userID, "reason", reason, "error", err)
	panic(http.ErrAbortHandler)
}

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
	ctx, cancel := context.WithTimeout(r.Context(), s.accountExportDeadline())
	defer cancel()

	// Prefetch workspace list (small) before starting the streaming response.
	// Every read from here on runs inside the deadline (BUG-3659): these
	// first ones used to run outside it, so a read held by a lock kept the
	// request open with nothing bounding it.
	workspaces, err := withinExportDeadline(ctx, func() ([]models.Workspace, error) {
		return s.accountExportListWorkspaces(ctx, user.ID)
	})
	if err != nil {
		refuseAccountExportBeforeBody(w, user.ID, "listing workspaces", err)
		return
	}

	// BUG-1945: account-export dumps full collections/items for every
	// workspace this user owns (below), which bypasses BUG-1922's
	// workspace-export gate — a restricted owner (collection_access=
	// "specific") could still exfiltrate hidden collections via this
	// endpoint. Mirror BUG-1922's outright-deny: if the caller is
	// restricted in ANY owned workspace whose content this export would
	// include, refuse the whole export rather than silently omitting
	// that workspace — a partial-but-unflagged account dump would be as
	// misleading as an unfiltered one. Must run before any header/body
	// byte is written below, so a 403 here never follows partial output.
	for _, ws := range workspaces {
		if ws.OwnerID != user.ID {
			continue
		}
		visibleIDs, err := withinExportDeadline(ctx, func() ([]string, error) {
			return s.accountExportVisibleCollections(ctx, r, ws.ID)
		})
		if err != nil {
			refuseAccountExportBeforeBody(w, user.ID, "checking access to "+ws.Slug, err)
			return
		}
		if visibleIDs != nil {
			writeError(w, http.StatusForbidden, "forbidden",
				"Account export is unavailable while your workspace access is restricted")
			return
		}
	}

	if f := s.accountExportHooks.afterPreflight; f != nil {
		f()
	}

	// Stream the response — once we start writing, we can't send error status codes
	w.Header().Set("Content-Type", "application/json")
	// Not pad-export.json: that is the workspace bundle's file, a different
	// shape (BUG-3626).
	w.Header().Set("Content-Disposition", "attachment; filename=\"pad-account-export.json\"")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)

	// Write opening structure: the format and version first, so a reader
	// knows what it holds before anything else.
	fmt.Fprintf(w, "{\n  \"format\": %q,\n  \"version\": %d,\n  \"user\": ", accountExportFormat, accountExportVersion)
	enc.Encode(map[string]interface{}{
		"id":           user.ID,
		"email":        user.Email,
		"username":     user.Username,
		"name":         user.Name,
		"role":         user.Role,
		"plan":         user.EffectivePlan(time.Now()), // expiry enforced (BUG-3356)
		"totp_enabled": user.TOTPEnabled,
		"created_at":   user.CreatedAt,
		"updated_at":   user.UpdatedAt,
	})

	w.Write([]byte(",\n  \"workspaces\": [\n"))

	for i, ws := range workspaces {
		// Check timeout between workspaces
		if ctx.Err() != nil {
			abortAccountExport(user.ID, fmt.Sprintf("timed out after %d of %d workspaces", i, len(workspaces)), ctx.Err())
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

		// Only export full data for owned workspaces, read from one snapshot
		// inside the deadline (BUG-3659).
		if ws.OwnerID == user.ID {
			type content struct {
				collections []models.Collection
				items       []models.Item
			}
			c, err := withinExportDeadline(ctx, func() (content, error) {
				colls, items, err := s.accountExportWorkspace(ctx, r, ws.ID)
				return content{colls, items}, err
			})
			if err != nil {
				abortAccountExport(user.ID, "reading "+ws.Slug, err)
			}
			wsData["collections"] = c.collections
			wsData["items"] = c.items
		}

		w.Write([]byte("    "))
		enc.Encode(wsData)

		// Flush after each workspace to free memory and show progress
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// The last workspace may have used up the budget too.
	if ctx.Err() != nil {
		abortAccountExport(user.ID, "timed out after the last workspace", ctx.Err())
	}
	// The trailer is the completeness proof: written only here, after every
	// workspace, so a reader can refuse a file without it.
	w.Write([]byte("\n  ],\n  \"complete\": true\n}\n"))

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// oldSidecarEmptyCustomerRefusal is the exact body a sidecar that predates
// user_id sends for an empty customer_id: pad-cloud's writeJSON (a
// json.Encoder, so a trailing newline) of {"error": "customer_id must start
// with 'cus_'"} before BUG-3560.
const oldSidecarEmptyCustomerRefusal = `{"error":"customer_id must start with 'cus_'"}`

// olderSidecarRefusedEmptyCustomer reports whether a cancel-customer refusal
// is the one a sidecar that predates user_id gives an empty customer_id: a
// 400 whose body is exactly oldSidecarEmptyCustomerRefusal, surrounding
// whitespace aside. A current sidecar sends that message only for a
// non-empty customer_id without the prefix, and this door sends an empty
// one, so on this path it can only mean the old sidecar. The body is compared
// whole, not decoded: a decode would accept extra fields or a duplicated
// "error" key alongside the message (retro Codex review, round 1). Anything
// else, including another 400 or an unreadable body, is not it.
func olderSidecarRefusedEmptyCustomer(e *billing.SidecarError) bool {
	return e != nil && e.Status == http.StatusBadRequest &&
		strings.TrimSpace(e.Body) == oldSidecarEmptyCustomerRefusal
}
