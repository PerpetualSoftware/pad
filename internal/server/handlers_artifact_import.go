package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// artifactImportResponse is the JSON body returned by a successful import.
type artifactImportResponse struct {
	Ref      string   `json:"ref"`
	Slug     string   `json:"slug"`
	Warnings []string `json:"warnings"`
}

// collectionIDForKind resolves an artifact Kind to the destination collection
// in a workspace: whichever collection DECLARES that kind via its
// artifact_kind trait (SPEC-5 §Collection traits).
//
// Replaces a hardcoded kind→slug map. The map assumed the destination was
// always named "playbooks"/"conventions", so importing into a workspace that
// had renamed either collection 404'd even though the collection was right
// there ([[BUG-2702]]). TASK-2657.
//
// visibleCollIDs restricts the candidates to collections the caller can see;
// nil means an unrestricted caller. Filtering BEFORE selection matters for the
// same reason it does in resolvePlaybook: with more than one collection
// declaring a kind, picking the first and then failing the visibility check
// lets a hidden collection shadow a visible one, so an import would be refused
// even though a destination the caller can write to exists. Codex round 2.
//
// Returns ("", nil) when no visible collection declares the kind — the caller
// reports that as a workspace with nowhere to put this artifact.
func (s *Server) collectionIDForKind(workspaceID string, k artifact.Kind, visibleCollIDs []string) (string, error) {
	traited, err := s.store.ListTraitedCollections(workspaceID)
	if err != nil {
		return "", err
	}
	candidates := traited
	if visibleCollIDs != nil {
		candidates = traited[:0:0]
		for _, c := range traited {
			if isCollectionVisible(c.ID, visibleCollIDs) {
				candidates = append(candidates, c)
			}
		}
	}
	if c := collections.FindByArtifactKind(candidates, string(k)); c != nil {
		return c.ID, nil
	}
	return "", nil
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

	// Require a well-formed title, matching handleCreateItem. artifact.Decode
	// tolerates a missing title (producing an "untitled" item), so an import
	// must not diverge from the normal create path's validation.
	//
	// BUG-2833: this comment used to claim it matched handleCreateItem's gate
	// while testing strings.TrimSpace(...) == "" against a create path that
	// tested art.Title == "" exactly — so a title of "   " was refused HERE and
	// accepted THERE, and the comment asserting they agreed is what stopped
	// anyone noticing. Both now call the same models pair, so the claim is
	// enforced rather than asserted; create adopted this door's trim, which is
	// the stricter and correct reading.
	art.Title = models.NormalizeItemTitle(art.Title)
	if msg := models.ValidateItemTitle(art.Title); msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}

	importVisibleIDs, ivErr := s.visibleCollectionIDs(r, workspaceID)
	if ivErr != nil {
		writeInternalError(w, ivErr)
		return
	}
	collID, err := s.collectionIDForKind(workspaceID, art.Kind, importVisibleIDs)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if collID == "" {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("This workspace has no collection that accepts %q artifacts", art.Kind))
		return
	}
	coll, err := s.store.GetCollection(collID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("This workspace has no collection that accepts %q artifacts", art.Kind))
		return
	}

	// Edit permission (grant-aware for guests) + collection visibility,
	// mirroring handleCreateItem's write gate.
	if !s.requireEditPermission(w, r, workspaceID, "", coll.ID) {
		return
	}
	// Belt-and-braces: collectionIDForKind already selected from the visible
	// set, so this cannot fail for a restricted caller. Kept because the
	// resolution above may change and this is the gate handleCreateItem
	// applies; it reuses the same set rather than re-querying.
	if !isCollectionVisible(coll.ID, importVisibleIDs) {
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
	// Attribution: a normal agent/api create. CreatedBy and Source are both
	// left blank so createItemChecked stamps them from the request auth
	// context, matching every other create path.
	//
	// This used to set CreatedBy to the user's UUID, which is the wrong
	// DOMAIN for the field, not just the wrong value: created_by holds the
	// role — "user" or "agent" — and consumers compare it to those literals
	// (TimelineVersionCard.svelte). An imported item
	// therefore matched neither and rendered as neither. Found while fixing
	// BUG-2542; it also would have defeated that fix here, since a non-empty
	// CreatedBy suppresses the actor stamp. The user's identity is already
	// carried by the items.created_by_user_id column, which no create path
	// currently populates — separate gap, not widened into this change.

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

	// Carry the create's own warnings into the import response (BUG-2850).
	//
	// CORRECTION. Round 10 removed this merge as unreachable, on the grounds
	// that artifact.Decode populates Fields only from FieldKeysForKind, so no
	// undeclared key could arrive. That check was real but it was the WRONG
	// SIDE of the comparison: UndeclaredFieldKeys compares the field map
	// against the DESTINATION COLLECTION'S SCHEMA, not against the artifact
	// format's key list. The destination schema is editable, so a canonical
	// artifact key can be undeclared THERE while being perfectly legal in the
	// artifact.
	//
	// Reachable and verified (round 11, and pinned by
	// TestImportArtifactReportsUndeclaredFieldsAgainstANarrowedSchema):
	// narrow the conventions collection's schema to declare only `status`,
	// import a convention carrying trigger/scope/priority — all three are
	// stored in the blob and UndeclaredFieldKeys names all three, so
	// createItemChecked sets item.Warnings and this handler dropped them.
	//
	// An import is exactly where that matters: the forgiving preprocessing
	// above exists because a foreign artifact's vocabulary may not match the
	// destination, and a key that survived into the blob unrecognized is the
	// same class of news as the values this handler already reports.
	if item.Warnings != nil {
		for _, key := range item.Warnings.UndeclaredFields {
			warnings = append(warnings, fmt.Sprintf("field %q is not declared by the destination collection's schema; stored as-is", key))
		}
		// The OTHER half of the same struct (TASK-2878, codex round 8).
		// Enumerating one member of a warnings struct and forwarding it is a
		// gap that widens every time the struct grows: `DroppedFields` was
		// added and this loop kept reporting only what it already knew. An
		// import discarding a value silently is the same class of news as one
		// storing an unrecognized key, and MORE so — that value is gone.
		for _, key := range item.Warnings.DroppedFields {
			warnings = append(warnings, fmt.Sprintf("field %q was discarded: the destination collection's schema declares a default for it that is not a valid reference", key))
		}
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
	case errors.Is(err, ErrArtifactUnbindableText):
		// "character", not "byte": the same refusal fires for a NUL
		// manufactured by a YAML escape during parsing, where no raw NUL
		// byte exists in the body (codex closing round 4).
		writeError(w, http.StatusBadRequest, "invalid_body",
			"Artifact body contains invalid UTF-8 or a NUL character")
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
