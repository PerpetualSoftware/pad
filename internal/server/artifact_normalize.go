package server

import (
	"encoding/json"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// normalizedArtifact is what the importer stores for an artifact: the item's
// title, content and fields after the forgiving preprocess, and the warnings
// that preprocess produced.
type normalizedArtifact struct {
	Title   string
	Content string
	// FieldsJSON is the stored fields blob.
	FieldsJSON string
	// Fields is FieldsJSON decoded back into canonical JSON types ([]any,
	// map[string]any), the shape the create path validates.
	Fields   map[string]any
	Warnings []string
}

// normalizeArtifact is the artifact importer's preprocess, shared by the human
// import (handleImportArtifact) and the app installer's preview and
// provisioning (SPEC-6 U8, DOC-3371 §2 step 3), so the item an owner reviews
// is the item that is stored. It never mutates art.
//
// Its output depends on the destination: select values the schema does not
// offer are cleared, and a playbook's invocation_slug is de-collided against
// slugTaken. That is why provisioning re-runs it on its own transaction and
// compares the digest with the one reviewed.
//
// The caller validates the title (models.ValidateItemTitle) first; a title
// that fails there must not reach this function.
func normalizeArtifact(art artifact.Artifact, coll *models.Collection, schema models.CollectionSchema, slugTaken func(string) (bool, error)) (*normalizedArtifact, error) {
	// Copy the artifact fields so the preprocess never mutates the decoded
	// value (keeps the parse layer's output immutable from the caller's POV).
	fields := make(map[string]any, len(art.Fields))
	for k, v := range art.Fields {
		fields[k] = v
	}
	warnings := preprocessArtifactFields(art.Kind, schema, fields, slugTaken)

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("marshal fields: %w", err)
	}
	// Normalize the field map through JSON so its nested types match what the
	// normal create path validates. handleCreateItem builds its fieldMap by
	// unmarshalling the JSON request body, so structured values are canonical
	// JSON types ([]any, map[string]any). The artifact decode produces Go-native
	// types (e.g. arguments as []map[string]any), which ValidateFields' json case
	// rejects — round-tripping fixes that without special-casing any field.
	normalized := make(map[string]any)
	if err := json.Unmarshal(fieldsJSON, &normalized); err != nil {
		return nil, fmt.Errorf("normalize fields: %w", err)
	}

	body := art.Body
	if footer := artifactProvenanceFooter(art.Provenance); footer != "" {
		body = body + footer
	}
	return &normalizedArtifact{
		Title:      models.NormalizeItemTitle(art.Title),
		Content:    body,
		FieldsJSON: string(fieldsJSON),
		Fields:     normalized,
		Warnings:   warnings,
	}, nil
}
