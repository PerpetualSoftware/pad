package cmdhelp

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// buildSyntheticTree returns a small cobra tree exercising the shapes the
// emitter has to handle: subgroups, leaf commands with positional args,
// flags of every cmdhelp type, repeatable flags, hidden commands, and
// embedded Examples.
//
//	root
//	├── item                    (group)
//	│   ├── create <coll> <title>   (string args + flags of every type)
//	│   └── update <ref>            (single positional)
//	├── deep                    (group, depth>1 to test MaxDepth)
//	│   └── nest                (group)
//	│       └── leaf
//	└── secret                  (hidden)
func buildSyntheticTree() *cobra.Command {
	root := &cobra.Command{
		Use:     "padtest",
		Short:   "Test root",
		Version: "9.9.9",
	}
	root.PersistentFlags().String("workspace", "", "workspace slug override")
	root.PersistentFlags().Int("port", 0, "server port")

	item := &cobra.Command{Use: "item", Short: "item group"}

	create := &cobra.Command{
		Use:     "create <collection> <title> [flags]",
		Short:   "Create a new item",
		Long:    "Create a new item in the specified collection.\n\nLonger description here.",
		Example: "  padtest item create task \"Fix OAuth\" --priority high\n  padtest item create idea \"Realtime\" --tags x,y,z\n  # comment line should be skipped",
	}
	create.Flags().String("priority", "medium", "priority level")
	create.Flags().Bool("stdin", false, "read from stdin")
	create.Flags().Int("retries", 0, "retry count")
	create.Flags().Float64("budget", 0, "budget cap")
	create.Flags().StringSlice("tags", nil, "comma-separated tags")
	create.Flags().StringArray("field", nil, "repeatable --field key=value")
	hiddenFlag := "internal-debug"
	create.Flags().Bool(hiddenFlag, false, "internal use")
	_ = create.Flags().MarkHidden(hiddenFlag)

	update := &cobra.Command{
		Use:   "update <ref>",
		Short: "Update an item",
	}

	// Variadic + embedded flag-like fragments — exercised by bulk:
	//   `bulk-update [--status X] <ref>...`
	bulk := &cobra.Command{
		Use:   "bulk-update [--status X] <ref>...",
		Short: "Update multiple items",
	}

	// Alternation in Use string + ValidArgs on the cobra struct —
	// exercised by `completion [bash|zsh|fish|powershell]`.
	completion := &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion",
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	}

	// ValidArgs without alternation — exercised by `Use: completion2 [shell]`.
	completion2 := &cobra.Command{
		Use:       "completion2 [shell]",
		Short:     "Generate shell completion (named arg)",
		ValidArgs: []string{"bash", "zsh"},
	}

	item.AddCommand(create, update, bulk, completion, completion2)

	deep := &cobra.Command{Use: "deep", Short: "deep group"}
	nest := &cobra.Command{Use: "nest", Short: "nest group"}
	leaf := &cobra.Command{Use: "leaf", Short: "leaf"}
	nest.AddCommand(leaf)
	deep.AddCommand(nest)

	secret := &cobra.Command{Use: "secret", Short: "hidden", Hidden: true}

	root.AddCommand(item, deep, secret)
	return root
}

func TestBuild_TopLevelEnvelope(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{
		Binary:   "padtest",
		Version:  "9.9.9",
		Homepage: "https://example.test",
		MaxDepth: -1,
	})

	if doc.CmdhelpVersion != "0.1" {
		t.Errorf("cmdhelp_version = %q, want 0.1", doc.CmdhelpVersion)
	}
	if doc.Binary != "padtest" {
		t.Errorf("binary = %q, want padtest", doc.Binary)
	}
	if doc.Version != "9.9.9" {
		t.Errorf("version = %q, want 9.9.9", doc.Version)
	}
	if doc.Homepage != "https://example.test" {
		t.Errorf("homepage = %q, want https://example.test", doc.Homepage)
	}
	if doc.Summary != "Test root" {
		t.Errorf("summary = %q, want Test root", doc.Summary)
	}
}

func TestBuild_GlobalFlagsEmittedTopLevelOnly(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})

	// Top-level global_flags should contain workspace + port.
	if _, ok := doc.GlobalFlags["workspace"]; !ok {
		t.Errorf("expected --workspace in global_flags, got: %v", keys(doc.GlobalFlags))
	}
	if _, ok := doc.GlobalFlags["port"]; !ok {
		t.Errorf("expected --port in global_flags, got: %v", keys(doc.GlobalFlags))
	}
	// And NOT duplicated in any per-command flags map.
	for path, cmd := range doc.Commands {
		if _, dup := cmd.Flags["workspace"]; dup {
			t.Errorf("global flag --workspace must not be duplicated on command %q", path)
		}
		if _, dup := cmd.Flags["port"]; dup {
			t.Errorf("global flag --port must not be duplicated on command %q", path)
		}
	}
}

