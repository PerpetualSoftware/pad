package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xarmian/pad/internal/email"
	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/metrics"
	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/store"
	"github.com/xarmian/pad/internal/webhooks"
)

type Server struct {
	store         *store.Store
	router        *chi.Mux
	routerOnce    sync.Once            // ensures setupRouter runs once, after all config
	httpServer    *http.Server         // underlying HTTP server (set during ListenAndServe)
	webFS         fs.FS                // embedded web UI static files (optional)
	events        events.EventBus       // real-time event bus (optional)
	webhooks      *webhooks.Dispatcher // webhook dispatcher (optional)
	email         *email.Sender        // transactional email sender (optional)
	rateLimiters  *RateLimiters        // per-endpoint rate limiters
	baseURL       string               // public base URL for generating links (e.g. invite URLs)
	corsOrigins   string               // comma-separated CORS origins (empty = localhost defaults)
	secureCookies bool                 // set Secure flag on cookies (for TLS deployments)
	metrics            *metrics.Metrics      // Prometheus metrics (optional)
	sseMaxConnections  int                   // global SSE connection limit (0 = unlimited)
	sseMaxPerWorkspace int                   // per-workspace SSE connection limit (0 = unlimited)
	version            string               // release version (e.g. "dev", "1.2.3")
	commit              string               // git commit hash
	buildTime           string               // build timestamp
	twoFAChallengeSecret []byte              // HMAC key for 2FA challenge tokens
}

func New(s *store.Store) *Server {
	return &Server{
		store:        s,
		rateLimiters: NewRateLimiters(),
	}
}

