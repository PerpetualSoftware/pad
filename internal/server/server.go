package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/xarmian/pad/internal/email"
	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/store"
	"github.com/xarmian/pad/internal/webhooks"
)

type Server struct {
	store     *store.Store
	router    *chi.Mux
	webFS     fs.FS                // embedded web UI static files (optional)
	events    *events.Bus          // real-time event bus (optional)
	webhooks  *webhooks.Dispatcher // webhook dispatcher (optional)
	email     *email.Sender        // transactional email sender (optional)
	baseURL   string               // public base URL for generating links (e.g. invite URLs)
	version   string               // release version (e.g. "dev", "1.2.3")
	commit    string               // git commit hash
	buildTime string               // build timestamp
}

func New(s *store.Store) *Server {
	srv := &Server{store: s}
	srv.setupRouter()
	return srv
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
func (s *Server) SetEventBus(bus *events.Bus) {
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

	// Middleware
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.RequestID)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:*", "http://127.0.0.1:*"},
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(s.TokenAuth)
	r.Use(s.SessionAuth)
	r.Use(s.RequireAuth)
	r.Use(jsonContentType)

	// SSE endpoint (outside jsonContentType middleware)
	r.Get("/api/v1/events", s.handleSSE)

	// API routes
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.handleHealth)

		// Auth endpoints (exempt from auth middleware)
		r.Route("/auth", func(r chi.Router) {
			r.Get("/session", s.handleSessionCheck)
			r.Post("/bootstrap", s.handleBootstrap)
			r.Post("/register", s.handleRegister)
			r.Post("/login", s.handleLogin)
			r.Post("/logout", s.handleLogout)
			r.Get("/me", s.handleGetCurrentUser)
			r.Patch("/me", s.handleUpdateCurrentUser)

			// Password reset
			r.Post("/forgot-password", s.handleForgotPassword)
			r.Post("/reset-password", s.handleResetPassword)

			// User-scoped API tokens
			r.Get("/tokens", s.handleListUserTokens)
			r.Post("/tokens", s.handleCreateUserToken)
			r.Delete("/tokens/{tokenID}", s.handleDeleteUserToken)
		})

		// Admin endpoints (admin-only, handlers check role internally)
		r.Route("/admin", func(r chi.Router) {
			r.Get("/settings", s.handleGetPlatformSettings)
			r.Patch("/settings", s.handleUpdatePlatformSettings)
			r.Post("/test-email", s.handleTestEmail)
		})

		// Templates
		r.Get("/templates", s.handleListTemplates)

		// Convention Library
		r.Get("/convention-library", s.handleConventionLibrary)

		// Playbook Library
		r.Get("/playbook-library", s.handlePlaybookLibrary)

		// Invitations (outside workspace scope)
		r.Post("/invitations/{code}/accept", s.handleAcceptInvitation)

		// Workspaces
		r.Route("/workspaces", func(r chi.Router) {
			r.Get("/", s.handleListWorkspaces)
			r.Post("/", s.handleCreateWorkspace)
			r.Post("/import", s.handleImportWorkspace)

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
						// Saved views within collection
						r.Get("/views", s.handleListViews)
						r.Post("/views", s.handleCreateView)
						r.Route("/views/{viewID}", func(r chi.Router) {
							r.Patch("/", s.handleUpdateView)
							r.Delete("/", s.handleDeleteView)
						})
					})
				})

				// Phases progress
				r.Get("/phases-progress", s.handlePhasesProgress)

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
					r.Get("/tasks", s.handleGetItemTasks)
				})

				// Links (v2)
				r.Delete("/links/{linkID}", s.handleDeleteItemLink)

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
				})

				// Dashboard (v2)
				r.Get("/dashboard", s.handleGetDashboard)
			})
		})

		// Search
		r.Get("/search", s.handleSearch)
	})

	s.router = r
}

// SetWebUI sets the embedded web UI filesystem for serving the SPA.
func (s *Server) SetWebUI(fsys fs.FS) {
	s.webFS = fsys
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

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(http.StatusOK)
		w.Write(indexHTML)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) ListenAndServe(addr string) error {
	log.Printf("Pad server listening on %s", addr)
	return http.ListenAndServe(addr, s.router)
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
		log.Printf("Error encoding JSON: %v", err)
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

func decodeJSON(r *http.Request, v interface{}) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// getWorkspaceID resolves workspace slug to ID.
func (s *Server) getWorkspaceID(w http.ResponseWriter, r *http.Request) (string, bool) {
	slug := chi.URLParam(r, "slug")
	ws, err := s.store.GetWorkspaceBySlug(slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return "", false
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return "", false
	}
	return ws.ID, true
}

// getWorkspaceDocument resolves workspace slug and document ID from URL params.
func (s *Server) getWorkspaceDocument(w http.ResponseWriter, r *http.Request) (string, *models.Document, bool) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return "", nil, false
	}

	docID := chi.URLParam(r, "docID")
	doc, err := s.store.GetDocument(docID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return "", nil, false
	}
	if doc == nil || doc.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Document not found")
		return "", nil, false
	}
	return workspaceID, doc, true
}
