package store

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Item title validation at the store's two write doors (BUG-2833 empty,
// BUG-2831 length).
//
// EVERY test here is DIFFERENTIAL by construction: testStore returns Postgres
// when PAD_TEST_POSTGRES_URL is set and SQLite otherwise, so `make test` and
// `make test-pg` run the same assertions against both backends. That is the
// point rather than a convenience — BUG-2831 IS a dialect split (SQLite
// accepted a title Postgres refused at the UNIQUE(workspace_id, slug) btree),
// so a bound that is only checked on one backend would not close it. The
// assertions below are written so that a dialect-specific refusal cannot pass
// as a validation refusal: they require the typed *InvalidItemTitleError, which
// no driver produces.

// overlongTitle returns a title one rune past the bound. It is built from a
// letter rather than repeated punctuation because slugify drops non-alphanumerics
// — a punctuation title of any length slugifies to "" and would exercise the
// untitled fallback instead of the length rule.
func overlongTitle() string {
	return strings.Repeat("a", models.MaxItemTitleRunes+1)
}

func maxLengthTitle() string {
	return strings.Repeat("b", models.MaxItemTitleRunes)
}

func asInvalidTitle(t *testing.T, err error) *InvalidItemTitleError {
	t.Helper()
	var bad *InvalidItemTitleError
	if !errors.As(err, &bad) {
		t.Fatalf("want *InvalidItemTitleError, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrInvalidItemTitle) {
		t.Errorf("errors.Is(err, ErrInvalidItemTitle) = false; the sentinel must be reachable through Unwrap")
	}
	return bad
}

func TestItemTitle_CreateRefuses(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "TitleCreate")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	cases := []struct {
		name     string
		title    string
		wantWord string
	}{
		{"empty", "", "required"},
		{"spaces only", "   ", "required"},
		// Tab/newline/NBSP: TrimSpace is unicode-aware, so "untitled wearing a
		// costume" is not limited to the space character.
		{"tabs and newlines only", "\t\n\r ", "required"},
		{"unicode space only", "  ", "required"},
		{"one rune over the bound", overlongTitle(), "too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: tc.title})
			if err == nil {
				t.Fatalf("CreateItem(%q) succeeded; want refusal", tc.title)
			}
			bad := asInvalidTitle(t, err)
			if !strings.Contains(strings.ToLower(bad.Reason), tc.wantWord) {
				t.Errorf("Reason = %q, want it to mention %q", bad.Reason, tc.wantWord)
			}
		})
	}
}

// TestItemTitle_UpdateRefusesEmpty is the literal BUG-2833 repro: the filing
// measured UpdateItem(target, {Title: ""}) returning err = nil and the item's
// title becoming empty, while the create path refused the same input.
func TestItemTitle_UpdateRefusesEmpty(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "TitleUpdateEmpty")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Real Title", "body")

	for _, title := range []string{"", "   ", "\t\n"} {
		empty := title
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &empty}); err == nil {
			t.Fatalf("UpdateItem(title=%q) succeeded; want refusal", title)
		} else {
			asInvalidTitle(t, err)
		}
	}

	// The refusal must also leave the row alone. A guard that refuses AFTER
	// writing would satisfy the error assertion above and still have destroyed
	// the title, which is the damage the filing is about.
	after, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Title != "Real Title" {
		t.Errorf("title = %q after refused updates, want it untouched (%q)", after.Title, "Real Title")
	}
	if after.Slug != item.Slug {
		t.Errorf("slug = %q after refused updates, want it untouched (%q)", after.Slug, item.Slug)
	}
}

// TestItemTitle_UpdateRefusesOverlong is BUG-2831 on the rename path. Before
// the bound this update SUCCEEDED on SQLite and failed on Postgres with
// `index row requires N bytes, maximum size is 8191 (SQLSTATE 54000)` reaching
// the handler's generic error arm. Both dialects now refuse it the same way,
// with a typed validation error rather than a driver error.
func TestItemTitle_UpdateRefusesOverlong(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "TitleUpdateLong")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Short", "")

	long := overlongTitle()
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &long})
	if err == nil {
		t.Fatalf("UpdateItem with a %d-rune title succeeded; want refusal", utf8.RuneCountInString(long))
	}
	bad := asInvalidTitle(t, err)
	if !strings.Contains(bad.Reason, "256") || !strings.Contains(bad.Reason, "255") {
		t.Errorf("Reason = %q, want it to name both the supplied length and the maximum", bad.Reason)
	}
}

