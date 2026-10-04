package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appstore"
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
	Access   string // "read" or "write"
}

// appRoutes is U6a's table: the reads. U6b adds the writes, U6c the
// attachments.
var appRoutes = []appRoute{
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
		s.appstoreState.store = appstore.New(s.store, appstore.Options{PlanLimit: s.cloudMode, ETagKey: key})
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
				inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(s, w, r) })
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
		tok := appBearer(r)
		if tok == "" {
			writeAppUnauthorized(w)
			return
		}
		grant, err := s.introspectAppToken(r.Context(), tok)
		if err != nil {
			writeAppUnauthorized(w)
			return
		}
		// Delegated tokens are refused until TASK-3399 enables them (lead
		// ruling R4): U5b must turn them on, never inherit them.
		if grant.AuthKind != "service" {
			writeAppUnauthorized(w)
			return
		}
		inst, err := s.store.GetInstallAPIState(grant.InstallID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if inst == nil || inst.State != "active" || inst.WorkspaceID != grant.WorkspaceID {
			writeAppUnauthorized(w)
			return
		}
		access := inst.ServiceAccess
		if access != "read" && access != "write" {
			writeAppUnauthorized(w) // no service access granted
			return
		}
		bot, err := s.store.AppPrincipalForInstall(grant.InstallID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if bot == nil || bot.IsDisabled() || bot.ID != grant.Subject || (inst.BotUserID != "" && inst.BotUserID != bot.ID) {
			writeAppUnauthorized(w)
			return
		}
		ac := &appContext{Grant: grant, InstallID: grant.InstallID, WorkspaceID: grant.WorkspaceID,
			Access: access, AuthKind: grant.AuthKind, Actor: bot}
		ctx := context.WithValue(r.Context(), appCtxKey{}, ac)
		ctx = WithCurrentUser(ctx, bot)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
		ws, err := s.store.GetWorkspaceByID(ac.WorkspaceID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if ws == nil {
			writeWorkspaceNotFound(w, "Workspace not found")
			return
		}
		member, err := s.store.GetWorkspaceMember(ac.WorkspaceID, ac.Actor.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if member == nil {
			writeWorkspaceNotFound(w, "Workspace not found")
			return
		}
		ceiling, err := s.store.InstallReadCeilingQ(s.store.DB(), ac.InstallID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		companions, err := s.store.InstallCompanionCollectionIDsQ(s.store.DB(), ac.InstallID)
		if err != nil {
			writeInternalError(w, err)
			return
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

		ctx := context.WithValue(r.Context(), appCtxKey{}, &full)
		ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, ws.ID)
		ctx = context.WithValue(ctx, ctxWorkspaceRole, role)
		ctx = WithAPITokenAuth(ctx)
		ctx = WithTokenScopes(ctx, full.Access)
		ctx = WithTokenAllowedWorkspaces(ctx, []string{ws.Slug})
		r = r.WithContext(ctx)
		if err := appContextSelfCheck(r); err != nil {
			slog.Error("app API: context self-check failed", "error", err)
			writeInternalError(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
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
