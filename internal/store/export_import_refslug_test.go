package store

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestImportRefShapedSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
		changed  bool
	}{
		{"IDENT-10", "ident-10-2", true},
		{"release-2", "release-2-2", true},
		{"ident-10", "ident-10-2", true},
		{"a-1", "a-1-2", true},
		{"my-doc", "my-doc", false},
		{"release-2-2", "release-2-2", false},
		{"ABC", "ABC", false},
		{"x_1", "x_1", false},
		{"release-", "release-", false},
		{"-5", "-5", false},
		{"2fa-1", "2fa-1", false},
		{"Mixed-Case-Slug", "Mixed-Case-Slug", false},
		{"ident-99999999999999999999", "ident-99999999999999999999", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, changed := importRefShapedSlug(tc.in)
			if got != tc.want || changed != tc.changed {
				t.Errorf("importRefShapedSlug(%q) = (%q, %t), want (%q, %t)", tc.in, got, changed, tc.want, tc.changed)
			}
			if _, _, ok := parseItemRef(got); changed && ok {
				t.Errorf("derived slug %q still parses as a ref", got)
			}
			if n := utf8.RuneCountInString(got); n > models.MaxItemTitleRunes {
				t.Errorf("output has %d runes, want at most %d", n, models.MaxItemTitleRunes)
			}
		})
	}
}

func TestImportRefShapedSlug_Boundaries(t *testing.T) {
	t.Parallel()
	for _, size := range []int{255, 254, 252} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Parallel()
			in := strings.Repeat("a", size-2) + "-5"
			if _, _, ok := parseItemRef(in); !ok {
				t.Fatal("boundary input must be ref-shaped")
			}
			out, changed := importRefShapedSlug(in)
			if !changed {
				t.Error("ref-shaped boundary input was not changed")
			}
			if n := utf8.RuneCountInString(out); n > models.MaxItemTitleRunes {
				t.Errorf("output has %d runes, want at most 255", n)
			}
			if _, _, ok := parseItemRef(out); ok {
				t.Errorf("output %q still parses as a ref", out)
			}
			if out != strings.ToLower(out) {
				t.Errorf("output %q is not lowercase", out)
			}
		})
	}
}