// TestItemTitle_BoundaryIsInclusive pins the off-by-one in both directions. A
// bound whose accepted side is untested can be tightened by accident and
// nothing fails.
func TestItemTitle_BoundaryIsInclusive(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "TitleBoundary")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	atBound, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: maxLengthTitle()})
	if err != nil {
		t.Fatalf("CreateItem with exactly %d runes must be accepted: %v", models.MaxItemTitleRunes, err)
	}
	if got := utf8.RuneCountInString(atBound.Title); got != models.MaxItemTitleRunes {
		t.Errorf("stored title = %d runes, want %d", got, models.MaxItemTitleRunes)
	}

	// RUNES, not bytes: a 255-rune title of 4-byte runes is 1020 bytes and must
	// still be accepted, or the constant's documented unit is a lie. This also
	// proves the slug derivation survives it — slugify drops non-ASCII, so the
	// slug falls back rather than carrying 1020 bytes into the btree.
	wide := strings.Repeat("𝔞", models.MaxItemTitleRunes)
	if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: wide}); err != nil {
		t.Fatalf("CreateItem with %d runes / %d bytes must be accepted: %v",
			utf8.RuneCountInString(wide), len(wide), err)
	}
}

// TestItemTitle_StoresTheNormalizedValue: validating a trimmed string while
// storing the raw one would leave the row holding something the validator never
// saw. Both doors must persist what they checked.
func TestItemTitle_StoresTheNormalizedValue(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "TitleNormalize")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	created, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "  Padded Title  "})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if created.Title != "Padded Title" {
		t.Errorf("created title = %q, want %q", created.Title, "Padded Title")
	}

	renamed := "\tRenamed Title\n"
	updated, err := s.UpdateItem(created.ID, models.ItemUpdate{Title: &renamed})
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if updated.Title != "Renamed Title" {
		t.Errorf("updated title = %q, want %q", updated.Title, "Renamed Title")
	}

	// Re-read: the normalization has to be in the ROW, not only in the
	// returned struct.
	stored, err := s.GetItem(created.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if stored.Title != "Renamed Title" {
		t.Errorf("stored title = %q, want %q", stored.Title, "Renamed Title")
	}
}

// TestItemTitle_GrandfathersLegacyRows pins the non-retroactive half of the
// rule (Dave's day-63 ruling, carried from MaxDocumentTitleRunes). A row whose
// stored title predates the bound must stay editable; only a genuine rename is
// a write-time door.
func TestItemTitle_GrandfathersLegacyRows(t *testing.T) {
	// RUNS ON BOTH DIALECTS. It used to skip on Postgres, on the stated ground
	// that a legacy over-bound title implies a slug past the btree index-tuple
	// cap — which is true of BUG-2804's 2 MiB fixture and FALSE of this one
	// (codex round 1, P2). overlongTitle() is 256 runes, so the slug it derives
	// is 256 ASCII bytes: an order of magnitude under the 2704 the cap actually
	// enforces. The skip was reasoning by analogy with a different fixture and
	// left the non-retroactive rule untested on the backend BUG-2831 is about.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "TitleGrandfather")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	legacy := createLegacyTitledItem(t, s, ws.ID, col.ID, overlongTitle(), "body")

	// 1. An update that does not touch the title must not validate one.
	newContent := "edited body"
	if _, err := s.UpdateItem(legacy.ID, models.ItemUpdate{Content: &newContent}); err != nil {
		t.Fatalf("content-only update on a legacy-titled row must succeed: %v", err)
	}

	// 2. Echoing the stored title back is not a rename.
	echoed := legacy.Title
	if _, err := s.UpdateItem(legacy.ID, models.ItemUpdate{Title: &echoed}); err != nil {
		t.Fatalf("echoing the stored legacy title must succeed: %v", err)
	}

	// 3. But a genuine rename to ANOTHER invalid title is refused — the
	// grandfathering is for the row's existing value, not a licence.
	another := strings.Repeat("z", models.MaxItemTitleRunes+1)
	if _, err := s.UpdateItem(legacy.ID, models.ItemUpdate{Title: &another}); err == nil {
		t.Fatal("renaming a legacy row to a DIFFERENT over-bound title must be refused")
	} else {
		asInvalidTitle(t, err)
	}

	// 4. And renaming it to a valid title works, so a legacy row is repairable.
	fixed := "Repaired Title"
	got, err := s.UpdateItem(legacy.ID, models.ItemUpdate{Title: &fixed})
	if err != nil {
		t.Fatalf("repairing a legacy row must succeed: %v", err)
	}
	if got.Title != fixed {
		t.Errorf("title = %q, want %q", got.Title, fixed)
	}
}

