package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3462 U2: what an item made from a built-in convention or playbook
// stands to gain from Pad's current text, and the opt-in door that takes it.
// Nothing here ever changes an item on its own: the state is derived on read,
// and the update is a PATCH the caller asks for, guarded by the version token
// it read.

// builtinText is a built-in's text: its body and the fields an update writes.
type builtinText struct {
	Content string         `json:"content"`
	Fields  map[string]any `json:"fields"`
}

// builtinStateResponse is GET /items/{ref}/builtin.
type builtinStateResponse struct {
	Key         string `json:"key"`
	Kind        string `json:"kind,omitempty"`
	State       string `json:"state"`
	SeedHash    string `json:"seed_hash,omitempty"`
	LibraryHash string `json:"library_hash,omitempty"`
	ItemHash    string `json:"item_hash,omitempty"`
	// Library is the current library text, present whenever there is
	// something to offer (update_available, diverged, unknown_origin).
	Library *builtinText `json:"library,omitempty"`
	// Seed is the text the item was given, present when there is something
	// to offer and it is known: with Library it shows what the LIBRARY
	// changed (seed -> library), apart from what the item's user changed
	// (seed -> the item's current text, which the caller already has).
	Seed *builtinText `json:"seed,omitempty"`
	// Seq is the item's version token at the read: send it back as
	// expected_seq to take the update.
	Seq int64 `json:"seq"`
}

func builtinOffers(state string) bool {
	return state == collections.BuiltinUpdateAvailable || state == collections.BuiltinDiverged ||
		state == collections.BuiltinUnknownOrigin
}

// builtinTextOf is a stored text as a builtinText: its fields without status
// or nulls, the shape BuiltinEntry.UpdateFields gives the library's.
func builtinTextOf(content, fieldsJSON string) (*builtinText, error) {
	fields := map[string]any{}
	if fieldsJSON != "" {
		if err := models.DecodeJSONKeepingNumbers([]byte(fieldsJSON), &fields); err != nil {
			return nil, err
		}
	}
	delete(fields, "status")
	for k, v := range fields {
		if v == nil {
			delete(fields, k)
		}
	}
	return &builtinText{Content: content, Fields: fields}, nil
}

// resolveBuiltinItem loads the item in the URL, checks the caller may see it,
// and loads its origin. It writes the response and returns ok=false on any
// refusal, including an item made from no built-in (404 not_builtin).
func (s *Server) resolveBuiltinItem(w http.ResponseWriter, r *http.Request) (workspaceID string, item *models.Item, origin *models.BuiltinOrigin, ok bool) {
	workspaceID, ok = s.getWorkspaceID(w, r)
	if !ok {
		return "", nil, nil, false
	}
	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return "", nil, nil, false
	}
	if item == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return "", nil, nil, false
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return "", nil, nil, false
	}
	origin, err = s.store.GetItemBuiltinOrigin(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return "", nil, nil, false
	}
	if origin == nil {
		writeError(w, http.StatusNotFound, "not_builtin", "This item was not made from a built-in convention or playbook")
		return "", nil, nil, false
	}
	return workspaceID, item, origin, true
}

// builtinStateFor derives an item's state and the response describing it.
func builtinStateFor(item *models.Item, origin *models.BuiltinOrigin) (*builtinStateResponse, collections.BuiltinEntry, error) {
	state, itemHash, entry, err := collections.BuiltinStateOf(*origin, item.Content, item.Fields)
	if err != nil {
		return nil, entry, err
	}
	resp := &builtinStateResponse{Key: origin.Key, State: state, SeedHash: origin.SeedHash, ItemHash: itemHash, Seq: item.Seq}
	if state == collections.BuiltinUnknownEntry {
		return resp, entry, nil
	}
	resp.Kind = entry.Kind
	resp.LibraryHash = entry.Hash()
	if builtinOffers(state) {
		resp.Library = &builtinText{Content: entry.Content, Fields: entry.UpdateFields()}
		if origin.SeedHash != "" && (origin.SeedContent != "" || origin.SeedFields != "") {
			seed, err := builtinTextOf(origin.SeedContent, origin.SeedFields)
			if err != nil {
				return nil, entry, err
			}
			resp.Seed = seed
		}
	}
	return resp, entry, nil
}

