package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3769: a workspace slug is slugified whichever door supplied it. An
// import name override, an import bundle's own slug and a create body's slug
// all used to land verbatim; a name slugify keeps nothing of produced "".
func TestBUG3769_EveryDoorSlugifiesTheWorkspaceSlug(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "bug3769@example.com", "Owner", "pw-test-12345")
	src, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Source 3769", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatal(err)
	}

	// The import name override (the reported case).
	ws, err := s.ImportWorkspace(data, "Interchange Golden", owner.ID, "import")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Slug != "interchange-golden" || ws.Name != "Interchange Golden" {
		t.Errorf("override import = slug %q name %q, want interchange-golden / Interchange Golden", ws.Slug, ws.Name)
	}

	// A bundle carrying a slug no export would write, with no override.
	data.Workspace.Slug = "Hand Edited Slug"
	if ws, err = s.ImportWorkspace(data, "", owner.ID, "import"); err != nil {
		t.Fatal(err)
	}
	if ws.Slug != "hand-edited-slug" {
		t.Errorf("bundle slug import = %q, want hand-edited-slug", ws.Slug)
	}

	for _, c := range []struct {
		in   models.WorkspaceCreate
		want string
	}{
		{models.WorkspaceCreate{Name: "Body Slug", Slug: "Foo Bar"}, "foo-bar"},
		{models.WorkspaceCreate{Name: "Kept", Slug: "kept-3769"}, "kept-3769"},
		{models.WorkspaceCreate{Name: "Fallback To Name", Slug: "!!!"}, "fallback-to-name"},
	} {
		c.in.OwnerID = owner.ID
		ws, err := s.CreateWorkspace(c.in)
		if err != nil {
			t.Fatalf("create %+v: %v", c.in, err)
		}
		if ws.Slug != c.want {
			t.Errorf("create name %q slug %q = %q, want %q", c.in.Name, c.in.Slug, ws.Slug, c.want)
		}
		if got, err := s.GetWorkspaceBySlug(c.want); err != nil || got == nil || got.ID != ws.ID {
			t.Errorf("GetWorkspaceBySlug(%q) = %+v, %v; want the created workspace", c.want, got, err)
		}
	}

	// Names slugify keeps nothing of fall back to "workspace", and the second
	// such workspace takes the next suffix. Its own sequence, so it does not
	// depend on the table's order (hunter r1 on BUG-3769).
	for i, name := range []string{"日本語", "!!!"} {
		want := "workspace"
		if i == 1 {
			want = "workspace-2"
		}
		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: owner.ID})
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		if ws.Slug != want {
			t.Errorf("create name %q = slug %q, want %q", name, ws.Slug, want)
		}
	}
}