// TestImportWorkspace_CoercesTitles pins doors 4/5 (ImportWorkspace, and
// `pad db migrate` which is ExportWorkspace piped into it). Import COERCES
// where the interactive doors REFUSE, and this test is the regression guard
// against a later change that "fixes" the asymmetry by making import validate:
// that would turn restoring a legacy archive into a hard failure for data this
// product already accepted.
func TestImportWorkspace_CoercesTitles(t *testing.T) {
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{
		Name:     "Owner",
		Email:    "title-import-owner@example.com",
		Password: "passw0rd!",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	longTitle := strings.Repeat("x", 300)
	export := &models.WorkspaceExport{
		Version:    1,
		ExportedAt: "2026-08-31T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{
			Name: "Legacy Archive",
			Slug: "legacy-archive",
		},
		Collections: []models.CollectionExport{{
			ID: "old-coll-1", Name: "Tasks", Slug: "tasks", Prefix: "TASK",
			Schema:    `{"fields":[]}`,
			CreatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
		}},
		Items: []models.ItemExport{
			{
				ID: "old-empty", CollectionID: "old-coll-1",
				Title: "", Slug: "untitled-legacy",
				Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
			},
			{
				ID: "old-whitespace", CollectionID: "old-coll-1",
				Title: "   ", Slug: "whitespace-legacy",
				Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
			},
			{
				// Over-bound title AND the over-bound slug it implies. The slug
				// is the half that matters on Postgres: this loop writes it
				// verbatim, and it is what carries UNIQUE(workspace_id, slug)
				// into the btree index tuple.
				//
				// HIGH-ENTROPY, not strings.Repeat, and the difference decides
				// whether this fixture reproduces the defect. Index tuples are
				// compressed before the size check, so a 9000-byte slug of one
				// repeated character IMPORTS FINE on Postgres — measured, with
				// the coercion disabled. The same length of poorly-compressible
				// text fails with `index row requires 9056 bytes, maximum size
				// is 8191`. Built from a repeated character, this test would
				// have passed against unfixed code on the backend the bug is
				// about, while still failing the rune-count assertion — green
				// for a reason unrelated to BUG-2831.
				ID: "old-long", CollectionID: "old-coll-1",
				Title: longTitle, Slug: highEntropySlug(9000),
				Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
			},
			{
				ID: "old-fine", CollectionID: "old-coll-1",
				Title: "Perfectly Ordinary", Slug: "perfectly-ordinary",
				Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
			},
		},
	}

	ws, err := s.ImportWorkspace(export, "Legacy Archive Target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace must SUCCEED on a legacy bundle, not refuse it: %v", err)
	}

	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	byOriginal := map[string]models.Item{}
	for _, it := range items {
		byOriginal[it.Title] = it
	}

	// Empty and whitespace-only both land as the coerced title — WITH a title,
	// so Dave's untitled-items invariant holds on this door too.
	untitled := 0
	for _, it := range items {
		if it.Title == "Untitled" {
			untitled++
		}
		if strings.TrimSpace(it.Title) == "" {
			t.Errorf("item %s imported with an empty title %q — no door may write one", it.ID, it.Title)
		}
		if n := utf8.RuneCountInString(it.Title); n > models.MaxItemTitleRunes {
			t.Errorf("item %s imported with a %d-rune title, over the %d bound", it.ID, n, models.MaxItemTitleRunes)
		}
		if n := utf8.RuneCountInString(it.Slug); n > models.MaxItemTitleRunes {
			t.Errorf("item %s imported with a %d-rune slug, over the %d bound — the btree half of BUG-2831 is still open",
				it.ID, n, models.MaxItemTitleRunes)
		}
	}
	if untitled != 2 {
		t.Errorf("want 2 items coerced to %q (the empty one and the whitespace-only one), got %d", "Untitled", untitled)
	}

	// The in-range item is untouched: coercion must not normalize rows that
	// were already fine, or every round-trip rewrites data it had no reason to.
	fine, ok := byOriginal["Perfectly Ordinary"]
	if !ok {
		t.Fatal("the in-range item did not import under its own title")
	}
	if fine.Slug != "perfectly-ordinary" {
		t.Errorf("in-range slug = %q, want it preserved verbatim as %q", fine.Slug, "perfectly-ordinary")
	}
}

