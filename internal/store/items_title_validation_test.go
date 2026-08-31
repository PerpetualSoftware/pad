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
	if s := testStore(t); s.dialect.Driver() == DriverPostgres {
		t.Skip("a legacy over-bound title cannot exist on Postgres: the slug it implies exceeds the 8191-byte btree index tuple, which is BUG-2831 itself")
	}
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
				// into the 8191-byte btree tuple.
				ID: "old-long", CollectionID: "old-coll-1",
				Title: longTitle, Slug: strings.Repeat("y", 9000),
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
