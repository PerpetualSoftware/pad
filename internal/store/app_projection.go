package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The app-projection block (SPEC-6 v12 §5, TASK-3389).
//
// A connected app's webhook payload must describe an event as it was WHEN IT
// HAPPENED, built from nothing but what the mutation's transaction froze. The
// stored snapshot alone cannot do that: it carries the creator's KIND but not
// their account or name, raw `fields` but not the schema they must be
// projected by, and a comment snapshot carries neither its item's collection
// nor its parent's item. A later lookup would describe a later state, or find
// a parent comment the delete already reaped.
//
// So every single-subject item and comment event freezes an `app_projection`
// block beside its snapshot, computed on the mutation's own transaction. The
// app dispatcher (SPEC-6 U5) builds app DTOs from this block alone.
//
// The block is written for every event today. SPEC-6 writes it only for
// workspaces with an installed app, and the gate lands with the installs table
// (DOC-3371 decomposition, U4/U5). It never reaches an owner webhook: the
// outbox drain strips it first (stripAppProjection).
//
// A data-shape problem never fails the mutation (lead ruling on #1763): an
// unparseable schema, a non-object fields blob, or a field type this build
// does not know degrades the item block to `fields` omitted plus
// `partial: true`, logged once per collection. Only database errors
// propagate. The dispatcher treats a partial block as ref-only.
//
// Identity in the block is erased on account deletion: ScrubOutboxUserRefsTx
// drops creator.user_id like any other frozen user id, and also the
// creator.display beside it, which no generic key match could tie to the
// deleted account.

const appProjectionVersion = 1

// appProjectionKey is the payload key the block lives under.
const appProjectionKey = "app_projection"

