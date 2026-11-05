package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/diff"
)

// TASK-4278: the fixtures the attachment-remap chunking tests share.
//
// remapFixtureIDs are the attachment ids the fixtures rewrite. remapA and
// remapB are mapped; remapC is deliberately NOT in the map, so a reference to
// it must come through every remap untouched.
const (
	remapOldA = "00000000-0000-0000-0000-0000000000a1"
	remapNewA = "00000000-0000-0000-0000-0000000000b1"
	remapOldB = "00000000-0000-0000-0000-0000000000a2"
	remapNewB = "00000000-0000-0000-0000-0000000000b2"
	remapC    = "00000000-0000-0000-0000-0000000000c3"
)

func remapFixtureMap() map[string]string {
	return map[string]string{remapOldA: remapNewA, remapOldB: remapNewB}
}

func remapRef(id string) string { return "![img](pad-attachment:" + id + ")" }

// remapEquivalenceFixture builds a small workspace that exercises every
// surface the remap rewrites: item content, item fields, comment bodies,
// version chains (full rows, reverse patches, a hashed row, a patch that does
// not apply), an unmapped id, a longer id that merely has a mapped id as its
// prefix, and a soft-deleted item whose comment is rewritten while its own
// body is not. Everything is keyed by title so the result is comparable
// across runs and drivers.
func remapEquivalenceFixture(t *testing.T, s *Store) (wsID string) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Remap Equivalence")
	col := createTestCollection(t, s, ws.ID, "Docs")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(s.q(q), args...); err != nil {
			t.Fatalf("fixture exec %q: %v", q, err)
		}
	}
	version := func(itemID, id, content string, isDiff bool, hash string, at string, seq int) {
		t.Helper()
		var h any
		if hash != "" {
			h = hash
		}
		exec(`INSERT INTO item_versions (id, item_id, content, is_diff, body_sha256, created_at, version_seq) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, itemID, content, s.dialect.BoolToInt(isDiff), h, at, seq)
	}
	comment := func(itemID, id, body string) {
		t.Helper()
		exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, itemID, ws.ID, body, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	}
	// Start every item's chain clean: the create path may have minted its own
	// first version, and the fixture wants to own the whole chain.
	clearVersions := func(itemID string) { exec(`DELETE FROM item_versions WHERE item_id = ?`, itemID) }

	plain := createTestItem(t, s, ws.ID, col.ID, "plain", "one "+remapRef(remapOldA)+" two "+remapRef(remapOldB)+" three "+remapRef(remapC))
	_ = plain
	fields := createTestItem(t, s, ws.ID, col.ID, "fields", "no refs in the body")
	exec(`UPDATE items SET fields = ? WHERE id = ?`, `{"status":"open","cover":"pad-attachment:`+remapOldA+`"}`, fields.ID)

	cur := "Draft three:\n\n" + remapRef(remapOldA) + "\n\nend"
	chained := createTestItem(t, s, ws.ID, col.ID, "chained", cur)
	clearVersions(chained.ID)
	full := "A full snapshot.\n\n" + remapRef(remapOldB)
	b2 := full + " More."
	version(chained.ID, "chain-v4", diff.CreateReversePatch("Draft two:\n\n"+remapRef(remapOldA)+"\n\nend", cur), true, "", "2026-01-04T00:00:00Z", 4)
	version(chained.ID, "chain-v3", full, false, versionBodySHA256(full), "2026-01-03T00:00:00Z", 3)
	version(chained.ID, "chain-v2", diff.CreateReversePatch(b2, full), true, "", "2026-01-02T00:00:00Z", 2)
	version(chained.ID, "chain-v1", b2, false, "", "2026-01-01T00:00:00Z", 1)

	broken := createTestItem(t, s, ws.ID, col.ID, "broken", "Body with a table:\n\n| a | b |\n| - | - |")
	clearVersions(broken.ID)
	version(broken.ID, "broken-v2", "@@ -1,12 +1,60 @@\n-Nowhere text\n+"+remapRef(remapOldA)+"\n", true, "", "2026-01-02T00:00:00Z", 2)
	version(broken.ID, "broken-v1", "old "+remapRef(remapOldB), false, "", "2026-01-01T00:00:00Z", 1)

	commented := createTestItem(t, s, ws.ID, col.ID, "commented", "body without refs")
	comment(commented.ID, "cm-a", "see "+remapRef(remapOldA))
	comment(commented.ID, "cm-c", "unmapped "+remapRef(remapC))
	comment(commented.ID, "cm-none", "nothing here")

	prefix := createTestItem(t, s, ws.ID, col.ID, "prefix", "longer id pad-attachment:"+remapOldA+"f and exact pad-attachment:"+remapOldA)
	_ = prefix

	deleted := createTestItem(t, s, ws.ID, col.ID, "deleted", "dead body "+remapRef(remapOldA))
	exec(`UPDATE items SET deleted_at = ? WHERE id = ?`, "2026-01-05T00:00:00Z", deleted.ID)
	comment(deleted.ID, "cm-deleted", "from a deleted item "+remapRef(remapOldB))

	createTestItem(t, s, ws.ID, col.ID, "untouched", "plain body")
	return ws.ID
}

// remapDump is the canonical post-remap state of a workspace: content, fields,
// comment bodies and version rows, keyed by title (item) or a stable id
// (comment, version). seq is left out on purpose: it is a counter, and the
// chunked remap bumps it per item exactly as the single transaction did, but
// the values depend on creation order, not on the remap.
type remapDump struct {
	Items    map[string]string // title -> content \x00 fields
	Comments map[string]string // comment id -> body
	Versions []string          // "title|is_diff|hash|content", in chain order
}

func dumpRemapState(t *testing.T, s *Store, wsID string) remapDump {
	t.Helper()
	d := remapDump{Items: map[string]string{}, Comments: map[string]string{}}
	rows, err := s.db.Query(s.q(`SELECT title, content, fields FROM items WHERE workspace_id = ?`), wsID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var title, content, fields string
		if err := rows.Scan(&title, &content, &fields); err != nil {
			t.Fatal(err)
		}
		// Postgres stores fields as jsonb (key order and spacing normalised);
		// compare the JSON value, not its spelling.
		var fv any
		if err := json.Unmarshal([]byte(fields), &fv); err != nil {
			t.Fatalf("fields of %q are not JSON: %v", title, err)
		}
		fb, _ := json.Marshal(fv)
		d.Items[title] = content + "\x00" + string(fb)
	}
	rows.Close()
	crows, err := s.db.Query(s.q(`SELECT id, body FROM comments WHERE workspace_id = ?`), wsID)
	if err != nil {
		t.Fatal(err)
	}
	for crows.Next() {
		var id, body string
		if err := crows.Scan(&id, &body); err != nil {
			t.Fatal(err)
		}
		d.Comments[id] = body
	}
	crows.Close()
	vrows, err := s.db.Query(s.q(`SELECT i.title, v.is_diff, COALESCE(v.body_sha256, ''), v.content FROM item_versions v
		JOIN items i ON i.id = v.item_id WHERE i.workspace_id = ? ORDER BY i.title, v.created_at, v.version_seq, v.id`), wsID)
	if err != nil {
		t.Fatal(err)
	}
	for vrows.Next() {
		var title, hash, content string
		var isDiff bool
		if err := vrows.Scan(&title, &isDiff, &hash, &content); err != nil {
			t.Fatal(err)
		}
		d.Versions = append(d.Versions, fmt.Sprintf("%s|%t|%s|%s", title, isDiff, hash, content))
	}
	vrows.Close()
	sort.Strings(d.Versions)
	return d
}

// remapGoldenPath holds the dump the SINGLE-TRANSACTION remap (the code before
// TASK-4278) produced for remapEquivalenceFixture. Regenerate only
// from that code: PAD_UPDATE_REMAP_GOLDEN=1 go test ./internal/store -run
// TestRemap_SingleChunkEquivalence on a tree whose remap is one transaction.
func remapGoldenPath() string { return filepath.Join("testdata", "remap_single_tx_golden.json") }

func writeRemapGolden(t *testing.T, d remapDump) {
	t.Helper()
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remapGoldenPath(), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
