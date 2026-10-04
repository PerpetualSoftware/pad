package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appstore"
	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The installed-app API (SPEC-6 §4, U6a, TASK-3401): /api/app/v1, a router of
// its own, mounted outside TokenAuth and RequireAuth so no session or PAT can
// ever authenticate it, and no app token can reach /api/v1.
//
// A request passes, in order:
//  1. appTokenAuth: an active app ACCESS token (introspectAppToken: audience,
//     binding, install active, client enabled, epoch current); the install's
//     own bot as the actor; delegated tokens refused until TASK-3399.
//  2. per route, the access check: a "write" row refuses a read token with
//     403 before anything is resolved or looked up.
//  3. per route, requireAppWorkspace: {ws} must be the token's workspace by
//     ID (R3), else the same 404 a missing workspace gets; then every context
//     key the shared helpers read is set from the grant, and checked.
//  4. the handler, which authorizes through the shared helpers under the
//     ceiling and encodes a fixed DTO.

// appRoute is one row of the route table. The table builds the router and
// drives the tests; adding a route is a spec amendment.
type appRoute struct {
	Method   string
	Template string // under /api/app/v1/workspaces/{ws}
	Name     string
	Handler  func(*Server, http.ResponseWriter, *http.Request)
	Auth     string // "service", "delegated" or "either"
	// Access is "read", "write" or "stream". A stream row is a read whose
	// body is NOT buffered (an attachment download, lead ruling for U6c): the
	// handler re-admits the request itself, immediately before its first
	// byte, and then streams.
	Access string
}

// appRoutes is the table: U6a's reads, U6b's writes and U6c's attachments.
var appRoutes = []appRoute{
	{"POST", "/items/{itemID}/attachments", "appUploadAttachment", (*Server).appUploadAttachment, "either", "write"},
	{"GET", "/attachments/{attachmentID}", "appGetAttachment", (*Server).appGetAttachment, "either", "read"},
	{"GET", "/attachments/{attachmentID}/content", "appDownloadAttachment", (*Server).appDownloadAttachment, "either", "stream"},
	// HEAD probes size and type through the same gate (ServeContent writes
	// no body for it), as the regular download allows.
	{"HEAD", "/attachments/{attachmentID}/content", "appHeadAttachment", (*Server).appDownloadAttachment, "either", "stream"},
	{"POST", "/collections/{collSlug}/items", "appCreateItem", (*Server).appCreateItem, "either", "write"},
	{"PATCH", "/items/{itemID}", "appUpdateItem", (*Server).appUpdateItem, "either", "write"},
	{"POST", "/items/{itemID}/comments", "appCreateComment", (*Server).appCreateComment, "either", "write"},
	{"PATCH", "/items/{itemID}/comments/{commentID}", "appUpdateComment", (*Server).appUpdateComment, "either", "write"},
	{"DELETE", "/items/{itemID}/comments/{commentID}", "appDeleteComment", (*Server).appDeleteComment, "either", "write"},
	{"GET", "/collections", "appListCollections", (*Server).appListCollections, "either", "read"},
	{"GET", "/collections/{collSlug}", "appGetCollection", (*Server).appGetCollection, "either", "read"},
	{"GET", "/collections/{collSlug}/items", "appListItems", (*Server).appListItems, "either", "read"},
	{"GET", "/items/{itemID}", "appGetItem", (*Server).appGetItem, "either", "read"},
	{"GET", "/items/{itemID}/comments", "appListComments", (*Server).appListComments, "either", "read"},
	{"GET", "/me", "appMe", (*Server).appMe, "either", "read"},
}

// appAPIPrefix is where the app router is mounted.
const appAPIPrefix = "/api/app/v1"

// appCtxKey holds the immutable app context of a request.
type appCtxKey struct{}

// appContext is what the token and workspace steps established. It is set
// once and never modified; every app handler reads it.
type appContext struct {
	Grant         *AppTokenGrant
	InstallID     string
	WorkspaceID   string
	WorkspaceSlug string
	Access        string // "read" or "write"
	AuthKind      string // "service" (delegated arrives with TASK-3399)
	Actor         *models.User
	ReadCeiling   []string // companion ∪ system collection ids
	Companions    []string // companion collection ids (the write set)
	Role          string   // the role the shared helpers see

	// token is the presented access token, kept so the request can be
	// re-admitted before its response is sent. Never serialized or logged.
	token string
}

func appContextFrom(r *http.Request) *appContext {
	c, _ := r.Context().Value(appCtxKey{}).(*appContext)
	return c
}