// testImportRefSlugs uses the shared store harness: SQLite by default and
// Postgres under PAD_TEST_POSTGRES_URL (make test-pg), with no build tag.
func testImportRefSlugs(t *testing.T, slugs ...string) (*Store, *models.Workspace, map[string]models.Item) {
	t.Helper()
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{
		Name: "Owner", Email: "refslug-owner@example.com", Password: "passw0rd!",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	const stamp = "2026-11-06T00:00:00Z"
	bundle := &models.WorkspaceExport{
		Version: 1, ExportedAt: stamp,
		Workspace: models.WorkspaceExportMeta{Name: "Ref slugs", Slug: "ref-slugs"},
		Collections: []models.CollectionExport{{
			ID: "c1", Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[]}`,
			CreatedAt: stamp, UpdatedAt: stamp,
		}},
	}
	for i, slug := range slugs {
		bundle.Items = append(bundle.Items, models.ItemExport{
			ID: fmt.Sprintf("row-%d", i), CollectionID: "c1", ItemNumber: i + 1,
			Title: fmt.Sprintf("Item %d", i), Slug: slug, Fields: `{}`, Tags: `[]`,
			CreatedAt: stamp, UpdatedAt: stamp,
		})
	}
	ws, err := s.ImportWorkspace(bundle, "Ref slugs target", owner.ID, "")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != len(slugs) {
		t.Fatalf("imported %d items, want %d", len(items), len(slugs))
	}
	byTitle := make(map[string]models.Item, len(items))
	for _, item := range items {
		byTitle[item.Title] = item
	}
	for i := range slugs {
		title := fmt.Sprintf("Item %d", i)
		item, ok := byTitle[title]
		if !ok {
			t.Fatalf("import lost title %q", title)
		}
		if item.ItemNumber == nil || *item.ItemNumber != i+1 || item.Ref != fmt.Sprintf("TASK-%d", i+1) {
			t.Errorf("%s: number/ref = %v/%q, want %d/TASK-%d", title, item.ItemNumber, item.Ref, i+1, i+1)
		}
	}
	return s, ws, byTitle
}

func TestImportWorkspace_RefShapedSlugs(t *testing.T) {
	t.Parallel()
	s, ws, items := testImportRefSlugs(t, "IDENT-10", "release-2", "my-doc", "Keep-Upper-Case")
	for i, want := range []string{"ident-10-2", "release-2-2", "my-doc", "Keep-Upper-Case"} {
		if got := items[fmt.Sprintf("Item %d", i)].Slug; got != want {
			t.Errorf("row %d slug = %q, want %q", i, got, want)
		}
	}
	got, err := s.ResolveItem(ws.ID, "ident-10-2")
	if err != nil || got == nil || got.ID != items["Item 0"].ID {
		t.Errorf("ResolveItem(ident-10-2) = %v, %v, want imported Item 0", got, err)
	}
	for _, ref := range []string{"ident-10", "IDENT-10"} {
		got, err := s.ResolveItem(ws.ID, ref)
		if err != nil || got != nil {
			t.Errorf("ResolveItem(%q) = %v, %v, want nil, nil (only TASK refs exist)", ref, got, err)
		}
	}
}

func TestImportWorkspace_RefShapedSlugCollisions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		slugs []string
	}{
		{"derived_first", []string{"release-2", "release-2-2"}},
		{"verbatim_first", []string{"release-2-2", "release-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, items := testImportRefSlugs(t, tc.slugs...)
			// In either order the first row claims release-2-2. The second
			// collides with that claim, so uniqueSlugQ appends its own -2.
			for i, want := range []string{"release-2-2", "release-2-2-2"} {
				got := items[fmt.Sprintf("Item %d", i)].Slug
				if got != want {
					t.Errorf("row %d slug = %q, want %q", i, got, want)
				}
				if _, _, ok := parseItemRef(got); ok {
					t.Errorf("imported slug %q is ref-shaped", got)
				}
			}
			if items["Item 0"].Slug == items["Item 1"].Slug {
				t.Error("imported slugs must be distinct")
			}
		})
	}
}

func TestImportWorkspace_RefShapedSlugOverlong(t *testing.T) {
	t.Parallel()
	_, _, items := testImportRefSlugs(t, strings.Repeat("a", 300)+"-5")
	// Existing length coercion runs first and removes the ref's hyphen.
	out := items["Item 0"].Slug
	if out != strings.Repeat("a", models.MaxItemTitleRunes) {
		t.Errorf("slug = %q, want the existing 255-rune truncation", out)
	}
	if _, _, ok := parseItemRef(out); ok {
		t.Errorf("overlong imported slug %q is still ref-shaped", out)
	}
}

func TestImportWorkspace_RefShapedSlugWarnings(t *testing.T) {
	if os.Getenv("PAD_TEST_REFSLUG_WARN_CHILD") == "1" {
		testImportRefSlugWarnings(t)
		return
	}
	t.Parallel()
	// Isolate the global slog default so parallel tests cannot steal records.
	cmd := exec.Command(os.Args[0], "-test.run=^TestImportWorkspace_RefShapedSlugWarnings$", "-test.v")
	cmd.Env = append(os.Environ(), "PAD_TEST_REFSLUG_WARN_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("warning assertions: %v\n%s", err, out)
	}
}

func testImportRefSlugWarnings(t *testing.T) {
	t.Helper()
	var records []slog.Record
	previous := slog.Default()
	slog.SetDefault(slog.New(&recordCapturingHandler{records: &records}))
	t.Cleanup(func() { slog.SetDefault(previous) })
	// Truncation leaves a 255-rune ref, so this row needs BOTH coercions.
	both := strings.Repeat("A", 253) + "-5" + "0"
	_, ws, items := testImportRefSlugs(t, "IDENT-10", both)
	seen := map[string]map[string]int{}
	for _, record := range records {
		if record.Message != "import_workspace coerced item slug" {
			continue
		}
		attrs := map[string]string{}
		record.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		rowID := attrs["row_id"]
		if seen[rowID] == nil {
			seen[rowID] = map[string]int{}
		}
		seen[rowID][attrs["reason"]]++
		if record.Level != slog.LevelWarn || attrs["workspace_id"] != ws.ID {
			t.Errorf("unexpected warning level/workspace: %v, %v", record.Level, attrs)
		}
		i := 0
		if rowID == "row-1" {
			i = 1
		}
		if attrs["slug"] != items[fmt.Sprintf("Item %d", i)].Slug {
			t.Errorf("warning must name final slug: %v", attrs)
		}
		if attrs["reason"] == "ref_shaped" {
			want := "IDENT-10"
			if i == 1 {
				want = both
			}
			if attrs["requested"] != want {
				t.Errorf("requested = %q, want %q", attrs["requested"], want)
			}
		}
	}
	if seen["row-0"]["ref_shaped"] != 1 || len(seen["row-0"]) != 1 {
		t.Errorf("ref-shaped row warnings = %v, want one ref_shaped", seen["row-0"])
	}
	if seen["row-1"]["ref_shaped"] != 1 || seen["row-1"]["too_long"] != 1 || len(seen["row-1"]) != 2 {
		t.Errorf("doubly coerced row warnings = %v, want one too_long and one ref_shaped", seen["row-1"])
	}
	out := items["Item 1"].Slug
	if n := utf8.RuneCountInString(out); n > models.MaxItemTitleRunes {
		t.Errorf("doubly coerced slug has %d runes, want at most 255", n)
	}
	if _, _, ok := parseItemRef(out); ok {
		t.Errorf("doubly coerced slug %q is still ref-shaped", out)
	}
}

// A collision on a hyphen-free base must not leave the resolved slug
// ref-shaped: two over-long slugs truncate to the same 255 letters, and
// uniqueSlugQ would give the second "aaa...a-2", which parses as a ref.
func TestImportWorkspace_RefShapedSlugAfterCollisionSuffix(t *testing.T) {
	t.Parallel()
	_, _, byTitle := testImportRefSlugs(t,
		strings.Repeat("a", 300), strings.Repeat("a", 301), strings.Repeat("a", 302))
	seen := map[string]bool{}
	for _, title := range []string{"Item 0", "Item 1", "Item 2"} {
		slug := byTitle[title].Slug
		if seen[slug] {
			t.Errorf("duplicate slug for %s", title)
		}
		seen[slug] = true
		if _, _, ok := parseItemRef(slug); ok {
			t.Errorf("%s: slug %q (%d runes) parses as a ref", title, slug[:12], len(slug))
		}
	}
	if got := byTitle["Item 0"].Slug; got != strings.Repeat("a", models.MaxItemTitleRunes) {
		t.Errorf("first row slug = %d runes, want the plain truncation", utf8.RuneCountInString(got))
	}
}

// importRefSlugBundle is a bundle whose slugs include ref-shaped ones, for the
// preserved-slug (pad db migrate-to-pg) and plain-door comparison.
func importRefSlugBundle(slug string) *models.WorkspaceExport {
	const stamp = "2026-11-06T00:00:00Z"
	bundle := &models.WorkspaceExport{
		Version: 1, ExportedAt: stamp,
		Workspace: models.WorkspaceExportMeta{Name: "Ref slugs", Slug: slug},
		Collections: []models.CollectionExport{{
			ID: "c1", Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[]}`,
			CreatedAt: stamp, UpdatedAt: stamp,
		}},
	}
	for i, itemSlug := range []string{"release-2", "IDENT-10", "my-doc"} {
		bundle.Items = append(bundle.Items, models.ItemExport{
			ID: fmt.Sprintf("row-%d", i), CollectionID: "c1", ItemNumber: i + 1,
			Title: fmt.Sprintf("Item %d", i), Slug: itemSlug, Fields: `{}`, Tags: `[]`,
			CreatedAt: stamp, UpdatedAt: stamp,
		})
	}
	return bundle
}

