package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// App item writes inside a FencedTx (SPEC-6 U2a, DOC-3371 §4; TASK-3390).
//
// They reproduce the side effects an app write is ALLOWED to have (the
// allowed-writes table in §4) and none of the others. Compared with the
// human CreateItem / UpdateItem:
//   - no title semantics: no rename cascade, no broken-title resolution;
//   - no slug probing: the slug is the title plus a random suffix;
//   - no relation-link rebuild, no parent link, no attachment stamp
//     (pad-attachment: tokens are refused before this layer);
//   - wiki-link TARGETS resolve only to live companion items;
//   - activities are written INSIDE the transaction, not on the pool, with the
//     install in the debounce identity.
// The caller (internal/appstore) has already validated the input against the
// collection schema; these methods re-check everything that depends on the
// database under the fence.

// FencedActor is who an app write acts as: the delegated user ("user") or the
// install's bot ("agent").
type FencedActor struct {
	Kind      string // "user" or "agent"
	UserID    string
	AgentName string // optional, recorded in activity metadata
}

func (a FencedActor) valid() bool {
	return (a.Kind == "user" || a.Kind == "agent") && a.UserID != ""
}

// FencedItemCreate is an app item create, already validated by appstore.
type FencedItemCreate struct {
	CollectionID string
	Title        string
	Content      string
	Fields       string // a JSON object of declared, non-relation keys
	Actor        FencedActor
	// PlanLimit enforces items_per_workspace, as the human create does when
	// the server passes WithPlanLimit: on Pad Cloud, never on self-host.
	PlanLimit bool
}

// Errors the app layer maps to responses. None carries a count or another
// item's identity.
var (
	// ErrAppItemLimit: the workspace is at its item cap. The refusal names no
	// count (the human renderer's details.current is workspace-wide).
	ErrAppItemLimit = errors.New("app write: item limit reached")
	// ErrAppNotAppItem: the item exists and is a companion, but this install
	// did not create it, so the app may not update it.
	ErrAppNotAppItem = errors.New("app write: item was not created by this install")
	// ErrAppETagMismatch: the item changed since the app last read it.
	ErrAppETagMismatch = errors.New("app write: etag mismatch")
	// ErrAppBadActor: the actor is not a delegated user or the bot.
	ErrAppBadActor = errors.New("app write: invalid actor")
)

// appSource is items.source / item_versions.source / activities.source for an
// app write. None of those columns carries a CHECK (comments.source does; U2b).
const appSource = "app"

// appSlugSuffixLen is the random suffix on an app-created item's slug. Slugs
// are never allocated by scanning for collisions: a scan would let an app
// learn whether a hidden item holds a slug. A unique-index hit fails the
// transaction and appstore retries the whole create with a fresh suffix.
const appSlugSuffixLen = 6