// appFenceSpec is the appstore fence for this request.
func (c *appContext) appFenceSpec() store.FenceSpec {
	return store.FenceSpec{InstallID: c.InstallID, WorkspaceID: c.WorkspaceID, Epoch: c.Grant.AuthEpoch, Companions: c.Companions}
}

// appStoreState lazily builds the server's one appstore. One per Server,
// because the upload reservations live in process (U6c wires the blobs).
type appStoreState struct {
	once  sync.Once
	store *appstore.Store
	err   error
}

// appETagLabel names the HKDF purpose of the etag key: server-only,
// mandatory and persistent (lead ruling R5 on TASK-3401).
const appETagLabel = "pad-app-etag-v1"

func (s *Server) appStore() (*appstore.Store, error) {
	s.appstoreState.once.Do(func() {
		key, err := s.store.DeriveServerKey(appETagLabel)
		if err != nil {
			s.appstoreState.err = err
			return
		}
		// Uploads write through the filesystem blob store the human uploads
		// use, with constant work, under the same hash guard. With no FS
		// backend wired the app store refuses uploads and downloads
		// (ErrAppNoAttachmentStore). Resolved once, like the store itself,
		// which holds the in-process upload reservations: SetAttachments runs
		// at startup, before any request.
		var blobs *attachments.FSStore
		if s.attachments != nil {
			blobs, _ = s.attachments.Backends()[attachments.FSPrefix].(*attachments.FSStore)
		}
		s.appstoreState.store = appstore.New(s.store, appstore.Options{PlanLimit: s.cloudMode, ETagKey: key,
			Blobs: blobs, InFlight: s.UploadInFlight()})
	})
	return s.appstoreState.store, s.appstoreState.err
}

// registerAppAPIRoutes mounts the app router. The router sets its own
// Content-Type (jsonContentType matches only /api/v1), and RateLimit runs
// AFTER the token step, so it keys on the actor rather than the address.
func (s *Server) registerAppAPIRoutes(r chi.Router) {
	r.Route(appAPIPrefix, func(r chi.Router) {
		r.Use(appJSONContentType)
		r.Use(s.requireAppsAvailable)
		r.Use(s.appTokenAuth)
		r.Use(s.RateLimit)
		r.Route("/workspaces/{ws}", func(r chi.Router) {
			for _, rt := range appRoutes {
				handler := rt.Handler
				access := rt.Access
				inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if s.appBeforeHandler != nil {
						s.appBeforeHandler()
					}
					// Writes are NOT re-validated after commit (lead ruling R2,
					// U6b): a write's authority is decided inside its FencedTx
					// under the epoch fence, and refusing the response AFTER a
					// commit would hide a write that happened and invite a
					// duplicate retry. Re-admission before send is for reads.
					if access == "write" {
						handler(s, w, r)
						return
					}
					// A stream row registers its re-checks like any read, but
					// re-admits itself before its first byte (appStreamGate)
					// instead of being buffered: a download is too large to
					// hold, and nothing is sent before the gate passes.
					if access == "stream" {
						handler(s, w, r.WithContext(context.WithValue(r.Context(), appRecheckKey{}, &appRechecks{})))
						return
					}
					// The handler writes into a buffer; nothing reaches the
					// app until the grant is re-validated (codex r1 P1).
					buf := newAppResponseBuffer()
					r = r.WithContext(context.WithValue(r.Context(), appRecheckKey{}, &appRechecks{}))
					handler(s, buf, r)
					if s.appAfterHandler != nil {
						s.appAfterHandler()
					}
					if err := s.appRevalidate(r); err != nil {
						writeAppRevalidateError(w, err)
						return
					}
					buf.flushTo(w)
				})
				h := s.requireAppAccess(rt.Access, s.requireAppWorkspace(inner))
				r.Method(rt.Method, rt.Template, h)
			}
		})
	})
}

func appJSONContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// requireAppsAvailable answers every app route with the same JSON 404 while
// installed apps are off on this server.
func (s *Server) requireAppsAvailable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.appsAvailable() {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeAppUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writeError(w, http.StatusUnauthorized, "unauthorized", "A valid app token is required")
}

