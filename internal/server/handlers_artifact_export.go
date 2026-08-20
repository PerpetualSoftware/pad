package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// kindForCollection resolves the artifact Kind a collection exports as from
// its artifact_kind trait (SPEC-5 §Collection traits). Returns (kind, true)
// when the collection declares one, ("", false) otherwise — a collection that
// declares no artifact kind is not exportable as an artifact.
//
// Replaces a slug switch on "playbooks"/"conventions". The switch broke as
// soon as either collection was renamed, silently making its items
// unexportable ([[BUG-2702]]); the trait travels with the collection.
// TASK-2657.
// The returned string is a human-facing reason when ok is false: the two
// refusals are different facts and an operator debugging an export needs to
// know WHICH. Collapsing them was the round-3 finding — a collection that
// declares `widget` was told it declares nothing at all.
func kindForCollection(coll *models.Collection) (artifact.Kind, bool, string) {
	if coll == nil {
		return "", false, "Collection not found"
	}
	traits, err := models.ParseCollectionTraits(coll.Traits)
	if err != nil {
		return "", false, "This collection's trait declarations could not be read, so its artifact kind is unknown"
	}
	if traits.ArtifactKind == nil || traits.ArtifactKind.Kind == "" {
		return "", false, "This collection does not declare an artifact kind, so its items cannot be exported as artifacts"
	}
	kind := artifact.Kind(traits.ArtifactKind.Kind)
	// The declared kind must be one the artifact format actually knows how to
	// serialize. SPEC-5 permits a collection to declare any kind string —
	// unknown kinds are legal, they simply don't round-trip — so this is not
	// rejected at declaration time. But without this check an unknown kind
	// would sail through to artifact.Encode, which returns ErrUnknownKind and
	// gets reported as a 500. A collection whose kind this build can't encode
	// is "not exportable as an artifact", which is a 400, exactly like a
	// collection that declares no kind at all.
	if _, err := artifact.FieldKeysForKind(kind); err != nil {
		return "", false, fmt.Sprintf("This collection declares artifact kind %q, which this version of Pad cannot export", kind)
	}
	return kind, true, ""
}

// handleExportItemArtifact serializes a single playbook or convention item to
// the portable artifact form (Markdown body + YAML frontmatter) and returns it
// as a downloadable attachment.
//
// Auth: per-item visibility only (requireItemVisible) — a viewer who can see
// the item may export it. This is deliberately NOT the workspace-export owner
// gate; an artifact is a single item the requester already has read access to.
func (s *Server) handleExportItemArtifact(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItemIncludeDeleted(ws.ID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if !s.requireItemVisible(w, r, ws.ID, item) {
		return
	}

	// Resolve the item's collection so we can map it to an artifact kind.
	coll, err := s.store.GetCollection(item.CollectionID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeInternalError(w, fmt.Errorf("export: item %s has no collection", item.Slug))
		return
	}
	kind, ok, reason := kindForCollection(coll)
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported_collection", reason)
		return
	}

	// Parse the item's structured fields into a map for the artifact.
	fields := map[string]any{}
	if item.Fields != "" {
		if err := json.Unmarshal([]byte(item.Fields), &fields); err != nil {
			writeInternalError(w, fmt.Errorf("export: parse item fields: %w", err))
			return
		}
	}

	author := ""
	if u := currentUser(r); u != nil {
		if u.Name != "" {
			author = u.Name
		} else {
			author = u.Email
		}
	}

	art := artifact.Artifact{
		Kind:          kind,
		FormatVersion: artifact.FormatVersion,
		Title:         item.Title,
		Fields:        fields,
		Body:          item.Content,
		Provenance: artifact.Provenance{
			Workspace:     ws.Slug,
			ExportedAt:    time.Now().UTC().Format(time.RFC3339),
			Author:        author,
			FormatVersion: artifact.FormatVersion,
		},
	}

	out, err := artifact.Encode(art)
	if err != nil {
		writeInternalError(w, fmt.Errorf("export: encode artifact: %w", err))
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", artifactExportFilename(item)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// artifactExportFilename builds the download filename for an exported item:
// "<slug>.pad.md".
func artifactExportFilename(item *models.Item) string {
	return item.Slug + ".pad.md"
}