func appSlug(title string) (string, error) {
	base := slugify(title)
	if base == "" {
		base = "untitled"
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	raw := make([]byte, appSlugSuffixLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("app slug: %w", err)
	}
	for i := range raw {
		raw[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return base + "-" + string(raw), nil
}

// AppItemETag is the opaque concurrency token an app sees: HMAC-SHA256 over
// (install, item, seq) under a SERVER-ONLY key, truncated to 128 bits. The app
// holds no key, so it cannot test candidate seq values offline (DOC-3371 §4).
func AppItemETag(key []byte, installID, itemID string, seq int64) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s\x00%s\x00%d", installID, itemID, seq)
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// CompanionCollectionSchema returns a companion collection's schema and
// settings, read in the fence, for the app layer's field validation.
func (f *FencedTx) CompanionCollectionSchema(collectionID string) (schemaJSON, settingsJSON string, err error) {
	if err := f.requireCompanionCollection(collectionID); err != nil {
		return "", "", err
	}
	err = f.tx.QueryRow(f.s.q(`SELECT schema, settings FROM collections WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL`),
		collectionID, f.workspaceID).Scan(&schemaJSON, &settingsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotCompanion
	}
	return schemaJSON, settingsJSON, err
}

// CompanionItemCollection returns the collection of a live item that this
// install created, refusing any other item the same way the update will.
func (f *FencedTx) CompanionItemCollection(itemID string) (string, error) {
	collectionID, err := f.requireCompanionItem(itemID)
	if err != nil {
		return "", err
	}
	if err := f.requireCreatedHere(itemID); err != nil {
		return "", err
	}
	return collectionID, nil
}

func (f *FencedTx) requireCreatedHere(itemID string) error {
	var createdVia sql.NullString
	if err := f.tx.QueryRow(f.s.q(`SELECT created_via_app FROM items WHERE id = ?`), itemID).Scan(&createdVia); err != nil {
		return fmt.Errorf("fenced item origin: %w", err)
	}
	if !createdVia.Valid || createdVia.String != f.installID {
		return ErrAppNotAppItem
	}
	return nil
}

// CreateItem creates an app item. Order: the companion check, the workspace
// lock (the human create's own key, so item_number and seq never collide),
// the item cap under that lock, then the row and its allowed side effects,
// the event and the activity.
func (f *FencedTx) CreateItem(in FencedItemCreate) (*models.Item, error) {
	if err := f.requireCompanionCollection(in.CollectionID); err != nil {
		return nil, err
	}
	if !in.Actor.valid() {
		return nil, ErrAppBadActor
	}
	if err := f.LockWorkspaceSeq(); err != nil {
		return nil, err
	}
	if in.PlanLimit {
		if err := f.s.enforceWorkspaceLimitTx(f.tx, f.workspaceID, "items_per_workspace"); err != nil {
			var pl *PlanLimitError
			if errors.As(err, &pl) {
				return nil, ErrAppItemLimit
			}
			return nil, err
		}
	}
	slug, err := appSlug(in.Title)
	if err != nil {
		return nil, err
	}
	fields := in.Fields
	if fields == "" {
		fields = "{}"
	}
	id := newID()
	ts := now()
	var flushedAt, flushedOpLogID any
	if in.Content != "" {
		flushedAt, flushedOpLogID = ts, int64(0)
	}
	actor := in.Actor
	if _, err := f.tx.Exec(f.s.q(`
		INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags,
		                   pinned, sort_order, role_sort_order,
		                   created_by, last_modified_by, source, item_number, created_at, updated_at,
		                   content_flushed_at, content_flushed_op_log_id, seq,
		                   created_by_user_id, last_modified_by_user_id, via_app, created_via_app)
		VALUES (?, ?, ?, ?, ?, ?, ?, '[]', ?, 0, 0, ?, ?, ?,
		        (SELECT COALESCE(MAX(item_number), 0) + 1 FROM items WHERE workspace_id = ?),
		        ?, ?, ?, ?, `+nextWorkspaceSeqSubquery+`, ?, ?, ?, ?)
	`), id, f.workspaceID, in.CollectionID, in.Title, slug, in.Content, fields,
		f.s.dialect.BoolToInt(false), actor.Kind, actor.Kind, appSource, f.workspaceID,
		ts, ts, flushedAt, flushedOpLogID, f.workspaceID,
		actor.UserID, actor.UserID, f.installID, f.installID); err != nil {
		return nil, fmt.Errorf("fenced item insert: %w", err)
	}

	if in.Content != "" {
		added, removed := diff.LineCounts("", in.Content)
		if _, err := f.tx.Exec(f.s.q(`
			INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at, version_seq,
			                           user_id, lines_added, lines_removed, is_create, via_app)
			VALUES (?, ?, ?, '', ?, ?, ?, ?, (SELECT COALESCE(MAX(version_seq), 0) + 1 FROM item_versions WHERE item_id = ?),
			        ?, ?, ?, ?, ?)
		`), newID(), id, in.Content, actor.Kind, appSource, f.s.dialect.BoolToInt(false), ts, id,
			actor.UserID, added, removed, f.s.dialect.BoolToInt(true), f.installID); err != nil {
			return nil, fmt.Errorf("fenced initial version: %w", err)
		}
	}

	if err := f.indexWikiLinks(id, in.Content); err != nil {
		return nil, err
	}

	doneKey := f.s.doneFieldKeyQ(f.tx, in.CollectionID)
	if initial := extractFieldValue(fields, doneKey); initial != "" {
		if _, err := f.tx.Exec(f.s.q(`
			INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+nextTransitionSeqSubquery+`)
		`), "create_"+id, id, f.workspaceID, in.CollectionID, doneKey, "", initial, ts); err != nil {
			return nil, fmt.Errorf("fenced create status transition: %w", err)
		}
	}

	item, err := f.s.getItemTx(f.tx, id)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, fmt.Errorf("fenced create: item %s not readable in transaction", id)
	}
	if err := f.s.emitItemEventTx(f.tx, kernelevents.ItemCreated, item, nil, ""); err != nil {
		return nil, err
	}
	if err := f.s.enqueueDecisionJobsTx(f.tx, f.workspaceID, item.ID, item.CollectionID); err != nil {
		return nil, err
	}
	if err := f.recordActivity(models.Activity{
		DocumentID: id, Action: "created", Actor: actor.Kind, Source: appSource,
		Metadata: agentMeta(actor.AgentName), UserID: actor.UserID,
	}); err != nil {
		return nil, err
	}
	return item, nil
}

// UpdateItemFields applies a fields patch (declared, scalar, non-relation
// keys, validated by appstore) to an item this install created. The seq lock
// is taken FIRST, so the item read, the etag check and the write all see the
// same row and no human write interleaves. No content, title, relation,
// parent or attachment change is possible here.
func (f *FencedTx) UpdateItemFields(itemID string, patch map[string]any, expectedETag string, etagKey []byte, actor FencedActor) (*models.Item, error) {
	if !actor.valid() {
		return nil, ErrAppBadActor
	}
	if err := f.LockWorkspaceSeq(); err != nil {
		return nil, err
	}
	collectionID, err := f.requireCompanionItem(itemID)
	if err != nil {
		return nil, err
	}
	if err := f.requireCreatedHere(itemID); err != nil {
		return nil, err
	}
	existing, err := f.s.getItemTx(f.tx, itemID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotCompanion
	}
	want := AppItemETag(etagKey, f.installID, itemID, existing.Seq)
	if !hmac.Equal([]byte(want), []byte(expectedETag)) {
		return nil, ErrAppETagMismatch
	}

	merged, err := mergeFieldsPatch(existing.Fields, patch)
	if err != nil {
		return nil, fmt.Errorf("fenced fields merge: %w", err)
	}
	doneKey := f.s.doneFieldKeyQ(f.tx, collectionID)
	statusBefore := extractFieldValue(existing.Fields, doneKey)
	ts := now()
	if _, err := f.tx.Exec(f.s.q(`
		UPDATE items SET fields = ?, updated_at = ?, seq = `+nextWorkspaceSeqSubquery+`,
		                 last_modified_by = ?, last_modified_by_user_id = ?, via_app = ?
		WHERE id = ?
	`), merged, ts, f.workspaceID, actor.Kind, actor.UserID, f.installID, itemID); err != nil {
		return nil, fmt.Errorf("fenced item update: %w", err)
	}

	newStatus := extractFieldValue(merged, doneKey)
	statusChanged := newStatus != statusBefore
	if statusChanged {
		if _, err := f.tx.Exec(f.s.q(`
			INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+nextTransitionSeqSubquery+`)
		`), newID(), itemID, f.workspaceID, collectionID, doneKey, statusBefore, newStatus, ts); err != nil {
			return nil, fmt.Errorf("fenced status transition: %w", err)
		}
	}

	updated, err := f.s.getItemTx(f.tx, itemID)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, fmt.Errorf("fenced update: item %s not readable in transaction", itemID)
	}
	if err := f.s.emitItemUpdateEventsTx(f.tx, existing, updated, statusChanged, statusBefore, doneKey, "", false); err != nil {
		return nil, err
	}
	if err := f.s.enqueueDecisionJobsTx(f.tx, f.workspaceID, itemID, collectionID); err != nil {
		return nil, err
	}
	meta := agentMeta(actor.AgentName)
	if changes := scalarFieldChanges(existing.Fields, merged); changes != "" {
		meta = withMetaKey(meta, "changes", changes)
	}
	if err := f.recordActivity(models.Activity{
		DocumentID: itemID, Action: "updated", Actor: actor.Kind, Source: appSource,
		Metadata: meta, UserID: actor.UserID,
	}); err != nil {
		return nil, err
	}
	return updated, nil
}

// indexWikiLinks indexes an app item's [[...]] links. It runs the human
// indexer, which writes ONLY rows whose source is this item, then breaks every
// resolved target that is not a live companion item: an app link names only
// what the app can see, and a link to anything else stays broken, which
// reveals nothing. [[workspace::REF]] is refused before this layer.
func (f *FencedTx) indexWikiLinks(itemID, content string) error {
	if err := f.s.replaceWikiLinks(f.tx, itemID, f.workspaceID, content); err != nil {
		return fmt.Errorf("fenced wiki links: %w", err)
	}
	ids := make([]string, 0, len(f.companions))
	for id := range f.companions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	q := `UPDATE item_wiki_links SET target_item_id = NULL
		WHERE source_item_id = ? AND target_item_id IS NOT NULL
		  AND target_item_id NOT IN (SELECT id FROM items WHERE workspace_id = ? AND deleted_at IS NULL`
	args := []any{itemID, f.workspaceID}
	if len(ids) > 0 {
		q += ` AND collection_id IN (?` + strings.Repeat(", ?", len(ids)-1) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	} else {
		q += ` AND 1 = 0`
	}
	q += `)`
	if _, err := f.tx.Exec(f.s.q(q), args...); err != nil {
		return fmt.Errorf("fenced wiki links: confine targets: %w", err)
	}
	return nil
}

// recordActivity writes an app activity on the fence's transaction. Only
// "updated" debounces, as on the human path; the install is part of the
// identity (Activity.ViaApp), so app and human rows never merge.
//
// LOCK ORDER: the caller holds the workspace seq lock; the debounce lock is
// taken second, the one order BUG-2777 permits (activities.go). Nothing else
// is locked after it.
func (f *FencedTx) recordActivity(a models.Activity) error {
	a.WorkspaceID = f.workspaceID
	a.ViaApp = f.installID
	if a.Metadata == "" {
		a.Metadata = "{}"
	}
	if a.Action != "updated" {
		_, err := f.s.createActivityQ(f.tx, a)
		return err
	}
	if f.s.dialect.Driver() == DriverPostgres {
		if _, err := f.tx.Exec("SELECT pg_advisory_xact_lock(hashtext('pad:activity-debounce:' || $1))", a.DocumentID); err != nil {
			return fmt.Errorf("fenced activity: debounce lock: %w", err)
		}
	}
	cutoff := time.Now().UTC().Add(-ActivityDebounceCooldown).Format(time.RFC3339)
	existingID, existingMeta, ok := f.s.recentDebounceCandidateQ(f.tx, a, cutoff, models.AgentNameFromMetadata(a.Metadata))
	if !ok {
		return fmt.Errorf("fenced activity: read debounce candidate")
	}
	if existingID != "" {
		res, err := f.tx.Exec(f.s.q(`
			UPDATE activities SET metadata = ?, created_at = ?
			WHERE id = ?
			  AND `+debounceCASPredicate+`
			  AND NOT EXISTS (SELECT 1 FROM comments c WHERE c.activity_id = activities.id AND c.item_id = activities.document_id)
		`), mergeActivityMeta(existingMeta, a.Metadata), now(), existingID, existingMeta)
		if err != nil {
			return fmt.Errorf("fenced activity: merge: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 1 {
			return nil
		}
		// Refused: a comment links the row, or a pool merge moved it. A fresh
		// row costs one extra timeline entry and loses nothing.
	}
	_, err := f.s.createActivityQ(f.tx, a)
	return err
}

func agentMeta(agent string) string {
	if agent == "" {
		return "{}"
	}
	b, _ := json.Marshal(map[string]string{"agent": agent})
	return string(b)
}

func withMetaKey(meta, key, value string) string {
	m := map[string]any{}
	_ = json.Unmarshal([]byte(meta), &m)
	m[key] = value
	b, _ := json.Marshal(m)
	return string(b)
}

// scalarFieldChanges renders "key: old → new" for each changed key, sorted
// and joined with "; ", the human activity format (server diffFields). App
// patches carry scalar values only, so values compare by their canonical JSON.
func scalarFieldChanges(before, after string) string {
	oldMap, err := models.DecodeFieldsJSON([]byte(before))
	if err != nil {
		return ""
	}
	newMap, err := models.DecodeFieldsJSON([]byte(after))
	if err != nil {
		return ""
	}
	var changes []string
	for key, newVal := range newMap {
		oldVal, exists := oldMap[key]
		if !exists {
			changes = append(changes, fmt.Sprintf("%s: → %s", key, showFieldValue(newVal)))
			continue
		}
		ob, _ := json.Marshal(models.CanonicalJSONNumbers(oldVal))
		nb, _ := json.Marshal(models.CanonicalJSONNumbers(newVal))
		if string(ob) == string(nb) {
			continue
		}
		changes = append(changes, fmt.Sprintf("%s: %s → %s", key, showFieldValue(oldVal), showFieldValue(newVal)))
	}
	sort.Strings(changes)
	return strings.Join(changes, "; ")
}

// showFieldValue renders a scalar field value for an activity change line: a
// string as itself, anything else as its canonical JSON.
func showFieldValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(models.CanonicalJSONNumbers(v))
	return string(b)
}

// IsItemSlugConflict reports whether err is the items (workspace_id, slug)
// unique violation, and no other: an app create retries only that one,
// with a fresh random suffix. Any other unique violation (item_number, a
// duplicate id) is a real failure and must surface, not be retried away.
func IsItemSlugConflict(err error) bool {
	if !isUniqueViolation(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "items_workspace_id_slug_key") || // Postgres default constraint name
		strings.Contains(msg, "items.workspace_id, items.slug") // SQLite
}