// appBearer reads the token from the Authorization header only: the app API
// takes no token from a query string or a form.
func appBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) < 8 || !strings.EqualFold(auth[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}

// appTokenAuth is step 1. It establishes the grant and the actor, and stores
// a partial app context; the workspace step completes it.
func (s *Server) appTokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ac, err := s.appAdmitToken(r.Context(), appBearer(r))
		if err != nil {
			if errors.Is(err, errAppAdmitInternal) {
				writeInternalError(w, err)
				return
			}
			writeAppUnauthorized(w)
			return
		}
		ctx := context.WithValue(r.Context(), appCtxKey{}, ac)
		ctx = WithCurrentUser(ctx, ac.Actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

var (
	errAppAdmit         = errors.New("app request not admitted")
	errAppAdmitInternal = errors.New("app admission failed")
)

// appAdmitToken is the token half of admission, shared by the door and the
// re-admission before a response is sent: the token is introspected (active,
// not expired, audience, binding, install, client, epoch), delegated grants
// are refused until TASK-3399, the install's service access is read, and the
// install's own bot must be the subject, live and enabled.
func (s *Server) appAdmitToken(ctx context.Context, tok string) (*appContext, error) {
	if tok == "" {
		return nil, errAppAdmit
	}
	grant, err := s.introspectAppToken(ctx, tok)
	if err != nil {
		// A store failure while reading the token's state is a fault, not a
		// refusal (codex U6c r4): a 401 invalid_token would tell the app to
		// discard a credential that is fine. Every named refusal stays a
		// denial, and so does the OAuth library's own introspection error,
		// which does not distinguish a storage fault from an unknown token
		// and therefore fails closed.
		if !isAppTokenDenial(err) {
			return nil, fmt.Errorf("%w: %w", errAppAdmitInternal, err)
		}
		return nil, errAppAdmit
	}
	// Delegated tokens are refused until TASK-3399 enables them (lead
	// ruling R4): U5b must turn them on, never inherit them.
	if grant.AuthKind != "service" {
		return nil, errAppAdmit
	}
	inst, err := s.store.GetInstallAPIState(grant.InstallID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAppAdmitInternal, err)
	}
	if inst == nil || inst.State != "active" || inst.WorkspaceID != grant.WorkspaceID {
		return nil, errAppAdmit
	}
	if inst.ServiceAccess != "read" && inst.ServiceAccess != "write" {
		return nil, errAppAdmit // no service access granted
	}
	bot, err := s.store.AppPrincipalForInstall(grant.InstallID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAppAdmitInternal, err)
	}
	if bot == nil || bot.IsDisabled() || bot.ID != grant.Subject || (inst.BotUserID != "" && inst.BotUserID != bot.ID) {
		return nil, errAppAdmit
	}
	return &appContext{Grant: grant, InstallID: grant.InstallID, WorkspaceID: grant.WorkspaceID,
		Access: inst.ServiceAccess, AuthKind: grant.AuthKind, Actor: bot, token: tok}, nil
}

// requireAppAccess is step 2: a write row refuses a read token, before any
// resolution or lookup.
func (s *Server) requireAppAccess(need string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ac := appContextFrom(r)
		if ac == nil {
			writeAppUnauthorized(w)
			return
		}
		if need == "write" && ac.Access != "write" {
			writeError(w, http.StatusForbidden, "insufficient_access", "This app's access is read-only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAppWorkspace is step 3 (RequireAppWorkspaceAccess). {ws} must be the
// token's workspace, by ID (lead ruling R3): a slug, another workspace, or a
// deleted one all get the workspace 404. It then sets every context key the
// shared helpers read, from the grant and real membership, and refuses the
// request if any is missing.
func (s *Server) requireAppWorkspace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ac := appContextFrom(r)
		if ac == nil {
			writeAppUnauthorized(w)
			return
		}
		if chi.URLParam(r, "ws") != ac.WorkspaceID {
			writeWorkspaceNotFound(w, "Workspace not found")
			return
		}
		full, err := s.appAdmitWorkspace(ac)
		if err != nil {
			if errors.Is(err, errAppAdmitInternal) {
				writeInternalError(w, err)
				return
			}
			writeWorkspaceNotFound(w, "Workspace not found")
			return
		}
		r = r.WithContext(appRequestContext(r.Context(), full))
		if err := appContextSelfCheck(r); err != nil {
			slog.Error("app API: context self-check failed", "error", err)
			writeInternalError(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// appAdmitWorkspace is the workspace half of admission, shared by the door
// and the re-admission: the workspace is live, the bot is a member, and the
// role, read ceiling and companion set are computed from the store as they
// stand now.
func (s *Server) appAdmitWorkspace(ac *appContext) (*appContext, error) {
	ws, err := s.store.GetWorkspaceByID(ac.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAppAdmitInternal, err)
	}
	if ws == nil {
		return nil, errAppAdmit
	}
	member, err := s.store.GetWorkspaceMember(ac.WorkspaceID, ac.Actor.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAppAdmitInternal, err)
	}
	if member == nil {
		return nil, errAppAdmit
	}
	ceiling, err := s.store.InstallReadCeilingQ(s.store.DB(), ac.InstallID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAppAdmitInternal, err)
	}
	companions, err := s.store.InstallCompanionCollectionIDsQ(s.store.DB(), ac.InstallID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAppAdmitInternal, err)
	}
	// The role the shared helpers see: the membership's, capped by the
	// token's access. A read token never acts as more than a viewer.
	role := member.Role
	if role == "owner" {
		role = "editor" // a bot is never an owner (TASK-3392); defence in depth
	}
	if ac.Access == "read" {
		role = "viewer"
	}
	full := *ac
	full.WorkspaceSlug = ws.Slug
	full.ReadCeiling = ceiling
	full.Companions = companions
	full.Role = role
	return &full, nil
}

// appRequestContext sets every key the shared helpers read from a complete
// app context.
func appRequestContext(parent context.Context, ac *appContext) context.Context {
	ctx := context.WithValue(parent, appCtxKey{}, ac)
	ctx = WithCurrentUser(ctx, ac.Actor)
	ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, ac.WorkspaceID)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, ac.Role)
	ctx = WithAPITokenAuth(ctx)
	ctx = WithTokenScopes(ctx, ac.Access)
	return WithTokenAllowedWorkspaces(ctx, []string{ac.WorkspaceSlug})
}