// highEntropySlug builds a deterministic, poorly-compressible slug of n bytes
// from a fixed LCG — reproducible across runs and machines, unlike math/rand
// without a pinned seed, and unlike strings.Repeat it survives the compression
// Postgres applies before the btree size check.
//
// The measured thresholds against Postgres 17 with the import coercion
// disabled: 2000 bytes accepted, 4000 refused (`index row size 4056 exceeds
// btree version 4 maximum 2704`), 9000 refused (`index row requires 9056
// bytes, maximum size is 8191`). 9000 is used above so the failure is the
// unambiguous hard-cap one.
func highEntropySlug(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	x := uint64(0x9e3779b97f4a7c15)
	for i := range out {
		x = x*6364136223846793005 + 1442695040888963407
		out[i] = alphabet[(x>>33)%uint64(len(alphabet))]
	}
	return string(out)
}

// TestImportWorkspace_TruncatedSlugCollidesWithALaterVerbatimSlug regresses
// codex round 1 P1.
//
// Truncation is the only way this loop can produce two identical slugs — a
// bundle exported from a live workspace cannot contain a duplicate, because
// UNIQUE(workspace_id, slug) held there. But the duplicate it creates lands in
// either order, and the first fix resolved uniqueness ONLY for the row it had
// truncated. Here the truncated row is inserted FIRST and the untouched row
// that happens to own the resulting slug comes second, so the row that needs
// resolving is the one the fix was not looking at.
//
// The failure mode is the whole import aborting — the opposite of the
// coerce-and-continue policy the title coercion exists to honour.
func TestImportWorkspace_TruncatedSlugCollidesWithALaterVerbatimSlug(t *testing.T) {
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{
		Name: "Owner", Email: "slug-collision-owner@example.com", Password: "passw0rd!",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// A long slug whose first MaxItemTitleRunes bytes are exactly `collides`
	// padded out — so truncation lands on a value a later row already owns.
	prefix := strings.Repeat("c", models.MaxItemTitleRunes)
	longSlug := prefix + strings.Repeat("d", 500)

	export := &models.WorkspaceExport{
		Version: 1, ExportedAt: "2026-08-31T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{Name: "Collide", Slug: "collide"},
		Collections: []models.CollectionExport{{
			ID: "c1", Name: "Tasks", Slug: "tasks", Prefix: "TASK",
			Schema:    `{"fields":[]}`,
			CreatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
		}},
		Items: []models.ItemExport{
			{
				// Inserted FIRST, truncates to `prefix`.
				ID: "long-first", CollectionID: "c1",
				Title: "Long slug row", Slug: longSlug,
				Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
			},
			{
				// Inserted SECOND, and its slug is already legal and untouched —
				// so nothing about THIS row is coerced, yet it is the one that
				// collides.
				ID: "exact-second", CollectionID: "c1",
				Title: "Exact slug row", Slug: prefix,
				Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
			},
		},
	}

	ws, err := s.ImportWorkspace(export, "Collide Target", owner.ID)
	if err != nil {
		t.Fatalf("a slug collision created by OUR truncation must be resolved, not abort the import: %v", err)
	}

	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want both items imported, got %d", len(items))
	}
	seen := map[string]string{}
	for _, it := range items {
		if prev, dup := seen[it.Slug]; dup {
			t.Errorf("duplicate slug %q on %q and %q", it.Slug, prev, it.Title)
		}
		seen[it.Slug] = it.Title
	}
	// Both rows survive AND stay distinguishable — a "fix" that let one
	// overwrite the other would also produce two unique slugs.
	titles := map[string]bool{}
	for _, it := range items {
		titles[it.Title] = true
	}
	for _, want := range []string{"Long slug row", "Exact slug row"} {
		if !titles[want] {
			t.Errorf("item %q did not survive the import", want)
		}
	}
}

