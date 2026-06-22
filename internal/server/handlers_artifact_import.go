package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// artifactImportResponse is the JSON body returned by a successful import.
type artifactImportResponse struct {
	Ref      string   `json:"ref"`
	Slug     string   `json:"slug"`
	Warnings []string `json:"warnings"`
}

// collectionSlugForKind maps an artifact Kind to the destination collection
// slug in a workspace.
func collectionSlugForKind(k artifact.Kind) (string, bool) {
	switch k {
	case artifact.KindPlaybook:
		return "playbooks", true
	case artifact.KindConvention:
		return "conventions", true
	default:
		return "", false
	}
}

// handleImportArtifact imports a single playbook/convention artifact (Markdown
// body + YAML frontmatter) into the workspace's playbooks/conventions
// collection.
//
// Auth: editor+ (item-create permission) plus destination-collection
// visibility — the same gate handleCreateItem applies to a write.
//
// Pipeline: guarded safe-parse (parseArtifactRequest) → map Kind to the
// destination collection → forgiving preprocess (blank foreign select values,
// force status=draft, de-collide invocation_slug) → createItemChecked.
func (s *Server) handleImportArtifact(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Safe-parse first so a malformed/oversized/bomb body is rejected before
	// we touch the store.
	art, err := parseArtifactRequest(w, r, s.importArtifactMaxBytes)
	if err != nil {
		writeArtifactParseError(w, err)
		return
	}

	// Require a title, matching handleCreateItem's "Title is required" gate.
	// artifact.Decode tolerates a missing title (producing an "untitled"
	// item), so we enforce it here so an import can't diverge from the normal
	// create path's validation.
	if strings.TrimSpace(art.Title) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Title is required")
		return
	}

	collSlug, ok := collectionSlugForKind(art.Kind)
	if !ok {
		// Decode already validates the kind, so this is defensive.
		writeError(w, http.StatusBadRequest, "unknown_kind", "Unknown artifact kind")
		return
	}

	coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("This workspace has no %q collection to import into", collSlug))
		return
	}

	// Edit permission (grant-aware for guests) + collection visibility,
	// mirroring handleCreateItem's write gate.
	if !s.requireEditPermission(w, r, workspaceID, "", coll.ID) {
		return
	}
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	if !isCollectionVisible(coll.ID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to parse collection schema")
		return
	}

	// Copy the artifact fields so the preprocess never mutates the decoded
	// value (keeps the parse layer's output immutable from the handler's POV).
	fields := make(map[string]any, len(art.Fields))
	for k, v := range art.Fields {
		fields[k] = v
	}

	warnings := s.preprocessArtifactFields(workspaceID, coll, art.Kind, schema, fields)

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to marshal fields")
		return
	}

	// Normalize the field map through JSON so its nested types match what the
	// normal create path validates. handleCreateItem builds its fieldMap by
	// unmarshalling the JSON request body, so structured values are canonical
	// JSON types ([]any, map[string]any). The artifact decode produces Go-native
	// types (e.g. arguments as []map[string]any), which ValidateFields' json case
	// rejects — round-tripping fixes that without special-casing any field.
	normalizedFields := make(map[string]any)
	if err := json.Unmarshal(fieldsJSON, &normalizedFields); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to normalize fields")
		return
	}

	body := art.Body
	if footer := artifactProvenanceFooter(art.Provenance); footer != "" {
		body = body + footer
	}

	input := models.ItemCreate{
		Title:   art.Title,
		Content: body,
		Fields:  string(fieldsJSON),
	}
	// Attribution: a normal agent/api create. Source is left blank here so
	// createItemChecked stamps it from the request auth context (cli/web),
	// matching every other create path.
	if u := currentUser(r); u != nil {
		input.CreatedBy = u.ID
	}

	// Enforce the workspace item-count limit (workspace-scoped), identical to
	// handleCreateItem. Writes the 403 plan_limit_exceeded response itself when
	// the cap is hit; no-op in self-hosted mode.
	if !s.enforcePlanLimit(w, workspaceID, "items_per_workspace") {
		return
	}

	item, cerr := s.createItemChecked(r, workspaceID, coll, schema, input, normalizedFields, "")
	if cerr != nil {
		writeError(w, cerr.status, cerr.code, cerr.message)
		return
	}

	writeJSON(w, http.StatusCreated, artifactImportResponse{
		Ref:      item.Ref,
		Slug:     item.Slug,
		Warnings: warnings,
	})
}