// appContextSelfCheck refuses a request whose context lacks any key the
// shared helpers read (§4 step 5).
func appContextSelfCheck(r *http.Request) error {
	ac := appContextFrom(r)
	switch {
	case ac == nil || ac.Grant == nil || ac.Actor == nil || ac.InstallID == "" || ac.WorkspaceSlug == "":
		return errors.New("app context incomplete")
	case ac.ReadCeiling == nil || ac.Companions == nil:
		return errors.New("app ceiling not set")
	case currentUser(r) == nil || currentUser(r).ID != ac.Actor.ID:
		return errors.New("current user is not the app actor")
	case workspaceRole(r) == "":
		return errors.New("workspace role not set")
	case r.Context().Value(ctxResolvedWorkspaceID) != ac.WorkspaceID:
		return errors.New("resolved workspace is not the token's")
	case !isAPITokenAuth(r):
		return errors.New("bearer status not set")
	case TokenScopesFromContext(r.Context()) == "":
		return errors.New("scopes not set")
	}
	allowed := TokenAllowedWorkspacesFromContext(r.Context())
	if len(allowed) != 1 || allowed[0] != ac.WorkspaceSlug {
		return errors.New("token allow-list is not exactly the install workspace")
	}
	return nil
}

// appCeilingAllows reports whether a collection is inside the request's app
// read ceiling. Outside the app API (no app context) everything passes.
func appCeilingAllows(r *http.Request, collectionID string) bool {
	ac := appContextFrom(r)
	if ac == nil {
		return true
	}
	for _, id := range ac.ReadCeiling {
		if id == collectionID {
			return true
		}
	}
	return false
}

// appResponseBuffer holds a handler's response until the grant is
// re-validated (codex r1 P1).
type appResponseBuffer struct {
	header http.Header
	status int
	body   []byte
}

func newAppResponseBuffer() *appResponseBuffer { return &appResponseBuffer{header: http.Header{}} }

func (b *appResponseBuffer) Header() http.Header { return b.header }

func (b *appResponseBuffer) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *appResponseBuffer) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

func (b *appResponseBuffer) flushTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body)
}

// appRecheckKey holds the request's re-checks: one per resource a handler
// authorized, replayed before the response is sent.
type appRecheckKey struct{}

type appRechecks struct{ fns []func(*http.Request) error }

// appAddRecheck registers a re-check of an authorization the handler made.
// It is run against the store as it stands when the response is about to be
// sent, under the re-admitted context.
func appAddRecheck(r *http.Request, fn func(*http.Request) error) {
	if rc, ok := r.Context().Value(appRecheckKey{}).(*appRechecks); ok {
		rc.fns = append(rc.fns, fn)
	}
}

