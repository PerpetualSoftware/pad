package cmdhelp

import (
	"errors"
	"testing"
)

// fakeResolver builds a Resolver that pulls from in-memory fixtures.
// It tracks how many times each Source func is called so we can assert
// caching: even when many commands reference the same enum_source, the
// Source MUST execute at most once per Apply.
type fakeResolver struct {
	collectionsCalls int
	rolesCalls       int

	collections []interface{}
	roles       []interface{}

	rolesError error
}

func (f *fakeResolver) build(workspace string) *Resolver {
	return &Resolver{
		Workspace: workspace,
		ArgEnumSources: map[string]string{
			"collection": EnumSourceCollections,
		},
		FlagEnumSources: map[string]string{
			"role": EnumSourceRoles,
		},
		Sources: map[string]DynamicEnum{
			EnumSourceCollections: func() ([]interface{}, error) {
				f.collectionsCalls++
				return f.collections, nil
			},
			EnumSourceRoles: func() ([]interface{}, error) {
				f.rolesCalls++
				if f.rolesError != nil {
					return nil, f.rolesError
				}
				return f.roles, nil
			},
		},
	}
}

// docWith constructs a minimal Document with two commands sharing a
// `collection` arg (caching test) and one `role` flag.
func docWith() *Document {
	return &Document{
		CmdhelpVersion: Version,
		Binary:         "padtest",
		Commands: map[string]Command{
			"item create": {
				Summary: "create",
				Args:    []Arg{{Name: "collection", Type: "string", Required: true}},
				Flags:   map[string]Flag{"role": {Type: "string"}},
			},
			"item update": {
				Summary: "update",
				Args:    []Arg{{Name: "collection", Type: "string", Required: true}},
			},
			"unrelated": {
				Summary: "no dynamic args here",
				Args:    []Arg{{Name: "title", Type: "string"}},
			},
		},
	}
}

func TestResolver_Apply_PopulatesEnumOnArgs(t *testing.T) {
	fake := &fakeResolver{
		collections: []interface{}{"tasks", "ideas", "plans"},
	}
	r := fake.build("docapp")

	doc := docWith()
	r.Apply(doc)

	for _, path := range []string{"item create", "item update"} {
		got := doc.Commands[path].Args[0]
		if got.EnumSource != EnumSourceCollections {
			t.Errorf("%s: enum_source = %q, want %q", path, got.EnumSource, EnumSourceCollections)
		}
		if got.Type != "enum" {
			t.Errorf("%s: type = %q, want enum", path, got.Type)
		}
		if len(got.Enum) != 3 {
			t.Errorf("%s: expected 3 enum values, got %d", path, len(got.Enum))
		}
	}
}

func TestResolver_Apply_PopulatesEnumOnFlags(t *testing.T) {
	fake := &fakeResolver{
		roles: []interface{}{"planner", "implementer"},
	}
	r := fake.build("docapp")

	doc := docWith()
	r.Apply(doc)

	got := doc.Commands["item create"].Flags["role"]
	if got.EnumSource != EnumSourceRoles {
		t.Errorf("enum_source = %q, want %q", got.EnumSource, EnumSourceRoles)
	}
	if got.Type != "enum" {
		t.Errorf("type = %q, want enum", got.Type)
	}
	if len(got.Enum) != 2 {
		t.Errorf("expected 2 enum values, got %d", len(got.Enum))
	}
}

func TestResolver_Apply_PopulatesContext(t *testing.T) {
	fake := &fakeResolver{}
	r := fake.build("docapp")

	doc := docWith()
	r.Apply(doc)

	if doc.Context == nil {
		t.Fatalf("expected Context to be populated")
	}
	if doc.Context.Workspace != "docapp" {
		t.Errorf("workspace = %q, want docapp", doc.Context.Workspace)
	}
}

func TestResolver_Apply_CachesPerSource(t *testing.T) {
	// Two commands share the `collection` arg. The Source func MUST run
	// at most once per Apply call regardless of how many commands need it.
	fake := &fakeResolver{collections: []interface{}{"tasks"}}
	r := fake.build("docapp")

	doc := docWith()
	r.Apply(doc)

	if fake.collectionsCalls != 1 {
		t.Errorf("collections source called %d times, want 1 (caching broken)", fake.collectionsCalls)
	}
	// Second apply restarts cache; another invocation = +1 call.
	r.Apply(doc)
	if fake.collectionsCalls != 2 {
		t.Errorf("after 2nd Apply, collections source called %d times, want 2", fake.collectionsCalls)
	}
}

