package server

import (
	"net/http"
	"regexp"
	"sync"
	"time"
)

// BUG-3475: an import's OUTCOME must reach the person who started it even
// when the response does not. The web dialog uploads over whatever path the
// browser has, and on a path that drops packets (Dave's QUIC to Cloudflare)
// the client gives up on its own timers while the server, 60s later on its
// per-Read deadline, finishes the import one way or the other. A lost 201 is
// the same shape: a complete workspace the client reported as failed, and a
// retry made a duplicate.
//
// So the client names each attempt with an import key (?import_key=), the
// bundle door records what became of it, and GET /workspaces/import-status
// answers it for the caller who started it.
//
// IN MEMORY, by ruling: one instance, a 1h TTL, lost on restart. A key this
// registry does not hold is an UNKNOWN outcome, never "nothing was created":
// the client says "check your workspace list" for it.

// Import outcome states, as GET /workspaces/import-status reports them.
const (
	importStateRunning    = "running"     // the server is still reading or importing
	importStateComplete   = "complete"    // the workspace was imported (201)
	importStateKept       = "kept"        // a mid-stream DATA error; the partial workspace was kept and is the caller's
	importStateRemoved    = "removed"     // a workspace was created and then removed (a reject or an interrupted upload)
	importStateNotCreated = "not_created" // the import failed before any workspace existed
	importStateUnknown    = "unknown"     // the server could not establish what was left behind
)

const (
	importOutcomeTTL = time.Hour
	// importOutcomeMax bounds the map. A key is minted per attempt by a
	// signed-in user, so this is far above real use; past it the oldest
	// entries go first.
	importOutcomeMax = 10000
)

// importKeyPattern: the client sends a UUID; anything key-shaped and bounded
// is accepted so a future client is not tied to that spelling.
var importKeyPattern = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

type importOutcome struct {
	State         string `json:"state"`
	WorkspaceSlug string `json:"workspace_slug,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	// OwnerUsername is the importer's username, for the client's link: an
	// import's workspace is always owned by the caller who started it.
	OwnerUsername string `json:"owner_username,omitempty"`
	updated       time.Time
}

type importOutcomeKey struct{ userID, key string }

type importOutcomeRegistry struct {
	mu      sync.Mutex
	entries map[importOutcomeKey]*importOutcome
	now     func() time.Time
}

func newImportOutcomeRegistry() *importOutcomeRegistry {
	return &importOutcomeRegistry{entries: map[importOutcomeKey]*importOutcome{}, now: time.Now}
}

// begin records a new attempt as running. It refuses a key the same user
// already has RUNNING: two uploads under one key would race to report.
func (g *importOutcomeRegistry) begin(userID, key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked()
	k := importOutcomeKey{userID, key}
	if e, ok := g.entries[k]; ok && e.State == importStateRunning {
		return false
	}
	if len(g.entries) >= importOutcomeMax && !g.evictOldestSettledLocked() {
		// Every entry is a running import (codex r1 on BUG-3475). Evicting
		// one would let its key begin again beside the upload still using
		// it, so this attempt goes untracked instead: its status reads as
		// unknown, which is the truth about it.
		return true
	}
	g.entries[k] = &importOutcome{State: importStateRunning, updated: g.now()}
	return true
}

func (g *importOutcomeRegistry) finish(userID, key, state, slug, name, ownerUsername string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := importOutcomeKey{userID, key}
	e, ok := g.entries[k]
	if !ok {
		// Untracked at begin (the map was full of running imports): only an
		// attempt that was tracked has an outcome to report.
		return
	}
	e.State, e.WorkspaceSlug, e.WorkspaceName, e.OwnerUsername, e.updated = state, slug, name, ownerUsername, g.now()
}

// get answers only the user who started the attempt; another user's key is
// indistinguishable from an unknown one.
func (g *importOutcomeRegistry) get(userID, key string) (importOutcome, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked()
	e, ok := g.entries[importOutcomeKey{userID, key}]
	if !ok {
		return importOutcome{}, false
	}
	return *e, true
}

func (g *importOutcomeRegistry) sweepLocked() {
	cutoff := g.now().Add(-importOutcomeTTL)
	for k, e := range g.entries {
		if e.updated.Before(cutoff) {
			delete(g.entries, k)
		}
	}
}

// evictOldestSettledLocked drops the oldest entry that is NOT running and
// reports whether there was one. A running entry is never evicted: its key
// is still in use by an upload, and dropping it would let the key begin again.
func (g *importOutcomeRegistry) evictOldestSettledLocked() bool {
	var oldest importOutcomeKey
	var at time.Time
	found := false
	for k, e := range g.entries {
		if e.State == importStateRunning {
			continue
		}
		if !found || e.updated.Before(at) {
			oldest, at, found = k, e.updated, true
		}
	}
	if found {
		delete(g.entries, oldest)
	}
	return found
}

func (s *Server) importOutcomesRegistry() *importOutcomeRegistry {
	s.importOutcomesOnce.Do(func() {
		if s.importOutcomes == nil {
			s.importOutcomes = newImportOutcomeRegistry()
		}
	})
	return s.importOutcomes
}

// handleImportStatus answers GET /workspaces/import-status?key=: the outcome
// of the caller's own import attempt. 404 when this server holds no such key
// for the caller (never started here, another user's, expired, or lost to a
// restart): the client reads that as UNKNOWN.
func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	key := r.URL.Query().Get("key")
	if !importKeyPattern.MatchString(key) {
		writeError(w, http.StatusBadRequest, "validation_error", "key must be 8-64 letters, digits or hyphens")
		return
	}
	e, ok := s.importOutcomesRegistry().get(user.ID, key)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No import with that key is known to this server")
		return
	}
	writeJSON(w, http.StatusOK, e)
}