// Init2FASecret loads the 2FA challenge signing key from platform_settings.
// If no key exists (first run), a new random key is generated and persisted.
// This must be called before the server handles requests so that challenge
// tokens survive process restarts and work across multiple instances.
func (s *Server) Init2FASecret() error {
	const settingKey = "2fa_challenge_secret"

	existing, err := s.store.GetPlatformSetting(settingKey)
	if err != nil {
		return fmt.Errorf("load 2FA secret: %w", err)
	}

	if existing != "" {
		decoded, err := base64.StdEncoding.DecodeString(existing)
		if err != nil {
			return fmt.Errorf("decode 2FA secret: %w", err)
		}
		s.twoFAChallengeSecret = decoded
		return nil
	}

	// First run — generate and persist a new secret.
	// Multiple instances may race here on a fresh database; after persisting,
	// re-read the winning value so all instances converge on the same key.
	secret, err := generateTwoFASecret()
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(secret)
	if err := s.store.SetPlatformSetting(settingKey, encoded); err != nil {
		return fmt.Errorf("persist 2FA secret: %w", err)
	}

	// Re-read to pick up whichever instance won the race (upsert may have
	// been overwritten by a concurrent instance between our check and write).
	final, err := s.store.GetPlatformSetting(settingKey)
	if err != nil {
		return fmt.Errorf("re-read 2FA secret: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(final)
	if err != nil {
		return fmt.Errorf("decode 2FA secret after re-read: %w", err)
	}
	s.twoFAChallengeSecret = decoded
	slog.Info("initialized 2FA challenge signing key")
	return nil
}

// SetVersion stores the build version info for the health endpoint.
func (s *Server) SetVersion(version, commit, buildTime string) {
	s.version = version
	s.commit = commit
	s.buildTime = buildTime
}

// SetBaseURL sets the public base URL used for generating shareable links.
func (s *Server) SetBaseURL(url string) {
	s.baseURL = strings.TrimRight(url, "/")
}

// SetEventBus attaches an event bus for real-time SSE streaming.
func (s *Server) SetEventBus(bus events.EventBus) {
	s.events = bus
}

// SetWebhookDispatcher attaches a webhook dispatcher for outgoing notifications.
func (s *Server) SetWebhookDispatcher(d *webhooks.Dispatcher) {
	s.webhooks = d
}

// SetEmailSender attaches a transactional email sender.
func (s *Server) SetEmailSender(e *email.Sender) {
	s.email = e
}

// SetCORSOrigins configures allowed CORS origins (comma-separated).
func (s *Server) SetCORSOrigins(origins string) {
	s.corsOrigins = origins
}

// SetSecureCookies enables the Secure flag on all cookies.
func (s *Server) SetSecureCookies(secure bool) {
	s.secureCookies = secure
}

// SetMetrics attaches Prometheus metrics to the server.
// Must be called before the first request is served.
func (s *Server) SetMetrics(m *metrics.Metrics) {
	s.metrics = m
}

// SetSSELimits configures global and per-workspace SSE connection limits.
// A value of 0 means unlimited.
func (s *Server) SetSSELimits(global, perWorkspace int) {
	s.sseMaxConnections = global
	s.sseMaxPerWorkspace = perWorkspace
}

// reconfigureEmail reads email settings from the platform_settings table
// and updates (or creates) the email sender. Called after admin settings change.
func (s *Server) reconfigureEmail() {
	apiKey, _ := s.store.GetPlatformSetting(settingMailerooAPIKey)
	fromAddr, _ := s.store.GetPlatformSetting(settingEmailFrom)
	fromName, _ := s.store.GetPlatformSetting(settingEmailFromName)

	if apiKey == "" {
		return // No API key — leave email as-is (may still have env var config)
	}

	if s.email == nil {
		// Create a new sender from platform settings
		s.email = email.NewSender(apiKey, fromAddr, fromName, s.baseURL)
	} else {
		// Update existing sender
		s.email.Configure(apiKey, fromAddr, fromName, s.baseURL)
	}
}

// InitEmailFromSettings loads email config from platform settings on startup,
// merging with any env-var-based sender that was already attached.
func (s *Server) InitEmailFromSettings() {
	s.reconfigureEmail()
}

func (s *Server) setupRouter() {
	r := chi.NewRouter()

	// Infrastructure middleware (applies to all routes including /metrics)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.RequestID)
	r.Use(StructuredLogger)
	if s.metrics != nil {
		r.Use(MetricsMiddleware(s.metrics))
	}
	r.Use(chimiddleware.Recoverer)

	// Security headers (applies to all routes)
	r.Use(SecurityHeaders)
	if s.secureCookies {
		r.Use(StrictTransportSecurity)
	}

	// Prometheus scrape endpoint — no auth/CSRF
	if s.metrics != nil {
		r.Group(func(r chi.Router) {
			r.Handle("/metrics", promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
		})
	}

	// All other routes — full middleware stack
	r.Group(func(r chi.Router) {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:   parseCORSOrigins(s.corsOrigins),
			AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
			AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Share-Password"},
			AllowCredentials: true,
			MaxAge:           300,
		}))
		r.Use(s.TokenAuth)
		r.Use(s.SessionAuth)
		r.Use(s.RateLimit)
		r.Use(s.CSRFProtect)
		r.Use(s.RequireAuth)
		r.Use(jsonContentType)

		// SSE endpoint (outside jsonContentType middleware — but inherits auth)
		r.Get("/api/v1/events", s.handleSSE)

		// API routes
		r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.handleHealth)
		r.Get("/health/live", s.handleHealthLive)
		r.Get("/health/ready", s.handleHealthReady)

		// Auth endpoints (exempt from auth middleware)
		r.Route("/auth", func(r chi.Router) {
			r.Get("/session", s.handleSessionCheck)
			r.Post("/bootstrap", s.handleBootstrap)
			r.Post("/register", s.handleRegister)
			r.Get("/check-username", s.handleCheckUsername)
			r.Post("/login", s.handleLogin)
			r.Post("/logout", s.handleLogout)
			r.Get("/me", s.handleGetCurrentUser)
			r.Patch("/me", s.handleUpdateCurrentUser)

			// Password reset
			r.Post("/forgot-password", s.handleForgotPassword)
			r.Post("/reset-password", s.handleResetPassword)

			// Two-factor authentication
			r.Post("/2fa/setup", s.handleTOTPSetup)
			r.Post("/2fa/verify", s.handleTOTPVerify)
			r.Post("/2fa/disable", s.handleTOTPDisable)
			r.Post("/2fa/login-verify", s.handleTOTPLoginVerify)

			// User-scoped API tokens
			r.Get("/tokens", s.handleListUserTokens)
			r.Post("/tokens", s.handleCreateUserToken)
			r.Delete("/tokens/{tokenID}", s.handleDeleteUserToken)
			r.Post("/tokens/{tokenID}/rotate", s.handleRotateUserToken)
		})

		// Admin endpoints (admin-only, handlers check role internally)
		r.Route("/admin", func(r chi.Router) {
			r.Get("/settings", s.handleGetPlatformSettings)
			r.Patch("/settings", s.handleUpdatePlatformSettings)
			r.Post("/test-email", s.handleTestEmail)
		})

		// Audit log (admin-only)
		r.Get("/audit-log", s.handleAuditLog)

		// Templates
		r.Get("/templates", s.handleListTemplates)

		// Convention Library
		r.Get("/convention-library", s.handleConventionLibrary)

		// Playbook Library
		r.Get("/playbook-library", s.handlePlaybookLibrary)

		// Invitations (outside workspace scope)
		r.Post("/invitations/{code}/accept", s.handleAcceptInvitation)

		// Share link resolution (outside workspace scope, no auth required)
		r.Get("/s/{token}", s.handleResolveShareLink)

		// Workspaces
		r.Route("/workspaces", func(r chi.Router) {
			r.Get("/", s.handleListWorkspaces)
			r.Post("/", s.handleCreateWorkspace)
			r.Post("/import", s.handleImportWorkspace)
			r.Put("/reorder", s.handleReorderWorkspaces)

			r.Route("/{slug}", func(r chi.Router) {
				r.Use(s.RequireWorkspaceAccess)

				r.Get("/", s.handleGetWorkspace)
				r.Patch("/", s.handleUpdateWorkspace)
				r.Delete("/", s.handleDeleteWorkspace)
				r.Get("/export", s.handleExportWorkspace)

				// Activity (workspace level)
				r.Get("/activity", s.handleListWorkspaceActivity)

				// Documents (v1 — will be replaced by items in Phase 2)
				r.Route("/documents", func(r chi.Router) {
					r.Get("/", s.handleListDocuments)
					r.Post("/", s.handleCreateDocument)

					r.Route("/{docID}", func(r chi.Router) {
						r.Get("/", s.handleGetDocument)
						r.Patch("/", s.handleUpdateDocument)
						r.Delete("/", s.handleDeleteDocument)
						r.Post("/restore", s.handleRestoreDocument)

						// Versions
						r.Get("/versions", s.handleListVersions)
						r.Get("/versions/{versionID}", s.handleGetVersion)

						// Activity (document level)
						r.Get("/activity", s.handleListDocumentActivity)
					})
				})

				// Collections (v2)
				r.Route("/collections", func(r chi.Router) {
					r.Get("/", s.handleListCollections)
					r.Post("/", s.handleCreateCollection)
					r.Route("/{collSlug}", func(r chi.Router) {
						r.Get("/", s.handleGetCollection)
						r.Patch("/", s.handleUpdateCollection)
						r.Delete("/", s.handleDeleteCollection)
						// Items within collection
						r.Get("/items", s.handleListCollectionItems)
						r.Post("/items", s.handleCreateItem)
						// Collection grants
						r.Get("/grants", s.handleListCollectionGrants)
						r.Post("/grants", s.handleCreateCollectionGrant)
						r.Delete("/grants/{grantID}", s.handleDeleteCollectionGrant)
						r.Get("/share-links", s.handleListCollectionShareLinks)
						r.Post("/share-links", s.handleCreateCollectionShareLink)
						// Saved views within collection
						r.Get("/views", s.handleListViews)
						r.Post("/views", s.handleCreateView)
						r.Route("/views/{viewID}", func(r chi.Router) {
							r.Patch("/", s.handleUpdateView)
							r.Delete("/", s.handleDeleteView)
						})
					})
				})

				// Plans progress
				r.Get("/plans-progress", s.handlePlansProgress)

				// User grants (all grants for a specific user in this workspace)
				r.Get("/users/{userID}/grants", s.handleListUserGrants)

				// Items (cross-collection, v2)
				r.Get("/items", s.handleListItems)
				r.Route("/items/{itemSlug}", func(r chi.Router) {
					r.Get("/", s.handleGetItem)
					r.Patch("/", s.handleUpdateItem)
					r.Delete("/", s.handleDeleteItem)
					r.Post("/restore", s.handleRestoreItem)
					r.Post("/move", s.handleMoveItem)
					r.Get("/versions", s.handleListItemVersions)
					r.Post("/versions/{versionID}/restore", s.handleRestoreItemVersion)
					r.Get("/activity", s.handleListItemActivity)
					r.Get("/links", s.handleGetItemLinks)
					r.Post("/links", s.handleCreateItemLink)
					r.Get("/comments", s.handleListComments)
					r.Post("/comments", s.handleCreateComment)
					r.Get("/timeline", s.handleListItemTimeline)
					r.Get("/children", s.handleGetItemChildren)
					r.Get("/progress", s.handleGetItemProgress)
					r.Get("/tasks", s.handleGetItemChildren) // deprecated alias
					r.Get("/grants", s.handleListItemGrants)
					r.Post("/grants", s.handleCreateItemGrant)
					r.Delete("/grants/{grantID}", s.handleDeleteItemGrant)
					r.Get("/share-links", s.handleListItemShareLinks)
					r.Post("/share-links", s.handleCreateItemShareLink)
				})

				// Links (v2)
				r.Delete("/links/{linkID}", s.handleDeleteItemLink)

				// Share links (workspace-scoped management)
				r.Delete("/share-links/{linkID}", s.handleDeleteShareLink)
				r.Get("/share-links/{linkID}/views", s.handleShareLinkViews)

				// Comments (v2)
				r.Route("/comments/{commentID}", func(r chi.Router) {
					r.Delete("/", s.handleDeleteComment)
					r.Post("/replies", s.handleCreateReply)
					r.Post("/reactions", s.handleAddReaction)
					r.Delete("/reactions/{emoji}", s.handleRemoveReaction)
				})

				// Role Board (cross-collection role-based view)
				r.Get("/roles/board", s.handleRoleBoard)
				r.Put("/roles/board/reorder", s.handleRoleBoardReorder)
				r.Put("/roles/board/lane-order", s.handleRoleBoardLaneReorder)

				// Agent Roles
				r.Route("/agent-roles", func(r chi.Router) {
					r.Get("/", s.handleListAgentRoles)
					r.Post("/", s.handleCreateAgentRole)
					r.Route("/{roleID}", func(r chi.Router) {
						r.Get("/", s.handleGetAgentRole)
						r.Patch("/", s.handleUpdateAgentRole)
						r.Delete("/", s.handleDeleteAgentRole)
					})
				})

				// Webhooks
				r.Route("/webhooks", func(r chi.Router) {
					r.Get("/", s.handleListWebhooks)
					r.Post("/", s.handleCreateWebhook)
					r.Route("/{webhookID}", func(r chi.Router) {
						r.Delete("/", s.handleDeleteWebhook)
						r.Post("/test", s.handleTestWebhook)
					})
				})

				// API Tokens
				r.Route("/tokens", func(r chi.Router) {
					r.Get("/", s.handleListTokens)
					r.Post("/", s.handleCreateToken)
					r.Delete("/{tokenID}", s.handleDeleteToken)
				})

				// Members
				r.Route("/members", func(r chi.Router) {
					r.Get("/", s.handleListMembers)
					r.Post("/invite", s.handleInviteMember)
					r.Delete("/invitations/{invID}", s.handleCancelInvitation)
					r.Delete("/{userID}", s.handleRemoveMember)
					r.Patch("/{userID}", s.handleUpdateMemberRole)
					r.Get("/{userID}/collection-access", s.handleGetMemberCollectionAccess)
					r.Put("/{userID}/collection-access", s.handleSetMemberCollectionAccess)
				})

				// Dashboard (v2)
				r.Get("/dashboard", s.handleGetDashboard)

				// Incremental sync — returns items changed since a timestamp
				r.Get("/changes", s.handleGetChanges)
			})
		})

		// Search
		r.Get("/search", s.handleSearch)
	})
	}) // end r.Group (full middleware stack)

	s.router = r
}

