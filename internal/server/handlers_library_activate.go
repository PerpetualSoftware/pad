package server

import (
	"fmt"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// libraryActivateRequest names a library entry by its key or, as every client
// did before keys existed, by its title. Conventions are looked up first, as
// the CLI and MCP doors always did.
type libraryActivateRequest struct {
	Key   string `json:"key,omitempty"`
	Title string `json:"title,omitempty"`
}

// libraryEntryToActivate is a resolved library entry: what to create, and
// where.
type libraryEntryToActivate struct {
	key        string
	title      string
	content    string
	fields     string
	convention *models.ItemConventionMetadata
	kind       artifact.Kind
	fallback   string
}

func resolveLibraryEntry(req libraryActivateRequest) *libraryEntryToActivate {
	for _, cat := range collections.ConventionLibrary() {
		for _, c := range cat.Conventions {
			if (req.Key != "" && c.Key == req.Key) || (req.Key == "" && c.Title == req.Title) {
				return &libraryEntryToActivate{key: c.Key, title: c.Title, content: c.Content,
					fields: collections.LibraryConventionFields(c), convention: collections.LibraryConventionMetadata(c),
					kind: artifact.KindConvention, fallback: "conventions"}
			}
		}
	}
	for _, cat := range collections.PlaybookLibrary() {
		for _, p := range cat.Playbooks {
			if (req.Key != "" && p.Key == req.Key) || (req.Key == "" && p.Title == req.Title) {
				return &libraryEntryToActivate{key: p.Key, title: p.Title, content: p.Content,
					fields: collections.LibraryPlaybookFields(p), kind: artifact.KindPlaybook, fallback: "playbooks"}
			}
		}
	}
	return nil
}

// handleActivateLibraryEntry creates an item from a convention or playbook
// library entry and records its built-in origin (TASK-3462). It is the one
// door the CLI, MCP and web activation use, so every activated copy stores
// the same fields as a template seed of the same entry, and every one can
// later be offered a fix to the entry's text.
//
// The destination is the collection that DECLARES the entry's artifact kind
// (BUG-2702), else the canonical slug. Auth is handleCreateItem's: edit
// permission on the destination plus its visibility.
//
// Activating an entry that is already active is NOT refused here, as it was
// not on the CLI or MCP doors before; a playbook's unique invocation_slug
// refuses the second one with the create door's conflict.
func (s *Server) handleActivateLibraryEntry(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	var req libraryActivateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if (req.Key == "") == (req.Title == "") {
		writeError(w, http.StatusBadRequest, "bad_request", "Name the library entry with exactly one of key or title")
		return
	}
	entry := resolveLibraryEntry(req)
	if entry == nil {
		name := req.Title
		if req.Key != "" {
			name = req.Key
		}
		// The wording handleLibraryEntry and the CLI's legacy path use, so
		// a reader matching the message (MCP stdio classifies CLI stderr by
		// prose) sees what it saw before this door existed.
		writeError(w, http.StatusNotFound, "not_found", "not found in convention or playbook library: "+name)
		return
	}
	builtin, ok := collections.LookupBuiltin(entry.key)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "Library entry has no built-in registration")
		return
	}

	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	collID, err := s.collectionIDForKind(workspaceID, entry.kind, visibleIDs)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	var coll *models.Collection
	if collID != "" {
		coll, err = s.store.GetCollection(collID)
	} else {
		// No collection declares the kind (a workspace from before the
		// artifact_kind backfill): the canonical slug, as the CLI and MCP
		// doors did. Only a SUCCESSFUL lookup reaches this fallback.
		coll, err = s.store.GetCollectionBySlug(workspaceID, entry.fallback)
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil || !isCollectionVisible(coll.ID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("This workspace has no collection that accepts %q entries", entry.kind))
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, "", coll.ID) {
		return
	}
	if !s.enforcePlanLimit(w, r, workspaceID, "items_per_workspace") {
		return
	}

	var schema models.CollectionSchema
	if err := models.UnmarshalItemFieldSchema([]byte(coll.Schema), &schema); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to parse collection schema")
		return
	}
	fieldMap := map[string]any{}
	if err := models.DecodeJSONKeepingNumbers([]byte(entry.fields), &fieldMap); err != nil {
		writeInternalError(w, err)
		return
	}
	if entry.convention != nil {
		// The typed member, as handleCreateItem lowers it.
		normalized, cerr := models.ValidateConventionMetadata(entry.convention)
		if cerr != nil {
			writeInternalError(w, cerr)
			return
		}
		fieldMap[models.ItemFieldConvention] = normalized
	}

	input := models.ItemCreate{
		Title:         entry.title,
		Content:       entry.content,
		BuiltinOrigin: &models.BuiltinOrigin{Key: entry.key, SeedHash: builtin.Hash(), SeedContent: entry.content, SeedFields: entry.fields},
	}
	item, cerr := s.createItemChecked(r, workspaceID, coll, schema, input, fieldMap, "", relationsRefuse)
	if cerr != nil {
		cerr.write(w, r)
		return
	}
	if err := s.enrichItemForResponse(r, item, visibleIDs); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}