func TestBuild_HiddenCommandsAndFlagsExcluded(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})

	if _, present := doc.Commands["secret"]; present {
		t.Errorf("hidden command 'secret' must not appear in commands map")
	}
	create := doc.Commands["item create"]
	if _, present := create.Flags["internal-debug"]; present {
		t.Errorf("hidden flag --internal-debug must not appear on item create")
	}
	if _, present := create.Flags["help"]; present {
		t.Errorf("cobra-installed --help flag must not appear in emitted flags")
	}
}

func TestBuild_VariadicArgsRepeatable(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})

	bulk := doc.Commands["item bulk-update"]
	if len(bulk.Args) != 1 {
		t.Fatalf("item bulk-update: expected 1 positional arg (embedded --status filtered), got %d: %+v", len(bulk.Args), bulk.Args)
	}
	if bulk.Args[0].Name != "ref" {
		t.Errorf("expected arg name 'ref', got %q", bulk.Args[0].Name)
	}
	if !bulk.Args[0].Required {
		t.Errorf("expected <ref>... to be required")
	}
	if !bulk.Args[0].Repeatable {
		t.Errorf("expected <ref>... to be marked repeatable (variadic)")
	}
}

func TestBuild_AlternationProducesEnum(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})

	completion := doc.Commands["item completion"]
	if len(completion.Args) != 1 {
		t.Fatalf("item completion: expected 1 positional arg, got %d: %+v", len(completion.Args), completion.Args)
	}
	a := completion.Args[0]
	if a.Type != "enum" {
		t.Errorf("expected alternation to produce type=enum, got %q", a.Type)
	}
	if len(a.Enum) != 4 {
		t.Errorf("expected 4 enum values, got %d: %v", len(a.Enum), a.Enum)
	}
	// Values should be in source order.
	wantValues := []string{"bash", "zsh", "fish", "powershell"}
	for i, w := range wantValues {
		if i >= len(a.Enum) || a.Enum[i] != w {
			t.Errorf("enum[%d] = %v, want %s", i, a.Enum[i], w)
		}
	}
}

func TestBuild_ValidArgsFillsEnumOnNamedArg(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})

	c2 := doc.Commands["item completion2"]
	if len(c2.Args) != 1 {
		t.Fatalf("item completion2: expected 1 positional, got %d: %+v", len(c2.Args), c2.Args)
	}
	a := c2.Args[0]
	if a.Name != "shell" {
		t.Errorf("expected arg name from Use string ('shell'), got %q", a.Name)
	}
	if a.Type != "enum" {
		t.Errorf("expected ValidArgs to upgrade type from string to enum, got %q", a.Type)
	}
	if len(a.Enum) != 2 || a.Enum[0] != "bash" || a.Enum[1] != "zsh" {
		t.Errorf("expected enum from ValidArgs [bash zsh], got %v", a.Enum)
	}
}

func TestBuild_EmbeddedFlagFragmentsFiltered(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})

	bulk := doc.Commands["item bulk-update"]
	for _, a := range bulk.Args {
		if strings.HasPrefix(a.Name, "-") {
			t.Errorf("flag-like fragment leaked as positional arg: %+v", a)
		}
	}
}

func TestBuild_PositionalArgsParsed(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})

	create := doc.Commands["item create"]
	if len(create.Args) != 2 {
		t.Fatalf("item create: expected 2 positional args (collection, title), got %d: %+v", len(create.Args), create.Args)
	}
	if create.Args[0].Name != "collection" || !create.Args[0].Required {
		t.Errorf("arg[0] = %+v, want {collection required:true}", create.Args[0])
	}
	if create.Args[1].Name != "title" || !create.Args[1].Required {
		t.Errorf("arg[1] = %+v, want {title required:true}", create.Args[1])
	}

	// "[flags]" placeholder must not become a positional arg.
	for _, a := range create.Args {
		if a.Name == "flags" || a.Name == "options" {
			t.Errorf("positional arg %q leaked from cobra placeholder; should be filtered", a.Name)
		}
	}
}

