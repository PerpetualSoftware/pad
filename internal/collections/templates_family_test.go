package collections

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func familyField(t *testing.T, c DefaultCollection, key string) models.FieldDef {
	t.Helper()
	for _, f := range c.Schema.Fields {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("collection %q has no %q field", c.Slug, key)
	return models.FieldDef{}
}

func familyCollection(t *testing.T, tmpl *WorkspaceTemplate, slug string) DefaultCollection {
	t.Helper()
	for _, c := range tmpl.Collections {
		if c.Slug == slug {
			return c
		}
	}
	t.Fatalf("template %q has no %q collection", tmpl.Name, slug)
	return DefaultCollection{}
}

func isSubset(sub, of []string) bool {
	set := map[string]bool{}
	for _, o := range of {
		set[o] = true
	}
	for _, s := range sub {
		if !set[s] {
			return false
		}
	}
	return true
}

func TestFamilyTemplates_HiddenAndShape(t *testing.T) {
	cases := []struct {
		name     string
		slugs    []string
		prefixes []string
	}{
		{
			name:     "family-shared",
			slugs:    []string{"groceries", "events", "todo", "notes", "conventions", "playbooks"},
			prefixes: []string{"GROC", "EVT", "TODO", "NOTE"},
		},
		{
			name:     "family-personal",
			slugs:    []string{"notes", "todo", "events", "conventions", "playbooks"},
			prefixes: []string{"NOTE", "TODO", "EVT"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := GetTemplate(tc.name)
			if tmpl == nil {
				t.Fatalf("GetTemplate(%q) returned nil", tc.name)
			}
			if !tmpl.Hidden {
				t.Errorf("%s should be Hidden", tc.name)
			}
			if tmpl.Category != CategoryCustom {
				t.Errorf("%s category = %q, want %q", tc.name, tmpl.Category, CategoryCustom)
			}
			if tmpl.Icon == "" || tmpl.Description == "" {
				t.Errorf("%s needs an Icon and Description", tc.name)
			}
			for _, v := range ListTemplates() {
				if v.Name == tc.name {
					t.Errorf("ListTemplates includes hidden %s", tc.name)
				}
			}
			found := false
			for _, a := range ListAllTemplates() {
				if a.Name == tc.name {
					found = true
				}
			}
			if !found {
				t.Errorf("ListAllTemplates omits %s", tc.name)
			}

			var slugs, prefixes []string
			for i, c := range tmpl.Collections {
				slugs = append(slugs, c.Slug)
				if c.SortOrder != i {
					t.Errorf("%s/%s SortOrder = %d, want %d", tc.name, c.Slug, c.SortOrder, i)
				}
				if c.Slug != "conventions" && c.Slug != "playbooks" {
					prefixes = append(prefixes, c.Prefix)
				}
			}
			if !reflect.DeepEqual(slugs, tc.slugs) {
				t.Errorf("%s slugs = %v, want %v", tc.name, slugs, tc.slugs)
			}
			if !reflect.DeepEqual(prefixes, tc.prefixes) {
				t.Errorf("%s prefixes = %v, want %v", tc.name, prefixes, tc.prefixes)
			}
			if len(tmpl.SeedItems) != 0 || len(tmpl.Conventions) != 0 || len(tmpl.Playbooks) != 0 {
				t.Errorf("%s should seed no items, conventions or playbooks", tc.name)
			}

			conv := familyCollection(t, tmpl, "conventions")
			if got := familyField(t, conv, "trigger").Options; !reflect.DeepEqual(got, BlankConventionTriggers) {
				t.Errorf("%s conventions triggers = %v, want %v", tc.name, got, BlankConventionTriggers)
			}
			pb := familyCollection(t, tmpl, "playbooks")
			if got := familyField(t, pb, "trigger").Options; !reflect.DeepEqual(got, BlankPlaybookTriggers) {
				t.Errorf("%s playbooks triggers = %v, want %v", tc.name, got, BlankPlaybookTriggers)
			}

			for _, c := range tmpl.Collections {
				if c.Slug == "conventions" || c.Slug == "playbooks" {
					continue
				}
				st := familyField(t, c, "status")
				if st.Type != "select" || !st.Required || len(st.Options) == 0 || st.Default != st.Options[0] {
					t.Errorf("%s/%s status = %+v, want a required select defaulting to its first option", tc.name, c.Slug, st)
				}
				if !isSubset(st.TerminalOptions, st.Options) || !isSubset(st.AbandonedOptions, st.TerminalOptions) {
					t.Errorf("%s/%s terminal/abandoned options are not subsets", tc.name, c.Slug)
				}
			}

			for _, slug := range []string{"events", "todo"} {
				st := familyField(t, familyCollection(t, tmpl, slug), "status")
				if !reflect.DeepEqual(st.AbandonedOptions, []string{"cancelled"}) {
					t.Errorf("%s/%s AbandonedOptions = %v, want [cancelled]", tc.name, slug, st.AbandonedOptions)
				}
				if !isSubset(st.AbandonedOptions, st.TerminalOptions) {
					t.Errorf("%s/%s AbandonedOptions not a subset of TerminalOptions", tc.name, slug)
				}
			}

			notes := familyCollection(t, tmpl, "notes")
			if notes.Settings.Layout != "content-primary" {
				t.Errorf("%s notes Layout = %q, want content-primary", tc.name, notes.Settings.Layout)
			}
			if got := familyField(t, notes, "status").TerminalOptions; !reflect.DeepEqual(got, []string{"archived"}) {
				t.Errorf("%s notes terminal = %v, want [archived]", tc.name, got)
			}
		})
	}
}

func TestFamilySharedGroceries(t *testing.T) {
	tmpl := GetTemplate("family-shared")
	if tmpl == nil {
		t.Fatal("family-shared template missing")
	}
	g := familyCollection(t, tmpl, "groceries")
	st := familyField(t, g, "status")
	if !reflect.DeepEqual(st.Options, []string{"needed", "got"}) || !reflect.DeepEqual(st.TerminalOptions, []string{"got"}) || st.Default != "needed" {
		t.Errorf("groceries status = %+v", st)
	}
	store := familyField(t, g, "store")
	if store.Type != "select" || !store.Required || store.Default != "Either" {
		t.Errorf("groceries store = %+v, want required select defaulting to Either", store)
	}
	if !reflect.DeepEqual(store.Options, []string{"Either", "Costco", "Big Y"}) {
		t.Errorf("groceries store options = %v", store.Options)
	}
	if g.Settings.BoardGroupBy != "status" {
		t.Errorf("groceries BoardGroupBy = %q, want status", g.Settings.BoardGroupBy)
	}
	if g.Settings.ListGroupBy != "store" {
		t.Errorf("groceries ListGroupBy = %q, want store", g.Settings.ListGroupBy)
	}
	if g.Settings.DefaultView != "list" {
		t.Errorf("groceries DefaultView = %q, want list", g.Settings.DefaultView)
	}
}

// Both family templates share one schema per collection.
func TestFamilyTemplatesShareSchemas(t *testing.T) {
	shared, personal := GetTemplate("family-shared"), GetTemplate("family-personal")
	if shared == nil || personal == nil {
		t.Fatal("family templates missing")
	}
	for _, slug := range []string{"events", "todo", "notes"} {
		a, b := familyCollection(t, shared, slug), familyCollection(t, personal, slug)
		if !reflect.DeepEqual(a.Schema, b.Schema) || !reflect.DeepEqual(a.Settings, b.Settings) {
			t.Errorf("%s schema/settings differ between family-shared and family-personal", slug)
		}
	}
}
