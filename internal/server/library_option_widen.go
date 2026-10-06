package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// libraryWidenAttempts bounds the optimistic retry when another writer moves
// the collection between our read and our write.
const libraryWidenAttempts = 3

// widenLibraryOptions adds to a system convention or playbook collection's
// select options any value this create carries that the LIBRARY itself uses
// for that field and the options do not yet list (BUG-3446). The `blank`
// template seeds those collections with one trigger and one scope on purpose,
// so before this every triggered library entry was refused there — through
// `pad library activate`, `pad_library.activate`, the web Activate button and
// the onboard playbook's own `pad item create conventions` step alike.
//
// Bounded on purpose: a value outside collections.LibraryOptionVocabulary is
// left for validation to refuse as before, so the options never become a
// free-for-all. An editor may trigger it (lead ruling, BUG-3446): the words
// are ones every non-blank template already ships. What was added is returned
// so the create can report it, in its write warnings and in its activity.
// Not on the artifact import door (relationsCarry): an import clears a
// select value the destination does not list and warns
// (preprocessArtifactFields), and keeps doing so; createItemChecked calls
// this only for an ordinary create.
//
// The caller validates the create against withSelectOptions(schema, wanted)
// BEFORE calling this, so a create the widened schema would still refuse
// writes nothing (codex round 2). The schema write goes through
// Store.UpdateCollection with the collection's own concurrency token, so it
// takes the same locks as any schema edit. It is its own transaction, ahead
// of the item insert, so a refusal after it (the plan cap at insert, a title
// the store rejects, a race lost to a unique value, or another writer's
// schema edit landing between the dry run and this write, codex round 3)
// leaves the library word listed and recorded only in the log; it is a word
// the library uses, and nothing else depends on it. The store re-validates the item under its
// lock if the schema moved again in between (BUG-3407).
func (s *Server) widenLibraryOptions(r *http.Request, workspaceID string, coll *models.Collection, schema models.CollectionSchema, fieldMap map[string]any) (*models.Collection, models.CollectionSchema, map[string][]string, *itemCreateError) {
	vocab := libraryVocabularyFor(coll)
	if len(vocab) == 0 || len(missingLibraryOptions(schema, vocab, fieldMap)) == 0 {
		return coll, schema, nil, nil
	}

	for attempt := 0; attempt < libraryWidenAttempts; attempt++ {
		fresh, err := s.store.GetCollection(coll.ID)
		if err != nil || fresh == nil {
			return coll, schema, nil, &itemCreateError{status: http.StatusInternalServerError, code: "internal_error", message: "Failed to read collection"}
		}
		var freshSchema models.CollectionSchema
		if err := models.UnmarshalItemFieldSchema([]byte(fresh.Schema), &freshSchema); err != nil {
			return coll, schema, nil, &itemCreateError{status: http.StatusInternalServerError, code: "internal_error", message: "Failed to parse collection schema"}
		}
		missing := missingLibraryOptions(freshSchema, vocab, fieldMap)
		if len(missing) == 0 {
			// Another writer added them first.
			return fresh, freshSchema, nil, nil
		}
		widened, err := appendSelectOptionsJSON(fresh.Schema, missing)
		if err != nil {
			return coll, schema, nil, &itemCreateError{status: http.StatusInternalServerError, code: "internal_error", message: "Failed to extend collection schema"}
		}
		updated, err := s.store.UpdateCollection(fresh.ID, models.CollectionUpdate{
			Schema:            &widened,
			ExpectedUpdatedAt: fresh.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
		if _, conflict := asCollectionUpdateConflictError(err); conflict {
			continue
		}
		if err != nil || updated == nil {
			slog.Error("widenLibraryOptions: UpdateCollection failed", "collection", fresh.ID, "error", err)
			return coll, schema, nil, &itemCreateError{status: http.StatusInternalServerError, code: "internal_error", message: "Failed to extend collection schema"}
		}
		var updatedSchema models.CollectionSchema
		if err := models.UnmarshalItemFieldSchema([]byte(updated.Schema), &updatedSchema); err != nil {
			return coll, schema, nil, &itemCreateError{status: http.StatusInternalServerError, code: "internal_error", message: "Failed to parse collection schema"}
		}
		s.publishCollectionEvent(events.CollectionUpdated, workspaceID, updated.ID, updated.Slug, "", false)
		slog.Info("widened library options", "workspace", workspaceID, "collection", updated.Slug, "added", missing, "actor", actorNameFromRequest(r))
		return updated, updatedSchema, missing, nil
	}
	return coll, schema, nil, &itemCreateError{status: http.StatusConflict, code: "conflict", message: fmt.Sprintf("The %s collection changed while its options were being extended; retry.", coll.Name)}
}

// libraryVocabularyFor is the library vocabulary for a SYSTEM collection
// declaring the convention or playbook artifact kind, nil for any other.
func libraryVocabularyFor(coll *models.Collection) map[string][]string {
	if coll == nil || !coll.IsSystem {
		return nil
	}
	traits, err := models.ParseCollectionTraits(coll.Traits)
	if err != nil || traits.ArtifactKind == nil {
		return nil
	}
	return collections.LibraryOptionVocabulary(traits.ArtifactKind.Kind)
}

// libraryOptionsWanted is what this create would add, read without writing.
func libraryOptionsWanted(coll *models.Collection, schema models.CollectionSchema, fieldMap map[string]any) map[string][]string {
	vocab := libraryVocabularyFor(coll)
	if len(vocab) == 0 {
		return nil
	}
	return missingLibraryOptions(schema, vocab, fieldMap)
}

// withSelectOptions returns a copy of schema with add appended to the first
// definition of each key, the in-memory twin of appendSelectOptionsJSON.
func withSelectOptions(schema models.CollectionSchema, add map[string][]string) models.CollectionSchema {
	out := models.CollectionSchema{Fields: append([]models.FieldDef(nil), schema.Fields...)}
	done := map[string]bool{}
	for i := range out.Fields {
		f := &out.Fields[i]
		if done[f.Key] || len(add[f.Key]) == 0 {
			continue
		}
		done[f.Key] = true
		opts := append([]string(nil), f.Options...)
		for _, v := range add[f.Key] {
			if !hasOption(opts, v) {
				opts = append(opts, v)
			}
		}
		f.Options = opts
	}
	return out
}

// missingLibraryOptions returns, per select field, the values fieldMap
// carries that the library vocabulary allows and the field's options lack.
func missingLibraryOptions(schema models.CollectionSchema, vocab map[string][]string, fieldMap map[string]any) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	for _, f := range schema.Fields {
		if seen[f.Key] {
			continue
		}
		seen[f.Key] = true
		if f.Type != "select" {
			continue
		}
		words, ok := vocab[f.Key]
		if !ok {
			continue
		}
		v, ok := fieldMap[f.Key].(string)
		if !ok || v == "" || hasOption(f.Options, v) || !hasOption(words, v) {
			continue
		}
		out[f.Key] = append(out[f.Key], v)
	}
	return out
}

// appendSelectOptionsJSON appends values to the named fields' options in the
// stored schema JSON, decoded generically so keys models.FieldDef does not
// declare survive the rewrite.
func appendSelectOptionsJSON(raw string, add map[string][]string) (string, error) {
	var doc map[string]any
	if err := models.DecodeJSONKeepingNumbers([]byte(raw), &doc); err != nil {
		return "", err
	}
	fields, ok := doc["fields"].([]any)
	if !ok {
		return "", fmt.Errorf("schema has no fields array")
	}
	done := map[string]bool{}
	for _, f := range fields {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		key, _ := fm["key"].(string)
		vals := add[key]
		if len(vals) == 0 || done[key] {
			continue
		}
		// The first definition of a key is the one validation reads; a
		// duplicate definition is left alone rather than given a copy.
		done[key] = true
		opts, _ := fm["options"].([]any)
		for _, v := range vals {
			present := false
			for _, o := range opts {
				if o == v {
					present = true
					break
				}
			}
			if !present {
				opts = append(opts, v)
			}
		}
		fm["options"] = opts
	}
	out, err := json.Marshal(doc)
	return string(out), err
}

// formatOptionsAdded renders the additions for an activity row, keys sorted:
// "scope: backend; trigger: on-commit".
func formatOptionsAdded(added map[string][]string) string {
	keys := make([]string, 0, len(added))
	for k := range added {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+strings.Join(added[k], ", "))
	}
	return strings.Join(parts, "; ")
}

func hasOption(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