type appProjectionCreator struct {
	UserID  string `json:"user_id,omitempty"`
	Display string `json:"display,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// itemAppProjection is the block on an item event.
type itemAppProjection struct {
	V            int                  `json:"v"`
	CollectionID string               `json:"collection_id"`
	Creator      appProjectionCreator `json:"creator"`
	// Fields holds the item's field values projected by the EVENT-TIME
	// schema: declared keys only, never a relation or multi_relation. It is
	// omitted, and Partial set, when the schema or the blob could not be read.
	// A pointer, so a clean block with nothing to project still carries
	// "fields": {} and only a partial block omits the key.
	Fields  *map[string]any `json:"fields,omitempty"`
	Partial bool            `json:"partial,omitempty"`
}

// appProjectionFieldTypes is every field type validateFieldType
// (internal/items/validate.go) knows. A schema declaring any other type
// projects partial: this build cannot say what its values mean.
var appProjectionFieldTypes = map[string]bool{
	"text": true, "url": true, "number": true, "checkbox": true, "date": true,
	"select": true, "multi_select": true, "relation": true, "multi_relation": true,
	"json": true,
}

// appProjectionWarned holds the collection ids already logged as partial, so a
// malformed collection logs once per process rather than once per write. It is
// capped: past appProjectionWarnCap distinct collections, further ones log on
// every write instead of growing the set.
var (
	appProjectionWarned    sync.Map
	appProjectionWarnCount atomic.Int64
)

const appProjectionWarnCap = 10000

func warnPartialAppProjection(collectionID, reason string) {
	if _, seen := appProjectionWarned.Load(collectionID); seen {
		return
	}
	if appProjectionWarnCount.Load() < appProjectionWarnCap {
		if _, seen := appProjectionWarned.LoadOrStore(collectionID, true); seen {
			return
		}
		appProjectionWarnCount.Add(1)
	}
	slog.Warn("app projection: partial block, fields omitted", "collection_id", collectionID, "reason", reason)
}

// commentAppProjection is the block on a comment event, deletion included.
type commentAppProjection struct {
	V            int                  `json:"v"`
	CollectionID string               `json:"collection_id"`
	Creator      appProjectionCreator `json:"creator"`
	ItemID       string               `json:"item_id"`
	// ParentCommentID is set only when the parent is a comment on the SAME
	// item. Legacy rows can point across items, and the foreign key does not
	// prevent it; such a parent projects to absent.
	ParentCommentID string `json:"parent_comment_id,omitempty"`
}

// userDisplayTx returns a user's display name, or "" when the account is gone.
func (s *Store) userDisplayTx(tx *sql.Tx, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}
	var name string
	err := tx.QueryRow(s.q(`SELECT name FROM users WHERE id = ?`), userID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("app projection: read user: %w", err)
	}
	return name, nil
}

// projectFieldsBySchema keeps only the keys the schema declares, minus
// relation and multi_relation fields. Numbers keep their literal form. It never
// fails: a data-shape problem returns a non-empty reason, and the caller marks
// the block partial.
func projectFieldsBySchema(fieldsJSON, schemaJSON string) (map[string]any, string) {
	// Item-field reasoning reads the schema through the item-field decoder,
	// which strips grandfathered reserved-key declarations (BUG-2685).
	var schema models.CollectionSchema
	if schemaJSON != "" {
		if err := models.UnmarshalItemFieldSchema([]byte(schemaJSON), &schema); err != nil {
			return nil, "schema does not parse: " + err.Error()
		}
	}
	allowed := make(map[string]bool, len(schema.Fields))
	for _, fd := range schema.Fields {
		if !appProjectionFieldTypes[fd.Type] {
			return nil, fmt.Sprintf("field %q has unknown type %q", fd.Key, fd.Type)
		}
		if fd.Type == "relation" || fd.IsMultiRelation() {
			continue
		}
		allowed[fd.Key] = true
	}
	out := map[string]any{}
	if fieldsJSON == "" {
		return out, ""
	}
	// A stored blob that is not a JSON object is legacy corruption BUG-3163
	// repairs; the repair write goes through this path.
	dec := json.NewDecoder(bytes.NewReader([]byte(fieldsJSON)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, "fields blob does not parse"
	}
	// One value and nothing after it: a single Decode stops at the end of the
	// first value, so trailing bytes would otherwise project as clean.
	if _, err := dec.Token(); err != io.EOF {
		return nil, "fields blob has trailing data"
	}
	raw, ok := doc.(map[string]any)
	if !ok {
		return nil, "fields blob is not an object"
	}
	for k, v := range raw {
		if allowed[k] {
			out[k] = v
		}
	}
	return out, ""
}

// buildItemAppProjectionTx freezes the block for an item event, reading the
// item's creator and its collection's schema on the mutation's transaction.
func (s *Store) buildItemAppProjectionTx(tx *sql.Tx, item *models.Item) (*itemAppProjection, error) {
	var creatorID sql.NullString
	err := tx.QueryRow(s.q(`SELECT created_by_user_id FROM items WHERE id = ?`), item.ID).Scan(&creatorID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("app projection: read item creator: %w", err)
	}
	display, err := s.userDisplayTx(tx, creatorID.String)
	if err != nil {
		return nil, err
	}
	// Only the schema is needed, so only the schema is read: hydrating the
	// whole collection row failed on legal NULL columns (codex round 1).
	var schemaCol sql.NullString
	err = tx.QueryRow(s.q(`SELECT schema FROM collections WHERE id = ?`), item.CollectionID).Scan(&schemaCol)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("app projection: read collection schema: %w", err)
	}
	schema := schemaCol.String
	p := &itemAppProjection{
		V:            appProjectionVersion,
		CollectionID: item.CollectionID,
		Creator:      appProjectionCreator{UserID: creatorID.String, Display: display, Kind: item.CreatedBy},
	}
	fields, reason := projectFieldsBySchema(item.Fields, schema)
	if reason != "" {
		warnPartialAppProjection(item.CollectionID, reason)
		p.Partial = true
		return p, nil
	}
	p.Fields = &fields
	return p, nil
}

// buildCommentAppProjectionTx freezes the block for a comment event. author is
// the comment's own author column, the display used when the account is gone.
func (s *Store) buildCommentAppProjectionTx(tx *sql.Tx, itemID, userID, author, kind, parentID string) (*commentAppProjection, error) {
	var collectionID string
	err := tx.QueryRow(s.q(`SELECT collection_id FROM items WHERE id = ?`), itemID).Scan(&collectionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("app projection: read comment item: %w", err)
	}
	display, err := s.userDisplayTx(tx, userID)
	if err != nil {
		return nil, err
	}
	if display == "" {
		display = author
	}
	p := &commentAppProjection{
		V:            appProjectionVersion,
		CollectionID: collectionID,
		Creator:      appProjectionCreator{UserID: userID, Display: display, Kind: kind},
		ItemID:       itemID,
	}
	if parentID != "" {
		var parentItemID string
		err := tx.QueryRow(s.q(`SELECT item_id FROM comments WHERE id = ?`), parentID).Scan(&parentItemID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("app projection: read parent comment: %w", err)
		}
		if err == nil && parentItemID == itemID {
			p.ParentCommentID = parentID
		}
	}
	return p, nil
}

// stripAppProjection removes every app_projection key from an outbox payload,
// at any depth, so folded bulk members are covered too. Owner webhooks get the
// payload without it: the block is for app delivery only, and it carries a
// creator display name that the item snapshot itself deliberately does not
// (scrubItemPII). A payload without the key is returned unchanged.
func stripAppProjection(payload []byte) ([]byte, error) {
	if !bytes.Contains(payload, []byte(`"`+appProjectionKey+`"`)) {
		return payload, nil
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("outbox: parse payload to strip app projection: %w", err)
	}
	if !stripAppProjectionNode(doc) {
		return payload, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("outbox: re-marshal stripped payload: %w", err)
	}
	return out, nil
}

func stripAppProjectionNode(node any) bool {
	changed := false
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v[appProjectionKey]; ok {
			delete(v, appProjectionKey)
			changed = true
		}
		for _, val := range v {
			if stripAppProjectionNode(val) {
				changed = true
			}
		}
	case []any:
		for _, val := range v {
			if stripAppProjectionNode(val) {
				changed = true
			}
		}
	}
	return changed
}

// StripAppProjection is stripAppProjection for the outbox drain, which builds
// owner-webhook deliveries outside this package.
func StripAppProjection(payload []byte) ([]byte, error) { return stripAppProjection(payload) }
