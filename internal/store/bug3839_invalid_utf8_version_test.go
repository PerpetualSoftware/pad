package store_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3839: go-diff cannot patch text that is not valid UTF-8. Near an edit it
// either panics (slice bounds out of range) or builds a patch that does not
// reproduce the version: "hello world" -> "hello \xff world" reverse-applied
// to "hello d". Every version writer now stores such a version as its full
// body. Requests cannot deliver invalid UTF-8 (encoding/json coerces it), but
// SQLite keeps it in rows an older release or a direct edit wrote, and the
// store writes whatever string it is given.
//
// Postgres refuses invalid UTF-8 in a text column, so there the body write
// itself fails, before any version is built: the PG legs pin that the store
// returns that error, does not panic, and leaves the item as it was.

// bug3839Long is a body long enough that a small edit is stored as a patch.
func bug3839Long(tail string) string {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("Paragraph about the deployment pipeline and its careful steps.\n\n")
	}
	b.WriteString(tail)
	return b.String()
}

// bug3839Shapes are the two failure shapes, as (before, after) bodies: the
// invalid byte adjacent to an edit, which panicked, and an invalid byte
// inserted into valid text, which built a wrong patch.
var bug3839Shapes = []struct {
	name          string
	before, after string
}{
	{"panic", bug3839Long("a\xff"), bug3839Long("a\xffb")},
	{"wrong_patch", bug3839Long("hello world"), bug3839Long("hello \xff world")},
}

// newestVersion is the newest item version row's stored form.
func newestItemVersion(t *testing.T, s *store.Store, itemID string) (isDiff bool) {
	t.Helper()
	if err := s.DB().QueryRow(`SELECT is_diff FROM item_versions WHERE item_id = $1 ORDER BY created_at DESC, version_seq DESC LIMIT 1`, itemID).Scan(&isDiff); err != nil {
		t.Fatal(err)
	}
	return isDiff
}

// resolvesTo requires some item version to resolve to exactly want.
func itemVersionResolvesTo(t *testing.T, s *store.Store, itemID, current, want string) {
	t.Helper()
	vs, err := s.ListItemVersionsResolved(itemID, current)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Content == want && !v.Unreconstructable {
			return
		}
	}
	got := make([]string, 0, len(vs))
	for _, v := range vs {
		got = append(got, v.Content[max(0, len(v.Content)-20):])
	}
	t.Fatalf("no version resolves to the body it records; version tails: %q", got)
}