// handleGetItemBuiltin: GET /workspaces/{ws}/items/{ref}/builtin. Readable by
// anyone who can read the item.
func (s *Server) handleGetItemBuiltin(w http.ResponseWriter, r *http.Request) {
	_, item, origin, ok := s.resolveBuiltinItem(w, r)
	if !ok {
		return
	}
	resp, _, err := builtinStateFor(item, origin)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// builtinListEntry is one row of GET /workspaces/{ws}/builtins.
type builtinListEntry struct {
	ItemID         string `json:"item_id"`
	Ref            string `json:"ref,omitempty"`
	Slug           string `json:"slug"`
	Title          string `json:"title"`
	CollectionSlug string `json:"collection_slug"`
	Key            string `json:"key"`
	Kind           string `json:"kind,omitempty"`
	State          string `json:"state"`
}

// handleListWorkspaceBuiltins: GET /workspaces/{ws}/builtins, the state of
// every item made from a built-in, for the library page. One query reads them
// all (a join), and visibility is one set computed once: an item is listed
// only when the caller sees its WHOLE collection, so an item-grant
// guest is never shown the titles of items it was not granted. A guest's one
// granted built-in is therefore not listed here; its item page still reads
// its own state.
func (s *Server) handleListWorkspaceBuiltins(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	rows, err := s.store.WorkspaceBuiltinItems(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// The fully visible set, once, however many collections the rows span.
	fully, err := s.fullyVisibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	out := []builtinListEntry{}
	for _, row := range rows {
		if !appCeilingAllows(r, row.CollectionID) || !isCollectionVisible(row.CollectionID, fully) {
			continue
		}
		state, _, entry, err := collections.BuiltinStateOf(row.Origin, row.Content, row.Fields)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out = append(out, builtinListEntry{
			ItemID: row.ItemID, Ref: row.Ref, Slug: row.Slug, Title: row.Title,
			CollectionSlug: row.CollectionSlug, Key: row.Origin.Key, Kind: entry.Kind, State: state,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// builtinUpdateRequest is POST /items/{ref}/builtin/update.
type builtinUpdateRequest struct {
	// ExpectedSeq is REQUIRED: the seq the caller read with the preview. The
	// update replaces the item's body, so it must be refused if the item
	// changed after the caller looked.
	ExpectedSeq *int64 `json:"expected_seq"`
	// OverwritePendingEdits lifts content_pending_flush, as on any PATCH.
	OverwritePendingEdits bool `json:"overwrite_pending_edits,omitempty"`
}

// handleBuiltinUpdate: POST /workspaces/{ws}/items/{ref}/builtin/update.
// Gives the item the library's current text: its body, and the fields an
// update writes (never status, never the title). It is an ordinary PATCH
// underneath (updateItem), so it takes every guard a PATCH takes; on success
// the item's seed moves to the library's version.
func (s *Server) handleBuiltinUpdate(w http.ResponseWriter, r *http.Request) {
	workspaceID, item, origin, ok := s.resolveBuiltinItem(w, r)
	if !ok {
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}
	var req builtinUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.ExpectedSeq == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "expected_seq is required: send the seq you read with the preview")
		return
	}
	state, _, entry, err := collections.BuiltinStateOf(*origin, item.Content, item.Fields)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if state == collections.BuiltinUnknownEntry {
		writeError(w, http.StatusConflict, "builtin_unknown",
			fmt.Sprintf("This Pad does not ship the built-in %q, so there is no text to update from", origin.Key))
		return
	}
	if !builtinOffers(state) {
		writeError(w, http.StatusConflict, "builtin_up_to_date", "This item already has the current built-in text")
		return
	}

	// The patch: every update field the library has, and a null for any the
	// item's seed had that the library no longer does, so a field the
	// library dropped is dropped. The convention metadata goes in typed.
	libFields := entry.UpdateFields()
	patch := map[string]any{}
	var convention *models.ItemConventionMetadata
	var clearConvention bool
	for k, v := range libFields {
		if k == models.ItemFieldConvention {
			b, err := json.Marshal(v)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			var meta models.ItemConventionMetadata
			if err := json.Unmarshal(b, &meta); err != nil {
				writeInternalError(w, err)
				return
			}
			if convention, err = models.ValidateConventionMetadata(&meta); err != nil {
				writeInternalError(w, err)
				return
			}
			continue
		}
		patch[k] = v
	}
	if origin.SeedFields != "" {
		seed, err := builtinTextOf("", origin.SeedFields)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		for k := range seed.Fields {
			if _, kept := libFields[k]; kept {
				continue
			}
			if k == models.ItemFieldConvention {
				// Reserved: cleared through the typed lowering, not the
				// caller-facing patch (codex r3).
				clearConvention = true
				continue
			}
			patch[k] = nil
		}
	}
	content := entry.Content
	summary := "Updated from Pad's built-in " + origin.Key
	newSeed := models.BuiltinOrigin{Key: origin.Key, SeedHash: entry.Hash(), SeedContent: entry.Content, SeedFields: entry.Fields}
	s.updateItem(w, r, &builtinItemUpdate{
		input: models.ItemUpdate{
			Content:               &content,
			FieldsPatch:           patch,
			ExpectedSeq:           req.ExpectedSeq,
			OverwritePendingEdits: req.OverwritePendingEdits,
			ChangeSummary:         summary,
			// The body it replaces goes into history even inside the
			// version throttle's window: a diverged item's edits are
			// what that history keeps.
			ForceVersion: true,
			// Recorded in the write's own transaction (codex r1).
			BuiltinSeed: &newSeed,
		},
		convention:      convention,
		clearConvention: clearConvention,
	})
}
