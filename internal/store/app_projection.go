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
// The block is written only while the workspace has an installed app, in any
// state but uninstalled (DOC-3371 §5, TASK-3392), decided on the mutation's
// own transaction: events before the first install carry none. It never reaches an owner webhook: the
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

// appProjectionVersion 2 adds actor_via_app (TASK-3411, SPEC-6 U10d). A v1
// block, written before it, carries no actor; the app DTO then attributes
// only create events, from the creator.
const appProjectionVersion = 2

// appProjectionKey is the payload key the block lives under.
const appProjectionKey = "app_projection"

type appProjectionCreator struct {
	UserID  string `json:"user_id,omitempty"`
	Display string `json:"display,omitempty"`
	Kind    string `json:"kind,omitempty"`
	// ViaApp is the install that created the item (items.created_via_app),
	// empty for an item a person or agent created directly (SPEC-6 §5,
	// TASK-3390).
	ViaApp string `json:"via_app,omitempty"`
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
	// ActorViaApp is the install whose fenced write caused THIS event, empty
	// for a person's or agent's write (v2, TASK-3411). Unlike the creator's
	// via_app it names the writer of the change, so an app can tell its own
	// echoes from other edits to the same item.
	ActorViaApp string `json:"actor_via_app,omitempty"`
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
	// ActorViaApp: as on the item block (v2, TASK-3411).
	ActorViaApp string `json:"actor_via_app,omitempty"`
}

// workspaceHasInstalledAppTx reports whether the workspace has an app
// installed, in any state but uninstalled: a disabled install can be enabled
// again and its dispatcher still needs the events in between.
func (s *Store) workspaceHasInstalledAppTx(tx *sql.Tx, workspaceID string) (bool, error) {
	if workspaceID == "" {
		return false, nil
	}
	var one int
	err := tx.QueryRow(s.q(`SELECT 1 FROM app_installs WHERE workspace_id = ? AND state <> 'uninstalled' LIMIT 1`), workspaceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("app projection: read installs: %w", err)
	}
	return true, nil
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
	if ok, err := s.workspaceHasInstalledAppTx(tx, item.WorkspaceID); err != nil || !ok {
		return nil, err
	}
	var creatorID, createdVia sql.NullString
	err := tx.QueryRow(s.q(`SELECT created_by_user_id, created_via_app FROM items WHERE id = ?`), item.ID).Scan(&creatorID, &createdVia)
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
		Creator:      appProjectionCreator{UserID: creatorID.String, Display: display, Kind: item.CreatedBy, ViaApp: createdVia.String},
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
//
// commentID names the comment, so the block records the install that wrote
// it (comments.via_app) as the creator's via_app, as an item's does (codex
// r4 on U10b). Every caller computes the block while the row still exists.
func (s *Store) buildCommentAppProjectionTx(tx *sql.Tx, commentID, itemID, userID, author, kind, parentID string) (*commentAppProjection, error) {
	var collectionID, workspaceID string
	err := tx.QueryRow(s.q(`SELECT collection_id, workspace_id FROM items WHERE id = ?`), itemID).Scan(&collectionID, &workspaceID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("app projection: read comment item: %w", err)
	}
	if ok, err := s.workspaceHasInstalledAppTx(tx, workspaceID); err != nil || !ok {
		return nil, err
	}
	display, err := s.userDisplayTx(tx, userID)
	if err != nil {
		return nil, err
	}
	if display == "" {
		display = author
	}
	var viaApp sql.NullString
	if err := tx.QueryRow(s.q(`SELECT via_app FROM comments WHERE id = ?`), commentID).Scan(&viaApp); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("app projection: read comment via_app: %w", err)
	}
	p := &commentAppProjection{
		V:            appProjectionVersion,
		CollectionID: collectionID,
		Creator:      appProjectionCreator{UserID: userID, Display: display, Kind: kind, ViaApp: viaApp.String},
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