func TestResolver_Apply_GracefulOnError(t *testing.T) {
	// Source returning error → enum_source is still announced, Enum is
	// left empty, and no other arg/flag is affected. The help command
	// MUST NOT fail because dynamic resolution failed.
	fake := &fakeResolver{
		collections: []interface{}{"tasks"},
		rolesError:  errors.New("server unreachable"),
	}
	r := fake.build("docapp")

	doc := docWith()
	r.Apply(doc) // must not panic / return error (Apply has no error)

	// Successful source still resolves.
	if got := doc.Commands["item create"].Args[0]; len(got.Enum) == 0 {
		t.Errorf("collections enum should still resolve when an unrelated source fails")
	}
	// Failed source: announces enum_source but leaves Enum empty AND
	// keeps the type as the original "string" (no upgrade without values).
	role := doc.Commands["item create"].Flags["role"]
	if role.EnumSource != EnumSourceRoles {
		t.Errorf("expected enum_source set even when resolution fails, got %q", role.EnumSource)
	}
	if len(role.Enum) != 0 {
		t.Errorf("expected empty Enum when resolution failed, got %v", role.Enum)
	}
	if role.Type != "string" {
		t.Errorf("type should remain string when no values resolved, got %q", role.Type)
	}
}

func TestResolver_Apply_NilIsNoOp(t *testing.T) {
	// Callers MUST be able to pass a nil Resolver via Options to disable
	// dynamic resolution entirely. The function call is what matters,
	// not the value.
	doc := docWith()
	var r *Resolver
	r.Apply(doc) // must not panic

	if doc.Commands["item create"].Args[0].EnumSource != "" {
		t.Errorf("nil Resolver must not stamp enum_source")
	}
	if doc.Context != nil {
		t.Errorf("nil Resolver must not populate Context")
	}
}

func TestResolver_Apply_PreservesExistingEnum(t *testing.T) {
	// When an arg already has Enum from alternation/ValidArgs, dynamic
	// resolution must NOT replace it. EnumSource is still stamped (so
	// the binding is announced) but the original values win — they are
	// the spec for that arg, not a snapshot.
	fake := &fakeResolver{collections: []interface{}{"would-replace"}}
	r := fake.build("docapp")

	doc := docWith()
	cmd := doc.Commands["item create"]
	cmd.Args[0].Enum = []interface{}{"locked"}
	cmd.Args[0].Type = "enum"
	doc.Commands["item create"] = cmd

	r.Apply(doc)

	got := doc.Commands["item create"].Args[0]
	if len(got.Enum) != 1 || got.Enum[0] != "locked" {
		t.Errorf("dynamic resolution should not replace existing Enum, got %v", got.Enum)
	}
	// EnumSource is still announced for transparency.
	if got.EnumSource != EnumSourceCollections {
		t.Errorf("expected enum_source to be set even when Enum is preserved, got %q", got.EnumSource)
	}
}

func TestResolver_Apply_GlobalFlags(t *testing.T) {
	// Global flags participate in resolution the same way per-command flags do.
	fake := &fakeResolver{roles: []interface{}{"planner"}}
	r := fake.build("docapp")

	doc := docWith()
	doc.GlobalFlags = map[string]Flag{"role": {Type: "string"}}
	r.Apply(doc)

	got := doc.GlobalFlags["role"]
	if got.EnumSource != EnumSourceRoles {
		t.Errorf("global flag --role: enum_source = %q, want %q", got.EnumSource, EnumSourceRoles)
	}
	if got.Type != "enum" || len(got.Enum) != 1 {
		t.Errorf("global flag --role: expected enum-typed with 1 value, got type=%q enum=%v", got.Type, got.Enum)
	}
}

func TestResolver_Apply_UnaffectedCommandsUnchanged(t *testing.T) {
	// A command with no matching arg/flag names must be untouched.
	fake := &fakeResolver{collections: []interface{}{"tasks"}}
	r := fake.build("docapp")

	doc := docWith()
	r.Apply(doc)

	un := doc.Commands["unrelated"]
	if un.Args[0].EnumSource != "" {
		t.Errorf("unrelated arg should not be touched, got enum_source = %q", un.Args[0].EnumSource)
	}
	if un.Args[0].Type != "string" {
		t.Errorf("unrelated arg type should remain string, got %q", un.Args[0].Type)
	}
}

func TestBuild_AppliesResolver(t *testing.T) {
	// End-to-end via Build(): when Options.Resolver is set, the emitted
	// Document must reflect both the static walk AND dynamic resolution.
	fake := &fakeResolver{collections: []interface{}{"tasks", "ideas"}}

	root := buildSyntheticTree()
	doc := Build(root, root, Options{
		Binary: "padtest",
		Resolver: &Resolver{
			Workspace: "fixture",
			ArgEnumSources: map[string]string{
				"collection": EnumSourceCollections,
			},
			Sources: map[string]DynamicEnum{
				EnumSourceCollections: func() ([]interface{}, error) {
					fake.collectionsCalls++
					return fake.collections, nil
				},
			},
		},
		MaxDepth: -1,
	})

	create := doc.Commands["item create"]
	if create.Args[0].EnumSource != EnumSourceCollections {
		t.Errorf("Build did not apply Resolver to args; enum_source = %q", create.Args[0].EnumSource)
	}
	if doc.Context == nil || doc.Context.Workspace != "fixture" {
		t.Errorf("Build did not apply Resolver to context; got %+v", doc.Context)
	}
}