// appRevalidate re-admits the request from scratch before its response is
// sent (codex r1 and r2): the token is introspected again (revocation,
// expiry, binding, epoch, install, client), the install's access and bot are
// read again, and the workspace, membership, role and ceiling are recomputed.
// The result must match what the request was admitted under, and then every
// authorization the handler made is replayed under the re-admitted context.
// A change to anything that authorized the response, committed while the
// request was in flight, therefore withholds it: every response is ordered
// against revocation at this check. App mutations are ordered by their
// FencedTx; this orders the reads.
func (s *Server) appRevalidate(r *http.Request) error {
	ac := appContextFrom(r)
	if ac == nil || ac.Grant == nil {
		return errors.New("no app context")
	}
	// A route without the re-check holder cannot have registered its
	// authorizations, so it fails closed rather than passing with none.
	rc, ok := r.Context().Value(appRecheckKey{}).(*appRechecks)
	if !ok {
		return errors.New("no re-check holder")
	}
	// The gate every new request passes (codex r3): apps turned off while a
	// read was in flight withholds it too.
	if on, err := s.appsAvailableChecked(); err != nil {
		return appFault(err)
	} else if !on {
		return errors.New("apps are not available")
	}
	tokAC, err := s.appAdmitToken(r.Context(), ac.token)
	if err != nil {
		return err
	}
	if tokAC.Grant.RequestID != ac.Grant.RequestID || tokAC.Grant.AuthEpoch != ac.Grant.AuthEpoch ||
		tokAC.InstallID != ac.InstallID || tokAC.WorkspaceID != ac.WorkspaceID ||
		tokAC.Access != ac.Access || tokAC.Actor.ID != ac.Actor.ID {
		return errors.New("the grant changed")
	}
	fresh, err := s.appAdmitWorkspace(tokAC)
	if err != nil {
		return err
	}
	if fresh.Role != ac.Role || fresh.WorkspaceSlug != ac.WorkspaceSlug {
		return errors.New("the membership changed")
	}
	r2 := r.WithContext(context.WithValue(appRequestContext(r.Context(), fresh), appRecheckMemoKey{}, &appRecheckMemo{collections: map[string]error{}}))
	for _, fn := range rc.fns {
		if err := fn(r2); err != nil {
			return err
		}
	}
	if s.appAfterRechecks != nil {
		s.appAfterRechecks()
	}
	// The credential once more, LAST (codex U6c r1 P1): the checks above take
	// time, and a revocation, disable or rotation committed while they ran
	// would otherwise pass. Re-admission is a sequence of reads, not one
	// snapshot, so it orders the response against a change committed before
	// its final step: this one, for the credential and the install, and each
	// re-check's own read for the resource it covers.
	last, err := s.appAdmitToken(r.Context(), ac.token)
	if err != nil {
		return err
	}
	if last.Grant.AuthEpoch != ac.Grant.AuthEpoch || last.Access != ac.Access || last.InstallID != ac.InstallID {
		return errors.New("the grant changed")
	}
	return nil
}

// isAppTokenDenial reports whether an introspection error is one of its named
// refusals, rather than a failure reading the token's state.
func isAppTokenDenial(err error) bool {
	for _, d := range []error{errAppTokenInactive, errAppTokenAudience, errAppTokenUnbound, errAppTokenMismatch,
		errAppTokenInstall, errAppTokenEpoch, errAppTokenDisabled, errAppTokenNoBackend} {
		if errors.Is(err, d) {
			return true
		}
	}
	return false
}

// appFault marks an error from re-admission as a server fault (a store or
// database failure), not a denial: the response is withheld either way, but
// a fault answers 500, never the 401 invalid_token that tells an app to
// discard a credential that is fine (codex U6c r2).
func appFault(err error) error { return fmt.Errorf("%w: %w", errAppAdmitInternal, err) }

// writeAppRevalidateError answers a failed re-admission: 500 for a fault,
// 401 for a denial.
func writeAppRevalidateError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAppAdmitInternal) {
		writeInternalError(w, err)
		return
	}
	writeAppUnauthorized(w)
}

// appRecheckMemoKey holds one re-validation's memo of collection re-checks:
// a list replays the same collection for every item, so each collection is
// re-checked once per re-validation. The re-checks run sequentially, not as an
// atomic snapshot; the memo only stops repeating one collection's check. It is
// fresh per re-validation and not shared between goroutines.
type appRecheckMemoKey struct{}

type appRecheckMemo struct{ collections map[string]error }

// appWriteAllows reports whether a collection is one the app may write: a
// companion. Outside the app API everything passes.
func appWriteAllows(r *http.Request, collectionID string) bool {
	ac := appContextFrom(r)
	if ac == nil {
		return true
	}
	for _, id := range ac.Companions {
		if id == collectionID {
			return true
		}
	}
	return false
}
