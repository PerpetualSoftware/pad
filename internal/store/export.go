package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// coerceJSONForImport normalizes an imported JSON column value to a
// well-formed default of the expected shape. Used at the import boundary
// (ImportWorkspace) where legacy / external workspace bundles may carry
// empty-string or malformed JSON that bypasses the column's NOT NULL
// DEFAULT (IDEA-1486) and that the handler-layer shape validators
// (IDEA-1488) never see. The lenient policy (coerce + log) matches the
// IDEA-1484 import-side precedent at export.go:206-209: don't break a
// workspace import because one row carries malformed data.
//
//   - raw == ""                     → return defaultJSON (empty-string sentinel).
//   - raw is well-formed JSON of the expected shape (non-nil object/array)
//     → return raw verbatim.
//   - raw is JSON `null`, unparseable, or wrong-shape → return defaultJSON
//     AND log a structured warning naming the field + row. The raw
//     value's LENGTH is logged (`raw_len`) but never its content, so
//     user data does not leak into logs.
//
// expectObject==true requires the parsed value to be a JSON object
// (map[string]any); expectObject==false requires a JSON array
// ([]any) — used for items.tags.
//
// IDEA-1486 R1 codex P2: `json.Unmarshal("null", &m)` returns nil error
// and leaves the destination nil. Imported `fields: null` (or the
// string literal `"null"` at the JSON-encoded-string layer) would
// otherwise satisfy this validator and land as a JSONB null on
// Postgres (which technically satisfies NOT NULL — SQL NULL ≠ JSONB
// null) or the text "null" on SQLite. Downstream readers that expect
// an object would choke. The non-nil check below forces JSON null to
// the log-and-coerce path with the rest of the malformed shapes.
func coerceJSONForImport(raw, defaultJSON, field, rowID, workspaceID string, expectObject bool) string {
	if raw == "" {
		return defaultJSON
	}
	if expectObject {
		var v map[string]any
		if err := json.Unmarshal([]byte(raw), &v); err == nil && v != nil {
			return raw
		}
	} else {
		var v []any
		if err := json.Unmarshal([]byte(raw), &v); err == nil && v != nil {
			return raw
		}
	}
	slog.Warn("import_workspace coerced malformed json",
		"field", field,
		"row_id", rowID,
		"workspace_id", workspaceID,
		"raw_len", len(raw))
	return defaultJSON
}

// importCoercedTitle is the item-title half of the same log-and-coerce policy
// (BUG-2833 / BUG-2831). It returns the title to write and, when it changed,
// the reason — so the caller can log one line carrying the row identity.
//
// COERCE, NOT REFUSE, and that is a ruling rather than a shortcut. The
// interactive doors (create, update, artifact import) REFUSE an empty or
// over-long title, because they are minting a new value from a live caller who
// can fix it. Import is restoring data this product already accepted: a
// 300-rune title has always been legal, so validating a bundle would turn
// "restore my archive" into a hard failure for rows we ourselves wrote, and
// `pad db migrate` — which is ExportWorkspace piped into this same INSERT —
// would refuse to migrate SQLite instances that work fine today.
//
// It also sits three lines from coerceJSONForImport, whose recorded IDEA-1488
// disposition is log-and-coerce "so a legacy bundle with one malformed item
// still imports". Refusing titles in the same loop would give one loop two
// dispositions for the same class of problem.
//
// The coercion does NOT weaken Dave's untitled-items ruling. The imported row
// lands WITH a title — the literal string "Untitled" — so the invariant that
// no door writes an empty title into the database holds here too, and the
// store's baseSlug == "" fallback stays defensive-only.
func importCoercedTitle(raw string) (string, string) {
	title := models.NormalizeItemTitle(raw)
	if title == "" {
		return importUntitledTitle, "empty"
	}
	if n := utf8.RuneCountInString(title); n > models.MaxItemTitleRunes {
		return string([]rune(title)[:models.MaxItemTitleRunes]), "too_long"
	}
	return title, ""
}

// importUntitledTitle is what an empty imported title becomes. Capitalized
// because it is a TITLE that a user will read in a list, not the lowercase
// "untitled" slug fallback in items.go — the two are deliberately different
// strings so a reader can tell which mechanism produced what they are looking
// at.
const importUntitledTitle = "Untitled"