// TestItemTitle_GrandfathersAVerbatimEchoOfAWhitespaceTitle regresses codex
// round 1 P2: the exemption is "a title identical to the stored one", and
// comparing only the NORMALIZED form makes that false for exactly the rows the
// exemption exists for.
func TestItemTitle_GrandfathersAVerbatimEchoOfAWhitespaceTitle(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "TitleEchoGrandfather")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// A legacy row that is BOTH over the bound AND carries edge whitespace —
	// the combination is what makes the two comparisons disagree.
	legacyTitle := "  " + strings.Repeat("q", models.MaxItemTitleRunes+1) + "  "
	legacy := createLegacyTitledItem(t, s, ws.ID, col.ID, legacyTitle, "")
	if legacy.Title != legacyTitle {
		t.Fatalf("fixture did not store the title verbatim: %q", legacy.Title)
	}

	// A client that read this item and echoed it back sends the stored bytes.
	echo := legacyTitle
	if _, err := s.UpdateItem(legacy.ID, models.ItemUpdate{Title: &echo}); err != nil {
		t.Fatalf("a verbatim echo of the stored title is not a rename and must be accepted: %v", err)
	}

	// Control: a genuinely different over-bound title is still refused, so the
	// exemption has not been widened into a hole.
	other := "  " + strings.Repeat("r", models.MaxItemTitleRunes+1) + "  "
	if _, err := s.UpdateItem(legacy.ID, models.ItemUpdate{Title: &other}); err == nil {
		t.Error("a DIFFERENT over-bound title must still be refused")
	} else {
		asInvalidTitle(t, err)
	}
}

// TestItemTitle_GrandfathersAWriteThatNormalizesToTheStoredTitle covers the
// OTHER disjunct of the exemption, which the verbatim-echo test above cannot
// see: a stored legacy title with no edge whitespace, sent back with some.
//
// It is not a rename — the value written is byte-identical to what is already
// there once normalized — so refusing it would refuse a no-op, which is the
// same defect class as BUG-2833 in the opposite direction. A mutation dropping
// this disjunct survived the rest of the suite, which is why the case is
// pinned separately rather than folded into the test above.
func TestItemTitle_GrandfathersAWriteThatNormalizesToTheStoredTitle(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "TitleNormalizedEcho")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Stored legacy title is over the bound and already trimmed.
	legacyTitle := strings.Repeat("q", models.MaxItemTitleRunes+1)
	legacy := createLegacyTitledItem(t, s, ws.ID, col.ID, legacyTitle, "")

	padded := "  " + legacyTitle + "\t"
	updated, err := s.UpdateItem(legacy.ID, models.ItemUpdate{Title: &padded})
	if err != nil {
		t.Fatalf("a title that NORMALIZES to the stored one writes nothing and must be accepted: %v", err)
	}
	if updated.Title != legacyTitle {
		t.Errorf("title = %q, want the stored value unchanged", updated.Title)
	}
}
