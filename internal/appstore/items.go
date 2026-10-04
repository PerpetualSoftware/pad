package appstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/items"
	"github.com/PerpetualSoftware/pad/internal/links"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// AppItemCreate is the only input an app item create takes (DOC-3371 §4).
// There is no parent, plan, tags, assignee, role, github_pr or convention.
type AppItemCreate struct {
	Title   string
	Content string
	Fields  map[string]any
}

// AppItemUpdate is the only input an app item update takes: a scalar fields
// patch and the etag the app last read. No content, title or relation key.
type AppItemUpdate struct {
	FieldsPatch  map[string]any
	ExpectedETag string
}

// InputError is a refusal of the request itself (400): it depends only on
// what the app sent and the companion schema, never on anything the app
// cannot see.
type InputError struct{ Reason string }

func (e *InputError) Error() string { return "app write refused: " + e.Reason }

func refuse(format string, args ...any) error {
	return &InputError{Reason: fmt.Sprintf(format, args...)}
}

// maxSlugAttempts bounds the retry on a random-suffix slug collision, which
// fails the whole transaction (on Postgres a failed statement aborts it).
const maxSlugAttempts = 3

// ItemWrite is a committed app item write and the view its response is
// built from, read in the same transaction (store.FencedTx.ItemView).
type ItemWrite struct {
	Item *models.Item
	View store.FencedItemView
	ETag string
}

// CreateItem creates an item in a companion collection, in one FencedTx.
func (a *Store) CreateItem(ctx context.Context, spec store.FenceSpec, collectionID string, in AppItemCreate, actor store.FencedActor) (*models.Item, error) {
	w, err := a.CreateItemWrite(ctx, spec, collectionID, in, actor)
	if err != nil {
		return nil, err
	}
	return w.Item, nil
}