// importCoercedSlug bounds an imported slug (BUG-2831). Coercing only the
// title would not close the import door: this loop writes `it.Slug` from the
// bundle VERBATIM rather than re-deriving it, and the slug — not the title —
// is what carries UNIQUE(workspace_id, slug) into a Postgres btree index tuple
// (capped at 2704 bytes in practice; see MaxItemTitleRunes for the
// measurement). A bundle from a SQLite instance holding a 2 MiB title carries a
// 2 MiB slug, and truncating the title alone would leave the INSERT failing
// exactly as before.
//
// Untouched unless it exceeds the bound: an in-range slug is written byte for
// byte, so nothing about ordinary round-tripping changes. Truncation can
// collide with another row in the same workspace, which is why the caller
// resolves the result through uniqueSlugQ inside the import transaction rather
// than hoping — but only for coerced slugs, so the uniqueness scan does not run
// for every row of a large import.
func importCoercedSlug(raw string) (string, bool) {
	if utf8.RuneCountInString(raw) <= models.MaxItemTitleRunes {
		return raw, false
	}
	return string([]rune(raw)[:models.MaxItemTitleRunes]), true
}

// ExportWorkspace exports all data for a workspace into a portable format.
func (s *Store) ExportWorkspace(slug string) (*models.WorkspaceExport, error) {
	ws, err := s.GetWorkspaceBySlug(slug)
	if err != nil {
		return nil, fmt.Errorf("workspace lookup: %w", err)
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace not found: %s", slug)
	}

	export := &models.WorkspaceExport{
		Version:    1,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Workspace: models.WorkspaceExportMeta{
			Name:        ws.Name,
			Slug:        ws.Slug,
			Description: ws.Description,
			Settings:    ws.Settings,
		},
	}

	// Collections
	rows, err := s.db.Query(s.q(`
		SELECT id, name, slug, icon, description, schema, settings, traits, prefix, sort_order, is_default, is_system, created_at, updated_at
		FROM collections WHERE workspace_id = ? AND deleted_at IS NULL
		ORDER BY sort_order, name`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export collections: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c models.CollectionExport
		var isDefault, isSystem bool
		if err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.Icon, &c.Description, &c.Schema, &c.Settings, &c.Traits, &c.Prefix, &c.SortOrder, &isDefault, &isSystem, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan collection: %w", err)
		}
		c.IsDefault = isDefault
		c.IsSystem = isSystem
		export.Collections = append(export.Collections, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Items
	itemRows, err := s.db.Query(s.q(`
		SELECT id, collection_id, title, slug, content, fields, tags, pinned, sort_order,
		       COALESCE(parent_id, ''), created_by, last_modified_by, source, COALESCE(item_number, 0), created_at, updated_at
		FROM items WHERE workspace_id = ? AND deleted_at IS NULL
		ORDER BY created_at, id`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export items: %w", err)
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var it models.ItemExport
		var pinned bool
		if err := itemRows.Scan(&it.ID, &it.CollectionID, &it.Title, &it.Slug, &it.Content, &it.Fields, &it.Tags, &pinned, &it.SortOrder, &it.ParentID, &it.CreatedBy, &it.LastModifiedBy, &it.Source, &it.ItemNumber, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		it.Pinned = pinned
		export.Items = append(export.Items, it)
	}
	if err := itemRows.Err(); err != nil {
		return nil, err
	}

	// Comments
	commentRows, err := s.db.Query(s.q(`
		SELECT c.id, c.item_id, c.author, c.body, c.created_by, c.source, c.created_at, c.updated_at
		FROM comments c
		JOIN items i ON c.item_id = i.id
		WHERE c.workspace_id = ? AND i.deleted_at IS NULL
		ORDER BY c.created_at`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export comments: %w", err)
	}
	defer commentRows.Close()
	for commentRows.Next() {
		var cm models.CommentExport
		if err := commentRows.Scan(&cm.ID, &cm.ItemID, &cm.Author, &cm.Body, &cm.CreatedBy, &cm.Source, &cm.CreatedAt, &cm.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		export.Comments = append(export.Comments, cm)
	}
	if err := commentRows.Err(); err != nil {
		return nil, err
	}

	// Item links — exported in full, including links whose source or target item
	// is soft-deleted. This is intentional and differs from user-facing reads
	// (GetItemLinks/GetParentForItem/GetParentMap, which all filter on
	// items.deleted_at IS NULL — see BUG-734). Backups need to round-trip the
	// raw graph so that re-importing into a workspace where the deleted items
	// are restored preserves the original relationships. The import path
	// already silently skips links whose endpoints are missing entirely.
	linkRows, err := s.db.Query(s.q(`
		SELECT id, source_id, target_id, link_type, created_by, created_at
		FROM item_links WHERE workspace_id = ?
		ORDER BY created_at`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export item links: %w", err)
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var lk models.ItemLinkExport
		if err := linkRows.Scan(&lk.ID, &lk.SourceID, &lk.TargetID, &lk.LinkType, &lk.CreatedBy, &lk.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan item link: %w", err)
		}
		export.ItemLinks = append(export.ItemLinks, lk)
	}
	if err := linkRows.Err(); err != nil {
		return nil, err
	}

	// Reminders — exported with their lifecycle marks intact, and ONLY for
	// items this bundle actually carries.
	//
	// The comment that stood here said soft-deleted items' reminders were
	// included so "a restore that brings the item back brings its reminder
	// with it", copying the item_links rationale. That was false for this
	// table: the items section filters on `deleted_at IS NULL`, so the item is
	// NOT in the bundle, and there is no restore that could ever reunite them
	// — the import simply drops the orphan on its itemMap lookup. Exporting
	// them shipped rows that could only ever be discarded, under a comment
	// asserting a benefit the bundle cannot deliver.
	//
	// item_links can carry soft-deleted endpoints because a link is a row
	// ABOUT two items and the graph is worth round-tripping raw; a reminder
	// whose item is absent is not a relationship, it is a dangling schedule.
	reminderRows, err := s.db.Query(s.q(`
		SELECT r.item_id, r.remind_at, COALESCE(r.fired_at, ''), COALESCE(r.acked_at, ''), r.created_at, r.updated_at
		FROM item_reminders r
		JOIN items i ON i.id = r.item_id
		WHERE r.workspace_id = ? AND i.deleted_at IS NULL
		ORDER BY r.created_at, r.id`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export reminders: %w", err)
	}
	defer reminderRows.Close()
	for reminderRows.Next() {
		var rm models.ReminderExport
		if err := reminderRows.Scan(&rm.ItemID, &rm.RemindAt, &rm.FiredAt, &rm.AckedAt, &rm.CreatedAt, &rm.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan reminder: %w", err)
		}
		export.Reminders = append(export.Reminders, rm)
	}
	if err := reminderRows.Err(); err != nil {
		return nil, err
	}

	// Item versions
	versionRows, err := s.db.Query(s.q(`
		SELECT v.id, v.item_id, v.content, v.change_summary, v.created_by, v.source, v.is_diff, v.created_at
		FROM item_versions v
		JOIN items i ON v.item_id = i.id
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL
		ORDER BY v.created_at, v.version_seq`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export item versions: %w", err)
	}
	defer versionRows.Close()
	for versionRows.Next() {
		var ver models.ItemVersionExport
		var isDiff bool
		if err := versionRows.Scan(&ver.ID, &ver.ItemID, &ver.Content, &ver.ChangeSummary, &ver.CreatedBy, &ver.Source, &isDiff, &ver.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan item version: %w", err)
		}
		ver.IsDiff = isDiff
		export.ItemVersions = append(export.ItemVersions, ver)
	}
	if err := versionRows.Err(); err != nil {
		return nil, err
	}

	return export, nil
}

// ImportWorkspace imports a workspace from an exported data structure.
// It creates a new workspace with regenerated IDs, remapping all references.
// If newName is non-empty, it overrides the workspace name and slug.
func (s *Store) ImportWorkspace(data *models.WorkspaceExport, newName string, ownerID string) (*models.Workspace, error) {
	if data.Version != 1 {
		return nil, fmt.Errorf("unsupported export version: %d", data.Version)
	}

	// Determine workspace name/slug
	wsName := data.Workspace.Name
	wsSlug := data.Workspace.Slug
	if newName != "" {
		wsName = newName
		wsSlug = newName
	}

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{
		Name:        wsName,
		Slug:        wsSlug,
		Description: data.Workspace.Description,
		Settings:    data.Workspace.Settings,
		OwnerID:     ownerID,
	})
	if err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}

	// Run all data inserts in a single transaction for atomicity
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// ID mapping: old ID -> new ID
	collMap := make(map[string]string)
	itemMap := make(map[string]string)

	// Import collections
	// Workspace-level trait conflicts in the ARCHIVE. The API gate refuses a
	// duplicate artifact_kind / invocation_field at create and update, but an
	// archive is written elsewhere and arrives whole, so it can carry a pair
	// the gate never saw. Import does not REFUSE it — an archive is often the
	// only copy of a workspace, and rejecting the whole restore over an
	// ambiguity the resolvers can still work through would be the wrong
	// trade. What must not happen is that it lands silently: with two
	// declarations live, which collection receives an imported artifact or
	// answers an invocation slug depends on collection order. Warn so the
	// operator can fix it. Codex round 8.
	seenKinds := map[string]string{}
	invocationCollection := ""
	for _, c := range data.Collections {
		if t, err := models.ParseCollectionTraits(c.Traits); err == nil {
			if t.ArtifactKind != nil && t.ArtifactKind.Kind != "" {
				if prev, dup := seenKinds[t.ArtifactKind.Kind]; dup {
					slog.Warn("import: archive declares one artifact kind on two collections; artifact routing will depend on collection order until one is changed",
						"kind", t.ArtifactKind.Kind, "collections", prev+","+c.Slug, "workspace_id", ws.ID)
				} else {
					seenKinds[t.ArtifactKind.Kind] = c.Slug
				}
			}
			if t.InvocationField != "" {
				if invocationCollection != "" {
					slog.Warn("import: archive declares invocation routing on two collections; playbook resolution will depend on collection order until one is changed",
						"collections", invocationCollection+","+c.Slug, "workspace_id", ws.ID)
				} else {
					invocationCollection = c.Slug
				}
			}
		}
	}

	for _, c := range data.Collections {
		newCollID := newID()
		collMap[c.ID] = newCollID

		// Coerce empty-string / malformed settings to a valid JSON object
		// before insert. IDEA-1484 (PR #562) hardened collections.settings
		// to NOT NULL DEFAULT '{}', but this INSERT explicitly supplies
		// the settings column — so the DEFAULT clause does NOT fire when
		// c.Settings is "". Without this coercion, Postgres rejects `""`
		// at JSONB type-validation and SQLite silently stores invalid
		// JSON. Legacy bundles and plain-JSON workspace imports
		// (handlers_workspaces.go, handlers_import_bundle.go,
		// cmd/pad/main.go's migrate command) can still carry "" or
		// malformed settings, so normalization belongs at the import
		// boundary rather than at the schema level. IDEA-1488 extends
		// this to log-and-coerce on non-empty malformed JSON.
		settings := coerceJSONForImport(c.Settings, "{}", "collections.settings", c.ID, ws.ID, true)
		// Same coercion for traits, and for the same reason: this INSERT
		// supplies the column explicitly, so the NOT NULL DEFAULT '{}' never
		// fires. Archives written before TASK-2657 carry no traits key at
		// all, which lands here as "" — a pre-traits archive imports as a
		// collection that declares nothing, which is the honest reading.
		traits := coerceJSONForImport(c.Traits, "{}", "collections.traits", c.ID, ws.ID, true)
		// coerceJSONForImport only guarantees the blob is valid JSON. Traits
		// are declarations that switch kernel behavior on, so an import is
		// held to the same grammar the API enforces — otherwise a
		// hand-edited or foreign archive can persist a declaration that
		// parses as JSON, fails the trait parse, and degrades to "declares
		// nothing", silently disabling bootstrap or invocation routing for
		// the imported workspace.
		//
		// Degrade rather than reject: an archive is often the only copy of a
		// workspace, and refusing the whole import over one bad declaration
		// would be worse than importing it with that collection declaring
		// nothing — which is exactly what a pre-traits archive does anyway.
		// Logged so it isn't silent. TASK-2657.
		// Reject declarations that don't parse or don't validate. The log
		// deliberately does NOT claim what the collection ends up with —
		// inference below may still give it the canonical set, and an earlier
		// version of this said "importing with no declarations" and was then
		// contradicted three lines later. Codex round 6.
		discarded := false
		if parsed, perr := models.ParseCollectionTraits(traits); perr != nil {
			slog.Warn("import: collection traits could not be parsed; discarding them",
				"collection", c.Slug, "workspace_id", ws.ID, "error", perr)
			traits = "{}"
			discarded = true
		} else if verr := parsed.Validate(); verr != nil {
			slog.Warn("import: collection traits failed validation; discarding them",
				"collection", c.Slug, "workspace_id", ws.ID, "error", verr)
			traits = "{}"
			discarded = true
		}

		// COMPATIBILITY INFERENCE for archives written before traits existed.
		//
		// Without this, importing a pre-TASK-2657 export silently reproduces
		// exactly the defect traits were introduced to fix ([[BUG-2702]]): the
		// migration backfill cannot help, because it ran long before these
		// rows were inserted, so the imported conventions/playbooks
		// collections would carry no declarations and the workspace would
		// come up with no always-on rules, no playbook invocation routing and
		// no artifact export. The archive looks fine and the workspace is
		// quietly inert.
		//
		// Only applied when the collection declares NOTHING. A declaration
		// that survived the round trip is authoritative, including the
		// deliberate empty one a user can set — but an archive that predates
		// the column cannot be distinguished from that case, and restoring a
		// working workspace is worth more than honouring a deliberate clear
		// that only round-trips through an export. Slug-keyed for the same
		// reason the migration is: on a row with no declarations, the slug is
		// the only evidence of intent that exists. Codex round 4.
		if inferred := collections.CanonicalTraitsForSlug(c.Slug); !inferred.IsZero() {
			if current, err := models.ParseCollectionTraits(traits); err == nil && current.IsZero() {
				if encoded, err := inferred.JSON(); err == nil {
					// Two different situations reach here and the log must
					// distinguish them: an archive that never had traits
					// (expected, benign) versus one whose declarations were
					// just thrown away (a data problem the operator should
					// know about, even though the outcome is a working
					// collection either way).
					if discarded {
						slog.Warn("import: collection's invalid declarations were discarded; substituting the canonical set for its slug",
							"collection", c.Slug, "workspace_id", ws.ID)
					} else {
						slog.Info("import: collection carried no trait declarations; inferring the canonical set from its slug",
							"collection", c.Slug, "workspace_id", ws.ID)
					}
					traits = encoded
				}
			}
		}

		_, err := tx.Exec(s.q(`
			INSERT INTO collections (id, workspace_id, name, slug, icon, description, schema, settings, traits, prefix, sort_order, is_default, is_system, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			newCollID, ws.ID, c.Name, c.Slug, c.Icon, c.Description, c.Schema, settings, traits, c.Prefix, c.SortOrder, s.dialect.BoolToInt(c.IsDefault), s.dialect.BoolToInt(c.IsSystem),
			c.CreatedAt, c.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("import collection %s: %w", c.Name, err)
		}
	}

	// Import items (first pass: create items, remap collection_id)
	// Item numbers are assigned sequentially in created_at order to produce
	// workspace-global numbering. Exported item_number values are ignored
	// because old exports used per-collection numbering which can have
	// duplicates within a workspace.
	//
	// IDEA-1486 + IDEA-1488: precompute each item's coerced fields/tags so
	// the second-pass UPDATE (which re-applies fields after the ID remap)
	// uses the SAME normalized value as the first-pass INSERT. Without
	// this, a malformed it.Fields gets coerced to "{}" on INSERT but the
	// second pass would re-write the original malformed value verbatim,
	// undoing the coercion. The map is keyed by the original (exported)
	// item ID so the second-pass loop can look up the normalized value.
	coercedFields := make(map[string]string, len(data.Items))
	coercedTags := make(map[string]string, len(data.Items))
	// Slugs this import has already written. See the collision note in the loop.
	claimedSlugs := make(map[string]bool, len(data.Items))
	// itemMap records the id an item WOULD get; insertedItems records the ones
	// that actually landed. The two differ for an orphaned item — one whose
	// collection is missing from the bundle — because the map entry is written
	// before the skip below, and it has to be: parent resolution inside this
	// same loop reads itemMap for items it has not reached yet.
	//
	// So a later section resolving an id through itemMap alone can get one
	// that names no row, and inserting a foreign key to it fails (SQLite
	// enforces FKs here — `_pragma=foreign_keys(on)` in the DSN — and Postgres
	// always does). item_links and item_versions survive that by skipping on
	// error; the reminder loop below checks this set instead, which refuses
	// the row for the right reason rather than letting the database refuse it
	// for an incidental one.
	insertedItems := make(map[string]bool, len(data.Items))
	var nextItemNumber int
	for _, it := range data.Items {
		newItemID := newID()
		itemMap[it.ID] = newItemID
		newCollID := collMap[it.CollectionID]
		if newCollID == "" {
			continue // skip orphaned items
		}

		// On first pass, parent_id may refer to an item not yet created, so use empty
		parentID := ""
		if it.ParentID != "" {
			if mapped, ok := itemMap[it.ParentID]; ok {
				parentID = mapped
			}
		}

		nextItemNumber++
		// IDEA-1486 + IDEA-1488: coerce empty-string / malformed
		// fields/tags at the import boundary. After migration 056 /
		// pgmigrations 035 hardened items.fields and items.tags to
		// NOT NULL DEFAULT, an imported item with fields="" 500s on
		// Postgres at JSONB type-validation and silently stores
		// invalid JSON on SQLite. Mirror collections.settings'
		// coercion above. The IDEA-1488 leg: log-and-coerce (not
		// fail-stop) so a legacy bundle with one malformed item
		// still imports.
		// BUG-2833 / BUG-2831: title + slug coercion, same log-and-coerce
		// policy as the fields/tags coercion below. See importCoercedTitle for
		// why import coerces where the interactive doors refuse.
		itemTitle, titleCoercion := importCoercedTitle(it.Title)
		if titleCoercion != "" {
			slog.Warn("import_workspace coerced item title",
				"reason", titleCoercion,
				"row_id", it.ID,
				"workspace_id", ws.ID,
				"raw_runes", utf8.RuneCountInString(it.Title))
		}
		itemSlug, slugCoerced := importCoercedSlug(it.Slug)
		// Collision resolution is keyed on the CLAIMED SET, not on whether THIS
		// row was truncated (codex round 1, P1). Truncation is the only source
		// of duplicate slugs here — a bundle exported from a live workspace
		// cannot contain two identical slugs, because UNIQUE(workspace_id, slug)
		// held there — but the duplicate it creates can land in EITHER order:
		// a long slug truncated to "x" can be inserted BEFORE a later row whose
		// slug is already exactly "x". Resolving only the truncated row leaves
		// that later, untouched row to hit the constraint and abort the entire
		// import, which is the opposite of this loop's coerce-and-continue
		// policy.
		//
		// Guarded by the map rather than by calling uniqueSlugQ unconditionally
		// so an ordinary import of N items still issues no extra queries: the
		// common case is a bundle with no truncation at all, where this costs
		// one map lookup per row.
		if claimedSlugs[itemSlug] {
			unique, uerr := s.uniqueSlugQ(tx, "items", "workspace_id", ws.ID, itemSlug)
			if uerr != nil {
				return nil, fmt.Errorf("import item %s: unique slug after truncation: %w", it.ID, uerr)
			}
			slog.Warn("import_workspace resolved a colliding item slug",
				"row_id", it.ID,
				"workspace_id", ws.ID,
				"requested", itemSlug,
				"slug", unique,
				"from_truncation", slugCoerced)
			itemSlug = unique
		}
		if slugCoerced {
			slog.Warn("import_workspace coerced item slug",
				"reason", "too_long",
				"row_id", it.ID,
				"workspace_id", ws.ID,
				"raw_runes", utf8.RuneCountInString(it.Slug),
				"slug", itemSlug)
		}
		claimedSlugs[itemSlug] = true
		fieldsJSON := coerceJSONForImport(it.Fields, "{}", "items.fields", it.ID, ws.ID, true)
		tagsJSON := coerceJSONForImport(it.Tags, "[]", "items.tags", it.ID, ws.ID, false)
		coercedFields[it.ID] = fieldsJSON
		coercedTags[it.ID] = tagsJSON
		// Stamp `seq` so workspace import populates the delta-sync cursor
		// column (PLAN-1343 / TASK-1352). Each INSERT reads MAX(seq)+1
		// within this transaction, so imported rows get sequential
		// per-workspace seqs — clients post-import see them on the next
		// /items-index fetch, and any subsequent mutation keeps bumping
		// from a sensible floor instead of a flat MAX(seq)=0.
		// Stamp BEFORE the INSERT — see the ORDERING note on
		// stampAttachmentRefsTx (BUG-2415).
		if err := stampAttachmentRefsTx(tx, s, ws.ID, it.Content, fieldsJSON); err != nil {
			return nil, err
		}
		// NO EVENT IS EMITTED HERE, AND THAT IS A RULING, NOT AN OVERSIGHT.
		// This is the second item-creation write path in the codebase — the
		// other is insertItemTx, which owns the API paths and (from TASK-2658)
		// the transactional event outbox. Import deliberately stays silent:
		// SPEC-3 (DOC-2653) §Taxonomy records that events/1 cannot express an
		// import, so restoring a 900-item archive fires no item.created fan-out
		// and consumers resync out-of-band. If a future contract version wants
		// import observable, the spec pre-names the shape — ONE additive
		// workspace-level `workspace.imported` event, never per-item fan-out.
		// Do not "fix" this by hanging an outbox write off this INSERT.
		_, err := tx.Exec(s.q(`
			INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags, pinned, sort_order, parent_id, created_by, last_modified_by, source, item_number, created_at, updated_at, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, `+nextWorkspaceSeqSubquery+`)`),
			newItemID, ws.ID, newCollID, itemTitle, itemSlug, it.Content, fieldsJSON, tagsJSON, s.dialect.BoolToInt(it.Pinned), it.SortOrder,
			parentID, it.CreatedBy, it.LastModifiedBy, it.Source, nextItemNumber,
			it.CreatedAt, it.UpdatedAt, ws.ID)
		if err != nil {
			return nil, fmt.Errorf("import item %s: %w", it.Title, err)
		}
		insertedItems[newItemID] = true
	}

	// Second pass: remap parent_id and relation fields (now all items exist).
	// Use the coerced first-pass fields value as the remap input so a
	// malformed-and-coerced row doesn't get its coercion clobbered by the
	// raw export value (IDEA-1486 + IDEA-1488).
	_ = coercedTags // tags don't carry ID relations; second-pass only touches fields
	for _, it := range data.Items {
		newItemID := itemMap[it.ID]
		if newItemID == "" {
			continue
		}
		fieldsInput, ok := coercedFields[it.ID]
		if !ok {
			fieldsInput = it.Fields // defensive — should always be populated by first pass
		}
		// Remap relation fields now that ALL items are mapped
		fields := remapFieldIDs(fieldsInput, itemMap, collMap)
		parentID := ""
		if it.ParentID != "" {
			if mapped, ok := itemMap[it.ParentID]; ok {
				parentID = mapped
			}
		}
		_, err := tx.Exec(s.q(`UPDATE items SET fields = ?, parent_id = NULLIF(?, '') WHERE id = ?`),
			fields, parentID, newItemID)
		if err != nil {
			return nil, fmt.Errorf("remap item %s: %w", it.Title, err)
		}
	}

	// Import comments
	for _, cm := range data.Comments {
		newItemID := itemMap[cm.ItemID]
		if newItemID == "" {
			continue
		}
		// Stamp BEFORE the INSERT (BUG-2415).
		if err := stampAttachmentRefsTx(tx, s, ws.ID, cm.Body); err != nil {
			return nil, err
		}
		_, err := tx.Exec(s.q(`
			INSERT INTO comments (id, item_id, workspace_id, author, body, created_by, source, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			newID(), newItemID, ws.ID, cm.Author, cm.Body, cm.CreatedBy, cm.Source,
			cm.CreatedAt, cm.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("import comment: %w", err)
		}
	}

	// Import item links
	for _, lk := range data.ItemLinks {
		newSourceID := itemMap[lk.SourceID]
		newTargetID := itemMap[lk.TargetID]
		if newSourceID == "" || newTargetID == "" {
			continue
		}
		_, err := tx.Exec(s.q(`
			INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			newID(), ws.ID, newSourceID, newTargetID, lk.LinkType, lk.CreatedBy,
			lk.CreatedAt)
		if err != nil {
			// Ignore duplicate links
			continue
		}
	}

	// Import reminders. NULL rather than empty string for the unset marks —
	// the lifecycle is defined by NULL-ness (models.Reminder), and an empty
	// string would make a never-fired reminder read as fired at "".
	for _, rm := range data.Reminders {
		newItemID := itemMap[rm.ItemID]
		// TWO GUARDS, AND NEITHER ALONE IS OBSERVABLE — measured, not assumed.
		// Reverting either one on its own leaves the test green: with the map
		// gate restored, the skip-on-error below survives the FK failure; with
		// the fatal return restored, this gate means the insert never fails.
		// Removing BOTH is what fails it. They are kept as a pair because they
		// defend the same failure at different depths — this one prevents the
		// bad write, the one below survives a bad write that arrives some
		// other way — and the pair is recorded here so a future reader does
		// not delete one as dead code after watching its mutant survive.
		//
		// insertedItems, not just a non-empty mapping: an ORPHANED item — one
		// whose collection is missing from the bundle — still gets a map entry
		// (it is written before the skip, because parent resolution needs it),
		// so `!= ""` is satisfied by an id that names no row. Inserting a
		// foreign key to it fails, and this loop used to treat that as fatal,
		// so ONE orphaned item with a reminder aborted the entire workspace
		// restore. Codex round 10.
		if !insertedItems[newItemID] {
			continue
		}
		// NORMALIZE ON THE WAY IN. Import is a WRITER like any other, and a
		// bundle is not necessarily one this server produced — it can be
		// hand-edited, or come from another instance. Inserting a raw
		// remind_at would let a bare date or a local offset into the one
		// column every comparison downstream treats as a UTC instant, where
		// it fires early, late, or never. Every other door normalizes; this
		// one was writing underneath them.
		//
		// A value that will not parse is SKIPPED, not fatal: the import-side
		// precedent here is lenient (coerce or drop, keep the import alive)
		// rather than failing a whole workspace restore over one row.
		remindAt, err := normalizeRemindAt(rm.RemindAt)
		if err != nil {
			slog.Warn("workspace import: skipping reminder with an unparseable remind_at",
				"workspace_id", ws.ID, "item_id", newItemID, "raw_len", len(rm.RemindAt))
			continue
		}
		var firedAt, ackedAt any
		if rm.FiredAt != "" {
			firedAt = rm.FiredAt
		}
		if rm.AckedAt != "" {
			ackedAt = rm.AckedAt
		}
		if _, err := tx.Exec(s.q(`
			INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			newID(), ws.ID, newItemID, remindAt, firedAt, ackedAt, rm.CreatedAt, rm.UpdatedAt); err != nil {
			// SKIP, not fatal — matching item_links and item_versions, whose
			// loops both survive a bad row. A reminder is the least critical
			// thing in a bundle, and failing a 900-item restore over one of
			// them is the wrong trade; this was the aggravating half of the
			// round-10 finding, and it was mine, not the pre-existing mapping.
			slog.Warn("workspace import: skipping reminder that failed to insert",
				"workspace_id", ws.ID, "item_id", newItemID, "error", err)
			continue
		}
	}

	// Import item versions
	for _, ver := range data.ItemVersions {
		newItemID := itemMap[ver.ItemID]
		if newItemID == "" {
			continue
		}
		// version_seq (BUG-2270): re-derive a per-item monotonic seq at
		// import time. data.ItemVersions is exported ORDER BY created_at,
		// version_seq, so COALESCE(MAX,0)+1 reassigns 1,2,3… in that same
		// deterministic order and imported same-second versions keep a
		// stable tie-break instead of all defaulting to 0.
		_, err := tx.Exec(s.q(`
			INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at, version_seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(version_seq), 0) + 1 FROM item_versions WHERE item_id = ?))`),
			newID(), newItemID, ver.Content, ver.ChangeSummary, ver.CreatedBy, ver.Source, s.dialect.BoolToInt(ver.IsDiff),
			ver.CreatedAt, newItemID)
		if err != nil {
			// Log detail but skip — version history is non-critical.
			// Migrated from fmt.Printf to slog.Warn alongside the
			// IDEA-1488 log-and-coerce additions; same severity, same
			// triage signal, but threads through the canonical logger.
			slog.Warn("import_workspace skipped item version",
				"item_id", ver.ItemID,
				"workspace_id", ws.ID,
				"err", err)
			continue
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit import: %w", err)
	}

	// Rebuild FTS indexes for the new workspace (outside transaction)
	s.rebuildFTSForWorkspace(ws.ID)

	return ws, nil
}

// rebuildFTSForWorkspace rebuilds the FTS index for all items in a workspace.
// This is needed after import because direct INSERTs bypass the FTS triggers.
// Only applicable to SQLite (PostgreSQL uses trigger-maintained tsvector columns).
func (s *Store) rebuildFTSForWorkspace(wsID string) {
	if s.dialect.Driver() != DriverSQLite {
		return
	}
	rows, err := s.db.Query(s.q(`SELECT rowid, title, content, tags FROM items WHERE workspace_id = ? AND deleted_at IS NULL`), wsID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var rowid int64
		var title, content, tags string
		if err := rows.Scan(&rowid, &title, &content, &tags); err != nil {
			continue
		}
		s.db.Exec(s.q(`INSERT INTO items_fts(rowid, title, content, tags) VALUES (?, ?, ?, ?)`), rowid, title, content, tags)
	}
}

// remapFieldIDs replaces old UUIDs in a JSON fields string with their new IDs.
// This handles relation fields (e.g. parent: "uuid") without needing to parse the schema.
//
// IDEA-1486: empty-string input is normalized to "{}". Centralizing the
// contract here keeps future callers safe by default — any UPDATE that
// writes the result back into items.fields is guaranteed to satisfy the
// post-migration NOT NULL DEFAULT '{}' invariant. Without this guard,
// the second-pass UPDATE at ImportWorkspace would write "" verbatim on
// items whose original fields were already empty, which silently stores
// invalid JSON on SQLite and (post-migration) would have already 500'd
// on the first-pass INSERT on Postgres if not for the import-boundary
// coercion above.
func remapFieldIDs(fieldsJSON string, itemMap, collMap map[string]string) string {
	if fieldsJSON == "" {
		return "{}"
	}
	result := fieldsJSON
	for oldID, newID := range itemMap {
		if oldID != "" && newID != "" {
			result = strings.ReplaceAll(result, oldID, newID)
		}
	}
	return result
}