// preprocessArtifactFields applies the forgiving import coercions, mutating
// fields in place and returning the accumulated warnings:
//
//   - Any select field whose imported value is not among the destination
//     schema's options is blanked (so the create's strict ValidateFields
//     doesn't 400 the whole import on a foreign vocabulary value).
//   - status is forced to "draft" regardless of the artifact's value.
//   - For playbooks, a non-empty invocation_slug that's already taken in the
//     destination collection is suffixed (-2, -3, …) until free.
func (s *Server) preprocessArtifactFields(workspaceID string, coll *models.Collection, kind artifact.Kind, schema models.CollectionSchema, fields map[string]any) []string {
	var warnings []string

	// Blank foreign select values (trigger/scope/priority and any other
	// select field the artifact carried).
	for _, def := range schema.Fields {
		if def.Type != "select" {
			continue
		}
		raw, ok := fields[def.Key]
		if !ok || raw == nil {
			continue
		}
		val, ok := raw.(string)
		if !ok || val == "" {
			continue
		}
		if !optionAllowed(def.Options, val) {
			fields[def.Key] = ""
			warnings = append(warnings,
				fmt.Sprintf("field %q value %q is not a valid option in this workspace; cleared on import", def.Key, val))
		}
	}

	// Force status=draft. The artifact may have been exported as active; an
	// import should never silently activate a playbook/convention.
	if cur, _ := fields["status"].(string); cur != "draft" {
		fields["status"] = "draft"
		if cur != "" {
			warnings = append(warnings,
				fmt.Sprintf("status %q reset to \"draft\" on import", cur))
		}
	}

	// De-collide invocation_slug for playbooks.
	if kind == artifact.KindPlaybook {
		if slug, _ := fields["invocation_slug"].(string); slug != "" {
			free, changed := s.freeInvocationSlug(workspaceID, coll.ID, slug)
			if changed {
				fields["invocation_slug"] = free
				warnings = append(warnings,
					fmt.Sprintf("invocation_slug %q was already in use; imported as %q", slug, free))
			}
		}
	}

	return warnings
}

// freeInvocationSlug returns an invocation_slug that's free in the destination
// collection. If the requested slug is already taken it appends -2, -3, …
// until an unused value is found. Returns (slug, changed).
func (s *Server) freeInvocationSlug(workspaceID, collectionID, requested string) (string, bool) {
	candidate := requested
	for n := 2; ; n++ {
		taken, err := s.invocationSlugTaken(workspaceID, collectionID, candidate)
		if err != nil {
			// On a query error, fall back to the requested slug and let the
			// create-time uniqueness precheck/constraint surface a conflict.
			return requested, candidate != requested
		}
		if !taken {
			return candidate, candidate != requested
		}
		candidate = fmt.Sprintf("%s-%d", requested, n)
	}
}

// invocationSlugTaken reports whether an item with the given invocation_slug
// already exists (non-archived) in the destination collection.
func (s *Server) invocationSlugTaken(workspaceID, collectionID, slug string) (bool, error) {
	existing, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionIDs: []string{collectionID},
		Fields:        map[string]string{"invocation_slug": slug},
		Limit:         1,
	})
	if err != nil {
		return false, err
	}
	return len(existing) > 0, nil
}

// optionAllowed reports whether val is among options. An empty options list
// means the field is unconstrained (any value allowed).
func optionAllowed(options []string, val string) bool {
	if len(options) == 0 {
		return true
	}
	for _, o := range options {
		if o == val {
			return true
		}
	}
	return false
}

// artifactProvenanceFooter renders an optional Markdown footer recording where
// an imported artifact came from. Returns "" when there's nothing useful to
// record.
func artifactProvenanceFooter(p artifact.Provenance) string {
	if p.Workspace == "" && p.Author == "" && p.ExportedAt == "" {
		return ""
	}
	footer := "\n\n---\n\n_Imported artifact"
	if p.Workspace != "" {
		footer += fmt.Sprintf(" from workspace `%s`", p.Workspace)
	}
	if p.Author != "" {
		footer += fmt.Sprintf(", exported by %s", p.Author)
	}
	if p.ExportedAt != "" {
		footer += fmt.Sprintf(" at %s", p.ExportedAt)
	}
	footer += "._\n"
	return footer
}

// writeArtifactParseError maps the typed errors from parseArtifactRequest to
// HTTP responses.
func writeArtifactParseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrArtifactTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			"Artifact body exceeds the size limit")
	case errors.Is(err, ErrArtifactUnsafeYAML):
		writeError(w, http.StatusBadRequest, "unsafe_yaml",
			"Artifact frontmatter was rejected by the import safety limits")
	case errors.Is(err, artifact.ErrMalformed):
		writeError(w, http.StatusBadRequest, "malformed_artifact", err.Error())
	case errors.Is(err, artifact.ErrUnknownKind):
		writeError(w, http.StatusBadRequest, "unknown_kind", err.Error())
	case errors.Is(err, artifact.ErrUnsupportedVersion):
		writeError(w, http.StatusBadRequest, "unsupported_version", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "Could not parse artifact: "+err.Error())
	}
}
