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

	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/store"
)

type Server struct {
	store  *store.Store
	router *chi.Mux
	webFS  fs.FS       // embedded web UI static files (optional)
	events *events.Bus // real-time event bus (optional)
}

func New(s *store.Store) *Server {
	srv := &Server{store: s}
	srv.setupRouter()
	return srv
}

// SetEventBus attaches an event bus for real-time SSE streaming.
func (s *Server) SetEventBus(bus *events.Bus) {
	s.events = bus
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
		AllowedHeaders:   []string{"Accept", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(jsonContentType)

	// SSE endpoint (outside jsonContentType middleware)
	r.Get("/api/v1/events", s.handleSSE)

	// API routes
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.handleHealth)

		// Templates
		r.Get("/templates", s.handleListTemplates)

		// Convention Library
		r.Get("/convention-library", s.handleConventionLibrary)

		// Playbook Library
		r.Get("/playbook-library", s.handlePlaybookLibrary)

		// Workspaces
		r.Route("/workspaces", func(r chi.Router) {
			r.Get("/", s.handleListWorkspaces)
			r.Post("/", s.handleCreateWorkspace)
			r.Post("/import", s.handleImportWorkspace)

			r.Route("/{slug}", func(r chi.Router) {
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
					r.Get("/links", s.handleGetItemLinks)
					r.Post("/links", s.handleCreateItemLink)
					r.Get("/comments", s.handleListComments)
					r.Post("/comments", s.handleCreateComment)
					r.Get("/tasks", s.handleGetItemTasks)
				})

				// Links (v2)
				r.Delete("/links/{linkID}", s.handleDeleteItemLink)

				// Comments (v2)
				r.Delete("/comments/{commentID}", s.handleDeleteComment)

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