func importedSlugsByTitle(t *testing.T, s *Store, wsID string) map[string]string {
	t.Helper()
	items, err := s.ListItems(wsID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		out[item.Title] = item.Slug
	}
	return out
}

// pad db migrate-to-pg copies a workspace to a new database with
// WithPreservedSlug, and an operator copy keeps every slug as the source has
// it (a slug is already in URLs and bookmarks). Only the plain import doors
// coerce a ref-shaped slug (TASK-4375).
func TestImportWorkspace_PreservedSlugKeepsRefShapedSlugs(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	ws, err := s.ImportWorkspace(importRefSlugBundle("ref-slugs-copy"), "", "", "", WithPreservedSlug())
	if err != nil {
		t.Fatalf("ImportWorkspace (preserved): %v", err)
	}
	got := importedSlugsByTitle(t, s, ws.ID)
	for title, want := range map[string]string{"Item 0": "release-2", "Item 1": "IDENT-10", "Item 2": "my-doc"} {
		if got[title] != want {
			t.Errorf("preserved copy: %s slug = %q, want %q byte for byte", title, got[title], want)
		}
	}
}

func TestImportWorkspace_PlainDoorStillCoercesRefShapedSlugs(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	ws, err := s.ImportWorkspace(importRefSlugBundle("ref-slugs-plain"), "Plain door", "", "")
	if err != nil {
		t.Fatalf("ImportWorkspace (plain): %v", err)
	}
	got := importedSlugsByTitle(t, s, ws.ID)
	for title, want := range map[string]string{"Item 0": "release-2-2", "Item 1": "ident-10-2", "Item 2": "my-doc"} {
		if got[title] != want {
			t.Errorf("plain door: %s slug = %q, want %q", title, got[title], want)
		}
	}
}