func TestBuild_FlagTypeMapping(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	create := doc.Commands["item create"]

	tests := []struct {
		name       string
		wantType   string
		repeatable bool
	}{
		{"priority", "string", false},
		{"stdin", "bool", false},
		{"retries", "int", false},
		{"budget", "float", false},
		{"tags", "string", true},  // StringSlice → repeatable string
		{"field", "string", true}, // StringArray → repeatable string
	}
	for _, tt := range tests {
		f, ok := create.Flags[tt.name]
		if !ok {
			t.Errorf("expected flag --%s, missing; have: %v", tt.name, keys(create.Flags))
			continue
		}
		if f.Type != tt.wantType {
			t.Errorf("--%s type = %q, want %q", tt.name, f.Type, tt.wantType)
		}
		if f.Repeatable != tt.repeatable {
			t.Errorf("--%s repeatable = %v, want %v", tt.name, f.Repeatable, tt.repeatable)
		}
	}
}

func TestBuild_ZeroDefaultsSuppressed(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	create := doc.Commands["item create"]

	// stdin defaults to false → suppressed.
	if d := create.Flags["stdin"].Default; d != nil {
		t.Errorf("--stdin default should be suppressed (zero), got %v", d)
	}
	// retries defaults to 0 → suppressed.
	if d := create.Flags["retries"].Default; d != nil {
		t.Errorf("--retries default should be suppressed (zero), got %v", d)
	}
	// priority defaults to "medium" → emitted.
	if d := create.Flags["priority"].Default; d != "medium" {
		t.Errorf("--priority default = %v, want \"medium\"", d)
	}
}

func TestBuild_ExamplesParsed(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	create := doc.Commands["item create"]

	if len(create.Examples) != 2 {
		t.Fatalf("expected 2 examples (comment line filtered), got %d: %+v", len(create.Examples), create.Examples)
	}
	if !strings.Contains(create.Examples[0].Cmd, "Fix OAuth") {
		t.Errorf("first example: %q does not mention OAuth", create.Examples[0].Cmd)
	}
	for _, ex := range create.Examples {
		if strings.HasPrefix(ex.Cmd, "#") {
			t.Errorf("comment line leaked through as example: %q", ex.Cmd)
		}
		if ex.Cmd != strings.TrimSpace(ex.Cmd) {
			t.Errorf("example not whitespace-trimmed: %q", ex.Cmd)
		}
	}
}

func TestBuild_DescriptionVsSummary(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	create := doc.Commands["item create"]

	if create.Summary != "Create a new item" {
		t.Errorf("summary = %q, want Short value", create.Summary)
	}
	if !strings.Contains(create.Description, "Longer description here") {
		t.Errorf("description missing or wrong: %q", create.Description)
	}
}

func TestBuild_CommandPathKeysAreSpaceJoined(t *testing.T) {
	root := buildSyntheticTree()
	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})

	wantPaths := []string{"item", "item create", "item update", "deep", "deep nest", "deep nest leaf"}
	for _, p := range wantPaths {
		if _, ok := doc.Commands[p]; !ok {
			t.Errorf("expected command path %q in commands map; have: %v", p, keys(doc.Commands))
		}
	}
	// Binary itself should not appear as a command entry.
	if _, present := doc.Commands["padtest"]; present {
		t.Errorf("root binary 'padtest' must not be a command entry")
	}
	if _, present := doc.Commands[""]; present {
		t.Errorf("empty-string key must not be a command entry")
	}
}

func TestBuild_MaxDepthLimitsWalk(t *testing.T) {
	root := buildSyntheticTree()

	// MaxDepth=0 from root: emit only depth-1 children (immediate children of root).
	doc0 := Build(root, root, Options{Binary: "padtest", MaxDepth: 0})
	if _, ok := doc0.Commands["item"]; !ok {
		t.Errorf("MaxDepth=0 should still emit immediate children of root")
	}
	if _, ok := doc0.Commands["item create"]; ok {
		t.Errorf("MaxDepth=0 should not recurse to grandchildren")
	}

	// MaxDepth=1: include grandchildren.
	doc1 := Build(root, root, Options{Binary: "padtest", MaxDepth: 1})
	if _, ok := doc1.Commands["item create"]; !ok {
		t.Errorf("MaxDepth=1 should include grandchildren like 'item create'")
	}
	if _, ok := doc1.Commands["deep nest leaf"]; ok {
		t.Errorf("MaxDepth=1 should not include depth-3 leaves")
	}

	// MaxDepth=-1: unlimited.
	docAll := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	if _, ok := docAll.Commands["deep nest leaf"]; !ok {
		t.Errorf("MaxDepth=-1 should walk the full tree")
	}
}

