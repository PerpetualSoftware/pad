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

// kindForCollectionSlug maps a collection slug to the artifact Kind it
// exports as. Only the playbooks and conventions collections carry the
// structured frontmatter the artifact format understands; everything else
// is not exportable as an artifact. Returns (kind, true) for a supported
// slug, ("", false) otherwise.
func kindForCollectionSlug(slug string) (artifact.Kind, bool) {
	switch slug {
	case "playbooks":
		return artifact.KindPlaybook, true
	case "conventions":
		return artifact.KindConvention, true
	default:
		return "", false
	}
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
	kind, ok := kindForCollectionSlug(coll.Slug)
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported_collection",
			"Only playbooks and conventions can be exported as artifacts")
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