// seedItemBody creates an item with a valid body, then sets the stored body
// to body directly, as a row an older release left behind. ok is false on
// Postgres, which refuses the write; that refusal is pinned.
func seedItemBody(t *testing.T, s *store.Store, pg bool, body string) (*models.Item, bool) {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3839"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Docs", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Content: strings.ToValidUTF8(body, "?"), Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().Exec(`UPDATE items SET content = $1 WHERE id = $2`, body, item.ID)
	if pg {
		if utf8.ValidString(body) {
			if err != nil {
				t.Fatal(err)
			}
			return item, true
		}
		if err == nil {
			t.Fatal("Postgres stored a body that is not valid UTF-8")
		}
		return item, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return item, true
}

// UpdateItem (items.go, the version row for an edit) on both shapes.
func TestBUG3839_ItemEditOfInvalidUTF8StoresTheFullBody(t *testing.T) {
	for _, backend := range contentBackends() {
		for _, sh := range bug3839Shapes {
			t.Run(backend.name+"/"+sh.name, func(t *testing.T) {
				s := backend.open(t)
				pg := backend.name == "Postgres"
				item, seeded := seedItemBody(t, s, pg, sh.before)
				if !seeded {
					return // Postgres refused the stored body: the shape cannot exist there
				}
				after := sh.after
				_, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &after, LastModifiedBy: "alice", Source: "cli"})
				if pg {
					if err == nil {
						t.Fatal("Postgres accepted a body that is not valid UTF-8")
					}
					got, gerr := s.GetItem(item.ID)
					if gerr != nil || got.Content != sh.before {
						t.Fatalf("the refused write changed the item: %v", gerr)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if newestItemVersion(t, s, item.ID) {
					t.Fatal("the version of a body that is not valid UTF-8 was stored as a patch")
				}
				itemVersionResolvesTo(t, s, item.ID, after, sh.before)
			})
		}
	}
}

// rebaseNewestVersionPatch (items.go): a throttled edit rebases the newest
// version's patch onto the new body, which is where the invalid text lands.
func TestBUG3839_ThrottledEditOntoInvalidUTF8RebasesToTheFullBody(t *testing.T) {
	for _, backend := range contentBackends() {
		for _, sh := range bug3839Shapes {
			t.Run(backend.name+"/"+sh.name, func(t *testing.T) {
				s := backend.open(t)
				pg := backend.name == "Postgres"
				a := bug3839Long("first draft")
				item, _ := seedItemBody(t, s, pg, a)
				write := func(body string) (*models.Item, error) {
					return s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, LastModifiedBy: "alice", Source: "web"})
				}
				// First edit: a version of a, stored as a patch.
				if _, err := write(strings.ToValidUTF8(sh.before, "?")); err != nil {
					t.Fatal(err)
				}
				if !newestItemVersion(t, s, item.ID) {
					t.Fatal("instrument: the first edit's version must be a reverse patch")
				}
				// Second edit, throttled: rebases that version onto sh.after.
				out, err := write(sh.after)
				if pg {
					if err == nil {
						t.Fatal("Postgres accepted a body that is not valid UTF-8")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !out.BodyEditedWithoutVersion {
					t.Fatal("instrument: the second edit must be throttled, so the rebase runs")
				}
				if newestItemVersion(t, s, item.ID) {
					t.Fatal("the rebased version of a body that is not valid UTF-8 was stored as a patch")
				}
				itemVersionResolvesTo(t, s, item.ID, sh.after, a)
			})
		}
	}
}

// UpdateDocument (documents.go), the legacy documents' version row.
func TestBUG3839_DocumentEditOfInvalidUTF8StoresTheFullBody(t *testing.T) {
	for _, backend := range contentBackends() {
		for _, sh := range bug3839Shapes {
			t.Run(backend.name+"/"+sh.name, func(t *testing.T) {
				s := backend.open(t)
				pg := backend.name == "Postgres"
				ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3839"})
				if err != nil {
					t.Fatal(err)
				}
				doc, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Doc", Content: strings.ToValidUTF8(sh.before, "?"), CreatedBy: "user", Source: "web"})
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.DB().Exec(`UPDATE documents SET content = $1 WHERE id = $2`, sh.before, doc.ID)
				if pg {
					if utf8.ValidString(sh.before) {
						if err != nil {
							t.Fatal(err)
						}
					} else {
						if err == nil {
							t.Fatal("Postgres stored a body that is not valid UTF-8")
						}
						return
					}
				} else if err != nil {
					t.Fatal(err)
				}
				after := sh.after
				_, err = s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &after, LastModifiedBy: "user", Source: "cli"})
				if pg {
					if err == nil {
						t.Fatal("Postgres accepted a body that is not valid UTF-8")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				vs, err := s.ListVersionsResolved(doc.ID, after)
				if err != nil || len(vs) == 0 {
					t.Fatalf("versions: %d %v", len(vs), err)
				}
				if vs[0].Content != sh.before {
					t.Fatalf("the newest version resolves to a body it does not record (tail %q)", vs[0].Content[max(0, len(vs[0].Content)-20):])
				}
			})
		}
	}
}

// planVersionChainRemapTx (attachments.go): an import's attachment remap
// re-diffs each version against its remapped successor. A body that is not
// valid UTF-8 reaches it from a stored row: here the item's current body
// carries an invalid byte next to the attachment ref the newest version's
// patch does not touch, as a direct edit or an older release would leave it.
func TestBUG3839_AttachmentRemapOfInvalidUTF8StoresTheFullBody(t *testing.T) {
	oldID, newID := "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000b2"
	ref := func(id string) string { return "![chart](pad-attachment:" + id + ")" }
	for _, backend := range contentBackends() {
		for _, sh := range []struct{ name, bad string }{{"lone_ff", "\xff"}, {"truncated", "\xe2\x82"}} {
			t.Run(backend.name+"/"+sh.name, func(t *testing.T) {
				s := backend.open(t)
				pg := backend.name == "Postgres"
				validCurrent := bug3839Long("See " + ref(oldID) + " and the notes.")
				older := bug3839Long("See " + ref(oldID) + ".")
				item, _ := seedItemBody(t, s, pg, validCurrent)
				if _, err := s.DB().Exec(`DELETE FROM item_versions WHERE item_id = $1`, item.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES ('v1', $1, $2, $3, '2026-10-01T00:00:00Z', 1)`,
					item.ID, bug3839Patch(t, older, validCurrent), true); err != nil {
					t.Fatal(err)
				}
				current := strings.Replace(validCurrent, " and the notes.", sh.bad+" and the notes.", 1)
				_, seeded := seedExisting(t, s, pg, item.ID, current)
				if !seeded {
					return
				}
				if err := s.RemapAttachmentReferencesInWorkspace(item.WorkspaceID, map[string]string{oldID: newID}); err != nil {
					t.Fatal(err)
				}
				vs, err := s.ListItemVersionsResolved(item.ID, strings.ReplaceAll(current, oldID, newID))
				if err != nil || len(vs) != 1 {
					t.Fatalf("versions: %d %v", len(vs), err)
				}
				if strings.Contains(vs[0].Content, oldID) {
					t.Fatal("instrument: the remap did not rewrite the version")
				}
				var isDiff bool
				if err := s.DB().QueryRow(`SELECT is_diff FROM item_versions WHERE id = 'v1'`).Scan(&isDiff); err != nil {
					t.Fatal(err)
				}
				if isDiff && !utf8.ValidString(vs[0].Content) {
					t.Fatal("a version whose body is not valid UTF-8 was re-stored as a patch")
				}
				t.Logf("v1 after remap: is_diff=%v tail %q", isDiff, vs[0].Content[max(0, len(vs[0].Content)-60):])
			})
		}
	}
}

// bug3839Patch is a reverse patch between valid texts, for fixtures.
func bug3839Patch(t *testing.T, older, newer string) string {
	t.Helper()
	p, ok := diff.ReversePatch(older, newer)
	if !ok {
		t.Fatal("fixture texts must be valid UTF-8")
	}
	return p
}

// seedExisting sets an existing item's stored body directly; false on
// Postgres for a body that is not valid UTF-8 (refused, as seedItemBody).
func seedExisting(t *testing.T, s *store.Store, pg bool, itemID, body string) (string, bool) {
	t.Helper()
	_, err := s.DB().Exec(`UPDATE items SET content = $1 WHERE id = $2`, body, itemID)
	if pg && !utf8.ValidString(body) {
		if err == nil {
			t.Fatal("Postgres stored a body that is not valid UTF-8")
		}
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return body, true
}

// bug3839Unmade is a patch applied over a base it was not made from, valid
// UTF-8 throughout, on which go-diff's PatchApply panics (found by the
// BUG-3839 measurement; TestBUG3839_ApplyPatchOverAnotherValidBaseNeverPanics
// is the population). The version walk applies a patch over another base
// whenever it skips a row.
var bug3839Unmade = struct{ from, to, base string }{"a\né+abb😀", "%a+a😀ab", "é😀ab😀+%"}

// seedDiffVersion replaces the item's history with one unhashed diff row.
func seedDiffVersion(t *testing.T, s *store.Store, itemID, patch string) {
	t.Helper()
	if _, err := s.DB().Exec(`DELETE FROM item_versions WHERE item_id = $1`, itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES ('v1', $1, $2, $3, '2026-10-01T00:00:00Z', 1)`,
		itemID, patch, true); err != nil {
		t.Fatal(err)
	}
}

// History (ListItemVersionsResolved) over a diff row whose patch go-diff
// cannot apply: the current body is not valid UTF-8, or (both backends) the
// patch was made from another base. The row reads as patch_failed instead of
// panicking the request.
func TestBUG3839_ItemHistoryRefusesARowGoDiffCannotApply(t *testing.T) {
	valid := bug3839Long("See the notes.")
	for _, backend := range contentBackends() {
		for _, sh := range []struct{ name, current, patch string }{
			{"invalid_current", bug3839Long("See the\xe2\x82 notes."), bug3839Patch(t, bug3839Long("See notes."), valid)},
			{"unmade_base", bug3839Unmade.base, bug3839Patch(t, bug3839Unmade.to, bug3839Unmade.from)},
		} {
			t.Run(backend.name+"/"+sh.name, func(t *testing.T) {
				s := backend.open(t)
				pg := backend.name == "Postgres"
				item, _ := seedItemBody(t, s, pg, valid)
				seedDiffVersion(t, s, item.ID, sh.patch)
				if _, seeded := seedExisting(t, s, pg, item.ID, sh.current); !seeded {
					return
				}
				vs, err := s.ListItemVersionsResolved(item.ID, sh.current)
				if err != nil || len(vs) != 1 {
					t.Fatalf("versions: %d %v", len(vs), err)
				}
				if !vs[0].Unreconstructable || vs[0].UnreconstructableReason != "patch_failed" {
					t.Fatalf("row reads unreconstructable=%v reason=%q content tail %q", vs[0].Unreconstructable, vs[0].UnreconstructableReason, vs[0].Content[max(0, len(vs[0].Content)-40):])
				}
			})
		}
	}
}

// The legacy documents' walk (ListVersionsResolved) over the same rows.
func TestBUG3839_DocumentHistoryRefusesARowGoDiffCannotApply(t *testing.T) {
	valid := bug3839Long("See the notes.")
	for _, backend := range contentBackends() {
		for _, sh := range []struct{ name, current, patch string }{
			{"invalid_current", bug3839Long("See the\xe2\x82 notes."), bug3839Patch(t, bug3839Long("See notes."), valid)},
			{"unmade_base", bug3839Unmade.base, bug3839Patch(t, bug3839Unmade.to, bug3839Unmade.from)},
		} {
			t.Run(backend.name+"/"+sh.name, func(t *testing.T) {
				s := backend.open(t)
				if backend.name == "Postgres" && !utf8.ValidString(sh.current) {
					return // Postgres cannot hold the body (seedItemBody pins the refusal)
				}
				ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3839"})
				if err != nil {
					t.Fatal(err)
				}
				doc, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Doc", Content: valid, CreatedBy: "user", Source: "web"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`DELETE FROM versions WHERE document_id = $1`, doc.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`INSERT INTO versions (id, document_id, content, is_diff, created_at) VALUES ('dv1', $1, $2, $3, '2026-10-01T00:00:00Z')`,
					doc.ID, sh.patch, true); err != nil {
					t.Fatal(err)
				}
				vs, err := s.ListVersionsResolved(doc.ID, sh.current)
				if err != nil || len(vs) != 1 {
					t.Fatalf("versions: %d %v", len(vs), err)
				}
				if !strings.HasPrefix(vs[0].Content, "[patch error: ") {
					t.Fatalf("row resolved to %q", vs[0].Content[max(0, len(vs[0].Content)-40):])
				}
			})
		}
	}
}

// rebaseNewestVersionPatch (items.go) applies the newest version's patch to
// the stored body before a throttled edit replaces it. When go-diff cannot
// apply it, the edit lands and the row is left as it was.
func TestBUG3839_ThrottledEditOverARowGoDiffCannotApplyLeavesTheRow(t *testing.T) {
	valid := bug3839Long("See the notes.")
	for _, backend := range contentBackends() {
		for _, sh := range []struct{ name, current, patch string }{
			{"invalid_current", bug3839Long("See the\xe2\x82 notes."), bug3839Patch(t, bug3839Long("See notes."), valid)},
			{"unmade_base", bug3839Unmade.base, bug3839Patch(t, bug3839Unmade.to, bug3839Unmade.from)},
		} {
			t.Run(backend.name+"/"+sh.name, func(t *testing.T) {
				s := backend.open(t)
				pg := backend.name == "Postgres"
				item, _ := seedItemBody(t, s, pg, valid)
				write := func(body string) (*models.Item, error) {
					return s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, LastModifiedBy: "alice", Source: "web"})
				}
				// A first edit makes alice's version row, so the next is throttled.
				if _, err := write(bug3839Long("First draft.")); err != nil {
					t.Fatal(err)
				}
				var vid string
				if err := s.DB().QueryRow(`SELECT id FROM item_versions WHERE item_id = $1 ORDER BY created_at DESC, version_seq DESC LIMIT 1`, item.ID).Scan(&vid); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`UPDATE item_versions SET content = $1, is_diff = $2, body_sha256 = NULL WHERE id = $3`, sh.patch, true, vid); err != nil {
					t.Fatal(err)
				}
				if _, seeded := seedExisting(t, s, pg, item.ID, sh.current); !seeded {
					return
				}
				out, err := write(bug3839Long("Second draft."))
				if err != nil {
					t.Fatal(err)
				}
				if !out.BodyEditedWithoutVersion {
					t.Fatal("instrument: the second edit must be throttled, so the rebase runs")
				}
				var content string
				var isDiff bool
				if err := s.DB().QueryRow(`SELECT content, is_diff FROM item_versions WHERE id = $1`, vid).Scan(&content, &isDiff); err != nil {
					t.Fatal(err)
				}
				if content != sh.patch || !isDiff {
					t.Fatalf("the row was rewritten: is_diff=%v", isDiff)
				}
			})
		}
	}
}