func TestBuild_TargetSubtree(t *testing.T) {
	root := buildSyntheticTree()
	itemGroup, _, err := root.Find([]string{"item"})
	if err != nil || itemGroup == nil {
		t.Fatalf("unable to locate item subtree: %v", err)
	}
	doc := Build(itemGroup, root, Options{Binary: "padtest", MaxDepth: -1})

	// Should contain item subtree.
	if _, ok := doc.Commands["item create"]; !ok {
		t.Errorf("targeted subtree should include 'item create'")
	}
	if _, ok := doc.Commands["item update"]; !ok {
		t.Errorf("targeted subtree should include 'item update'")
	}
	// Should NOT contain unrelated subtrees.
	if _, ok := doc.Commands["deep"]; ok {
		t.Errorf("targeted subtree should not include 'deep' from sibling subtree")
	}
	if _, ok := doc.Commands["deep nest leaf"]; ok {
		t.Errorf("targeted subtree should not include 'deep nest leaf'")
	}
}

func TestEmitJSON_ProducesValidJSON(t *testing.T) {
	root := buildSyntheticTree()
	var buf bytes.Buffer
	if err := EmitJSON(root, root, Options{Binary: "padtest", MaxDepth: -1}, &buf); err != nil {
		t.Fatalf("EmitJSON: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("emitted output is not valid JSON: %v\n%s", err, buf.String())
	}
	// Required top-level keys per cmdhelp v0.1 §3.
	for _, k := range []string{"cmdhelp_version", "binary", "commands"} {
		if _, ok := parsed[k]; !ok {
			t.Errorf("emitted JSON missing required top-level key %q", k)
		}
	}
}

func TestEmitJSON_VersionPatternMatchesSpec(t *testing.T) {
	// Spec §9: cmdhelp_version is MAJOR.MINOR — never includes a PATCH.
	versionRE := regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	if !versionRE.MatchString(Version) {
		t.Errorf("Version constant %q must match %s (no PATCH per spec §9)", Version, versionRE)
	}
}

func TestExitCode_TerseMarshalsAsString(t *testing.T) {
	code := ExitCode{Description: "ok"}
	out, err := json.Marshal(code)
	if err != nil {
		t.Fatalf("marshal terse exit code: %v", err)
	}
	if string(out) != `"ok"` {
		t.Errorf("terse form should marshal as bare string, got: %s", out)
	}
}

func TestExitCode_RichMarshalsAsObject(t *testing.T) {
	code := ExitCode{When: "validation error", Recovery: "Check args"}
	out, err := json.Marshal(code)
	if err != nil {
		t.Fatalf("marshal rich exit code: %v", err)
	}
	// Must be an object, must contain "when".
	var asObj map[string]interface{}
	if err := json.Unmarshal(out, &asObj); err != nil {
		t.Fatalf("rich form not an object: %v", err)
	}
	if asObj["when"] != "validation error" {
		t.Errorf("rich form missing/wrong 'when': %s", out)
	}
	if asObj["recovery"] != "Check args" {
		t.Errorf("rich form missing/wrong 'recovery': %s", out)
	}
}

func TestParseExamples_FiltersBlankAndComments(t *testing.T) {
	in := "  \n  cmd one\n# a comment\n\n  cmd two\n  # also a comment\n"
	got := parseExamples(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 examples, got %d: %+v", len(got), got)
	}
	if got[0].Cmd != "cmd one" || got[1].Cmd != "cmd two" {
		t.Errorf("got %+v, want [cmd one, cmd two]", got)
	}
}

func TestMapPflagType_CoversCommonTypes(t *testing.T) {
	cases := map[string]struct {
		want       string
		repeatable bool
	}{
		"string":      {"string", false},
		"int":         {"int", false},
		"int64":       {"int", false},
		"float64":     {"float", false},
		"bool":        {"bool", false},
		"duration":    {"duration", false},
		"stringSlice": {"string", true},
		"stringArray": {"string", true},
		"boolSlice":   {"bool", true},
		"weirdType":   {"string", false}, // fallback
	}
	for in, want := range cases {
		got, rep := mapPflagType(in)
		if got != want.want || rep != want.repeatable {
			t.Errorf("mapPflagType(%q) = (%q, %v), want (%q, %v)", in, got, rep, want.want, want.repeatable)
		}
	}
}

func keys(m interface{}) []string {
	switch v := m.(type) {
	case map[string]Flag:
		out := make([]string, 0, len(v))
		for k := range v {
			out = append(out, k)
		}
		return out
	case map[string]Command:
		out := make([]string, 0, len(v))
		for k := range v {
			out = append(out, k)
		}
		return out
	}
	return nil
}