// CreateItemWrite is CreateItem with the response view.
func (a *Store) CreateItemWrite(ctx context.Context, spec store.FenceSpec, collectionID string, in AppItemCreate, actor store.FencedActor) (*ItemWrite, error) {
	// The response carries an ETag, so a key that cannot make one refuses the
	// write before it happens rather than after.
	if err := a.etagKeyUsable(); err != nil {
		return nil, err
	}
	title := models.NormalizeItemTitle(in.Title)
	if msg := models.ValidateItemTitle(title); msg != "" {
		return nil, refuse("%s", msg)
	}
	if err := refuseReferences(in.Content); err != nil {
		return nil, err
	}
	if err := refuseWorkspaceLinks(in.Content); err != nil {
		return nil, err
	}
	fields := in.Fields
	if fields == nil {
		fields = map[string]any{}
	}
	if err := refuseReferencesInFields(fields); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < maxSlugAttempts; attempt++ {
		item, err := a.createItemOnce(ctx, spec, collectionID, title, in.Content, fields, actor)
		if err == nil || !store.IsItemSlugConflict(err) {
			return item, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (a *Store) createItemOnce(ctx context.Context, spec store.FenceSpec, collectionID, title, content string, fields map[string]any, actor store.FencedActor) (*ItemWrite, error) {
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ftx.Rollback() }()

	schema, err := companionSchema(ftx, collectionID)
	if err != nil {
		return nil, err
	}
	// A fresh copy per attempt: validation applies defaults in place.
	working := make(map[string]any, len(fields))
	for k, v := range fields {
		working[k] = v
	}
	if err := checkFieldKeys(working, schema); err != nil {
		return nil, err
	}
	working = items.CoerceFields(working, schema)
	// Validation applies schema defaults in place, AFTER the key checks
	// above, so it runs against a schema whose forbidden fields carry no
	// default: a default may not put a relation, computed or uniqueness-scoped
	// value on an app item. A required field of those kinds then cannot be
	// satisfied by an app, which is the refusal it should get.
	if err := items.ValidateFields(working, appWritableSchema(schema)); err != nil {
		return nil, refuse("%v", err)
	}
	// And a default is still content: no pad-attachment: token may arrive
	// through one either.
	if err := refuseReferencesInFields(working); err != nil {
		return nil, err
	}
	blob, err := json.Marshal(working)
	if err != nil {
		return nil, fmt.Errorf("app create: encode fields: %w", err)
	}

	item, err := ftx.CreateItem(store.FencedItemCreate{
		CollectionID: collectionID, Title: title, Content: content, Fields: string(blob), Actor: actor,
		PlanLimit: a.opts.PlanLimit,
	})
	if err != nil {
		return nil, err
	}
	view, err := ftx.ItemView(item.ID)
	if err != nil {
		return nil, err
	}
	if err := ftx.Commit(); err != nil {
		return nil, err
	}
	return &ItemWrite{Item: item, View: view, ETag: store.AppItemETag(a.opts.ETagKey, spec.InstallID, item.ID, item.Seq)}, nil
}

// UpdateItem applies a scalar fields patch to an item this install created,
// in one FencedTx.
func (a *Store) UpdateItem(ctx context.Context, spec store.FenceSpec, itemID string, in AppItemUpdate, actor store.FencedActor) (*models.Item, error) {
	w, err := a.UpdateItemWrite(ctx, spec, itemID, in, actor)
	if err != nil {
		return nil, err
	}
	return w.Item, nil
}

// UpdateItemWrite is UpdateItem with the response view.
func (a *Store) UpdateItemWrite(ctx context.Context, spec store.FenceSpec, itemID string, in AppItemUpdate, actor store.FencedActor) (*ItemWrite, error) {
	if len(in.FieldsPatch) == 0 {
		return nil, refuse("fields_patch is required")
	}
	if in.ExpectedETag == "" {
		return nil, refuse("expected_etag is required")
	}
	if err := a.etagKeyUsable(); err != nil {
		return nil, err
	}
	if err := refuseReferencesInFields(in.FieldsPatch); err != nil {
		return nil, err
	}

	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ftx.Rollback() }()

	collectionID, err := ftx.CompanionItemCollection(itemID)
	if err != nil {
		return nil, err
	}
	schema, err := companionSchema(ftx, collectionID)
	if err != nil {
		return nil, err
	}
	patch := make(map[string]any, len(in.FieldsPatch))
	for k, v := range in.FieldsPatch {
		patch[k] = v
	}
	if err := checkFieldKeys(patch, schema); err != nil {
		return nil, err
	}
	patch = items.CoerceFields(patch, schema)
	if err := items.ValidatePartialFields(patch, schema); err != nil {
		return nil, refuse("%v", err)
	}
	item, err := ftx.UpdateItemFields(itemID, patch, in.ExpectedETag, a.opts.ETagKey, actor)
	if err != nil {
		return nil, err
	}
	view, err := ftx.ItemView(itemID)
	if err != nil {
		return nil, err
	}
	if err := ftx.Commit(); err != nil {
		return nil, err
	}
	return &ItemWrite{Item: item, View: view, ETag: store.AppItemETag(a.opts.ETagKey, spec.InstallID, item.ID, item.Seq)}, nil
}

// appWritableSchema is schema with every default removed from the field
// kinds an app may not write.
func appWritableSchema(schema models.CollectionSchema) models.CollectionSchema {
	out := schema
	out.Fields = make([]models.FieldDef, len(schema.Fields))
	for i, def := range schema.Fields {
		if def.IsRelation() || def.IsMultiRelation() || def.Computed || def.UniqueScope != "" {
			def.Default = nil
		}
		out.Fields[i] = def
	}
	return out
}

func companionSchema(ftx *store.FencedTx, collectionID string) (models.CollectionSchema, error) {
	schemaJSON, _, err := ftx.CompanionCollectionSchema(collectionID)
	if err != nil {
		return models.CollectionSchema{}, err
	}
	var schema models.CollectionSchema
	if schemaJSON != "" {
		if err := models.UnmarshalItemFieldSchema([]byte(schemaJSON), &schema); err != nil {
			return models.CollectionSchema{}, fmt.Errorf("app write: companion schema: %w", err)
		}
	}
	return schema, nil
}

// checkFieldKeys refuses any key the companion schema does not declare, and
// any key an app may not write: a relation or multi-relation (an app update
// never touches relation edges), a computed field, a uniqueness-scoped field
// (v1 has no fenced uniqueness check), and the reserved metadata keys.
func checkFieldKeys(fields map[string]any, schema models.CollectionSchema) error {
	if undeclared := items.UndeclaredFieldKeys(fields, schema); len(undeclared) > 0 {
		return refuse("undeclared field(s): %s", strings.Join(undeclared, ", "))
	}
	if reserved := items.ReservedFieldKeysIn(fields); len(reserved) > 0 {
		return refuse("reserved field(s): %s", strings.Join(reserved, ", "))
	}
	for _, def := range schema.Fields {
		if _, set := fields[def.Key]; !set {
			continue
		}
		switch {
		case def.IsRelation() || def.IsMultiRelation():
			return refuse("field %q is a relation, which an app may not write", def.Key)
		case def.Computed:
			return refuse("field %q is computed", def.Key)
		case def.UniqueScope != "":
			return refuse("field %q is uniqueness-scoped, which an app may not write in v1", def.Key)
		}
	}
	return nil
}

// refuseReferences refuses ANY pad-attachment: token, whether or not the id
// exists, in any spelling: an unconditional refusal is not an oracle, and
// nothing can be bound to an item that does not exist yet (DOC-3371 §4).
func refuseReferences(text string) error {
	if strings.Contains(strings.ToLower(text), "pad-attachment:") {
		return refuse("pad-attachment: references are not accepted")
	}
	return nil
}

func refuseReferencesInFields(fields map[string]any) error {
	b, err := json.Marshal(fields)
	if err != nil {
		return refuse("fields are not valid JSON values")
	}
	return refuseReferences(string(b))
}

// refuseWorkspaceLinks refuses a workspace-qualified wiki link, [[ws::REF]],
// even one naming this workspace: an app link names only companion items.
func refuseWorkspaceLinks(content string) error {
	for _, l := range links.ExtractWikiLinks(content) {
		if l.Kind == links.WikiLinkKindWorkspaceRef {
			return refuse("workspace-qualified links are not accepted")
		}
	}
	return nil
}

// IsInputError reports whether err is a refusal of the request itself.
func IsInputError(err error) bool {
	var ie *InputError
	return errors.As(err, &ie)
}