// SetWebUI sets the embedded web UI filesystem for serving the SPA.
func (s *Server) SetWebUI(fsys fs.FS) {
	s.webFS = fsys
	s.ensureRouter()
	s.router.Handle("/*", s.spaHandler())
}

func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.webFS))
	indexHTML, _ := fs.ReadFile(s.webFS, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/") {
			http.NotFound(w, r)
			return
		}

		cleanPath := strings.TrimPrefix(path, "/")
		if cleanPath != "" {
			if _, err := fs.Stat(s.webFS, cleanPath); err == nil {
				if strings.Contains(path, "/immutable/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// Generate per-request nonce for inline script CSP
		nonce := generateCSPNonce()

		// Inject nonce into inline <script> tags (SvelteKit bootstrap)
		html := bytes.Replace(indexHTML, []byte("<script>"), []byte(fmt.Sprintf(`<script nonce="%s">`, nonce)), -1)

		// Set nonce-based CSP (overrides the strict default from SecurityHeaders)
		w.Header().Set("Content-Security-Policy", fmt.Sprintf(
			"default-src 'self'; script-src 'self' 'nonce-%s'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'",
			nonce))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(http.StatusOK)
		w.Write(html)
	})
}

// ensureRouter lazily initializes the router on first use, so all Set*
// configuration is applied before the middleware chain is built.
func (s *Server) ensureRouter() {
	s.routerOnce.Do(func() {
		s.setupRouter()
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.ensureRouter()
	s.router.ServeHTTP(w, r)
}

func (s *Server) ListenAndServe(addr string) error {
	s.ensureRouter()

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.router,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout left at 0 — SSE connections are long-lived.
		// Non-SSE handlers should use per-request context deadlines.
	}

	slog.Info("Pad server listening", "addr", addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully drains in-flight requests and stops the HTTP server.
// The provided context controls how long to wait for active connections.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// Handler returns the configured HTTP handler (router).
// Useful for testing with httptest.NewServer.
func (s *Server) Handler() http.Handler {
	s.ensureRouter()
	return s.router
}

// --- helpers ---

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 7 && r.URL.Path[:7] == "/api/v1" {
			w.Header().Set("Content-Type", "application/json")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

// writeInternalError logs the real error server-side and sends a generic
// message to the client. This prevents leaking SQL errors, file paths,
// and other internal details.
func writeInternalError(w http.ResponseWriter, err error) {
	slog.Error("internal server error", "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred")
}

func decodeJSON(r *http.Request, v interface{}) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// getWorkspaceID resolves workspace slug/ID from the request.
// If RequireWorkspaceAccess already resolved the workspace, reads from context.
// Otherwise falls back to direct resolution (for unauthenticated paths).
func (s *Server) getWorkspaceID(w http.ResponseWriter, r *http.Request) (string, bool) {
	// Fast path: already resolved by RequireWorkspaceAccess middleware
	if wsID, ok := r.Context().Value(ctxResolvedWorkspaceID).(string); ok && wsID != "" {
		return wsID, true
	}

	// Slow path: resolve directly (should rarely happen — only for routes
	// that don't go through RequireWorkspaceAccess)
	slugOrID := chi.URLParam(r, "slug")
	ws, err := s.resolveWorkspace(slugOrID, currentUser(r))
	if err != nil {
		writeInternalError(w, err)
		return "", false
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return "", false
	}
	return ws.ID, true
}

// getWorkspace returns the full workspace object resolved by middleware.
// Falls back to direct resolution for routes without RequireWorkspaceAccess.
func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) (*models.Workspace, bool) {
	// Fast path: use middleware-resolved ID
	if wsID, ok := r.Context().Value(ctxResolvedWorkspaceID).(string); ok && wsID != "" {
		ws, err := s.store.GetWorkspaceByID(wsID)
		if err != nil {
			writeInternalError(w, err)
			return nil, false
		}
		if ws != nil {
			return ws, true
		}
	}

	// Slow path: resolve from URL param
	slugOrID := chi.URLParam(r, "slug")
	ws, err := s.resolveWorkspace(slugOrID, currentUser(r))
	if err != nil {
		writeInternalError(w, err)
		return nil, false
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return nil, false
	}
	return ws, true
}

// visibleCollectionIDs returns the set of collection IDs the current user can
// see in the given workspace. Returns nil if the user has "all" access (no
// filtering needed), or a non-nil slice for "specific" access. Admins and
// unauthenticated users (fresh install) always get nil (all access).
func (s *Server) visibleCollectionIDs(r *http.Request, workspaceID string) ([]string, error) {
	user := currentUser(r)
	if user == nil || user.Role == "admin" {
		return nil, nil // No filtering for admins or unauthenticated
	}
	return s.store.VisibleCollectionIDs(workspaceID, user.ID)
}

// guestVisibleItemIDs returns the item IDs a guest has item-level grants on.
// Returns nil for non-guests or if the guest has no item-level grants.
func (s *Server) guestVisibleItemIDs(r *http.Request, workspaceID string) ([]string, error) {
	if workspaceRole(r) != "guest" {
		return nil, nil
	}
	user := currentUser(r)
	if user == nil {
		return nil, nil
	}
	_, itemIDs, err := s.store.GuestVisibleResources(workspaceID, user.ID)
	return itemIDs, err
}

// requireItemVisible checks that the item's collection is visible to the
// requesting user. For guests with item-level grants, also verifies that the
// specific item is granted (not just the collection). Writes a 404 and returns
// false if not. Callers should invoke this immediately after resolving an item
// by slug/ID.
func (s *Server) requireItemVisible(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item) bool {
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if !isCollectionVisible(item.CollectionID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return false
	}

	// For users with item-level grants (guests or restricted members),
	// collection visibility may come from item-level grants.
	// We need to verify the user actually has a grant on this specific item
	// (not just another item in the same collection).
	// Uses guestResourceFilter which skips this check for members with "all" access.
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return false
	}
	if len(grantedItemIDs) > 0 {
		// If the collection has a full collection grant, the item is visible
		for _, id := range fullCollIDs {
			if id == item.CollectionID {
				return true
			}
		}
		// Check if this collection came from member_collection_access (not grants)
		if workspaceRole(r) != "guest" {
			memberColls, _ := s.store.GetMemberCollectionAccess(workspaceID, currentUserID(r))
			for _, id := range memberColls {
				if id == item.CollectionID {
					return true
				}
			}
		}
		// Otherwise, the specific item must be in the granted items list
		for _, id := range grantedItemIDs {
			if id == item.ID {
				return true
			}
		}
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return false
	}

	return true
}

// isItemVisibleToGuest checks if an item is visible given grant-based access,
// considering both full-collection grants and individual item grants.
// When fullCollIDs and grantedItemIDs are both nil, always returns true (no grant filtering).
func (s *Server) isItemVisibleToGuest(r *http.Request, workspaceID string, item *models.Item, fullCollIDs, grantedItemIDs []string) bool {
	if fullCollIDs == nil && grantedItemIDs == nil {
		return true
	}
	// Full collection grant covers all items in the collection
	for _, id := range fullCollIDs {
		if id == item.CollectionID {
			return true
		}
	}
	// Otherwise, the specific item must be in the granted items list
	for _, id := range grantedItemIDs {
		if id == item.ID {
			return true
		}
	}
	return false
}

// guestResourceFilter returns the full-collection IDs and granted item IDs for
// the current user if they need item-level grant filtering. Returns nil/nil for:
// - unauthenticated users
// - admin users
// - members with "all" collection access (grants should merge, not replace)
// For guests: returns direct collection grants as fullCollIDs + item grants.
// For restricted members: returns member_collection_access + system collections
// + direct collection grants as fullCollIDs, plus item grants as grantedItemIDs.
// This ensures item grants are additive to the member's existing access.
func (s *Server) guestResourceFilter(r *http.Request, workspaceID string) (fullCollIDs, grantedItemIDs []string, err error) {
	user := currentUser(r)
	if user == nil || user.Role == "admin" {
		return nil, nil, nil
	}

	role := workspaceRole(r)

	// For workspace members with "all" collection access, item grants should
	// not restrict their existing full visibility.
	if role != "guest" {
		member, err := s.store.GetWorkspaceMember(workspaceID, user.ID)
		if err != nil {
			return nil, nil, err
		}
		if member != nil && (member.CollectionAccess == "all" || member.CollectionAccess == "") {
			return nil, nil, nil
		}
	}

	// Get grant-based resources
	grantCollIDs, grantedItemIDs, err := s.store.GuestVisibleResources(workspaceID, user.ID)
	if err != nil {
		return nil, nil, err
	}

	// For guests, grant resources are the only source of access
	if role == "guest" {
		return grantCollIDs, grantedItemIDs, nil
	}

	// For restricted members ("specific" access), merge their normal
	// member_collection_access + system collections into fullCollIDs so
	// item grants are additive, not a replacement. This is critical:
	// without this merge, a member with access to collection A plus one
	// item grant in collection B would lose collection A in cross-collection
	// queries that use these IDs.
	fullCollSet := make(map[string]bool)
	for _, id := range grantCollIDs {
		fullCollSet[id] = true
	}

	// Add member_collection_access collections
	memberColls, err := s.store.GetMemberCollectionAccess(workspaceID, user.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range memberColls {
		fullCollSet[id] = true
	}

	// Add system collections (always visible to members)
	sysColls, err := s.store.ListSystemCollectionIDs(workspaceID)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range sysColls {
		fullCollSet[id] = true
	}

	fullCollIDs = make([]string, 0, len(fullCollSet))
	for id := range fullCollSet {
		fullCollIDs = append(fullCollIDs, id)
	}

	return fullCollIDs, grantedItemIDs, nil
}

// isCollectionVisible checks if a collection ID is in the visible set.
// If visibleIDs is nil, all collections are visible.
func isCollectionVisible(collectionID string, visibleIDs []string) bool {
	if visibleIDs == nil {
		return true
	}
	for _, id := range visibleIDs {
		if id == collectionID {
			return true
		}
	}
	return false
}

// requireEditPermission checks if the user has edit access to the given item.
// For regular members (editor/owner), this uses the standard role check.
// For members with insufficient roles (e.g., viewers), it falls back to
// grant-based permissions so grants can override the base role.
// For guests, it resolves the effective permission from grants directly.
// Returns true if the request should continue, false if it was rejected with a 403.
func (s *Server) requireEditPermission(w http.ResponseWriter, r *http.Request, workspaceID string, itemID, collectionID string) bool {
	role := workspaceRole(r)

	// Editors and owners always have edit access
	if role != "guest" && requireRole(r, "editor") {
		return true
	}

	// For guests and members with insufficient role (e.g., viewers),
	// check grant-based permissions as an override.
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusForbidden, "forbidden", "Insufficient permissions")
		return false
	}

	perm, err := s.store.ResolveUserPermission(workspaceID, user.ID, itemID, collectionID)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if permissionLevel(perm) < permissionLevel("edit") {
		writeError(w, http.StatusForbidden, "forbidden", "Insufficient permissions")
		return false
	}
	return true
}

// resolveWorkspace resolves a workspace by slug or UUID, scoped to the
// authenticated user's accessible workspaces when a user context is present.
// Returns nil (not an error) if no workspace is found.
func (s *Server) resolveWorkspace(slugOrID string, user *models.User) (*models.Workspace, error) {
	// 1. Is it a UUID? Try resolving by ID first, then fall back to slug.
	//    A workspace slug could be UUID-shaped (e.g. imported data), so we
	//    can't short-circuit here.
	if isUUID(slugOrID) {
		ws, err := s.store.GetWorkspaceByID(slugOrID)
		if ws != nil || err != nil {
			return ws, err
		}
		// Not found by ID — fall through to slug-based resolution
	}

	// 2. No authenticated user — fall back to global slug lookup
	//    (fresh install, or pre-auth paths)
	if user == nil {
		return s.store.GetWorkspaceBySlug(slugOrID)
	}

	// 3. Admin users — global slug lookup (admins can see all workspaces)
	if user.Role == "admin" {
		return s.store.GetWorkspaceBySlug(slugOrID)
	}

	// 4. Auth-scoped slug resolution: find workspaces where user is owner or member
	workspaces, err := s.store.GetWorkspacesBySlugForUser(slugOrID, user.ID)
	if err != nil {
		return nil, err
	}

	if len(workspaces) == 1 {
		return &workspaces[0], nil
	}
	if len(workspaces) == 0 {
		return nil, nil
	}

	// Ambiguous: multiple workspaces match — this should be rare.
	// For now, return the first one. The 409 disambiguation is only needed
	// when we actually have per-owner slug uniqueness (after the unique
	// constraint is changed). Currently slugs are globally unique.
	return &workspaces[0], nil
}

// isUUID is defined in handlers_items.go

// getWorkspaceDocument resolves workspace slug and document ID from URL params.
func (s *Server) getWorkspaceDocument(w http.ResponseWriter, r *http.Request) (string, *models.Document, bool) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return "", nil, false
	}

	docID := chi.URLParam(r, "docID")
	doc, err := s.store.GetDocument(docID)
	if err != nil {
		writeInternalError(w, err)
		return "", nil, false
	}
	if doc == nil || doc.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Document not found")
		return "", nil, false
	}
	return workspaceID, doc, true
}
