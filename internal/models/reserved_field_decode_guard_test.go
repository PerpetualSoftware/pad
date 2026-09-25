package models_test

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

// BUG-2685's enforcement half.
//
// The fix is one predicate — models.UnmarshalItemFieldSchema — applied at every
// site that decodes a collection schema FOR ITEM-FIELD PURPOSES. A predicate
// nothing enforces is a convention the next consumer has to remember, and the
// last four review rounds this bug was filed out of are what remembering looks
// like in practice: each round found another site.
//
// So this test enumerates every raw `json.Unmarshal` into a models.
// CollectionSchema in the tree and requires each to be listed below WITH A
// REASON. A new item-field consumer that decodes raw fails here and is told
// which call to use; a genuine collection-definition site is added to the map
// and the reason is the thing review argues about.
//
// The allow-list is deliberately positional (file + the identifier written to)
// rather than a whole-file exemption: a file may hold both kinds, and
// handlers_collections.go does.
//
// IT IS TYPE-RESOLVED, and that was earned rather than chosen. The first
// version approximated types by reading declarations with go/ast, and FIVE
// consecutive codex rounds each named more DESTINATION SPELLINGS it could not
// see — a bare pointer parameter; an inferred-type var; new(T); an aliased
// models import, `var x = new(T)`, `dst := &s`, an inline element type; a type
// alias, `dst := helper()`, a closure parameter. That loop was measuring how
// many spellings a reviewer could think of, not whether the guard was sound
// (CONVE-35). go/packages answers the question the approximation was
// approximating: what is the TYPE of this expression. No spelling escapes it,
// including the ones nobody has thought of yet.
//
// It also removes the need for a registry of parallel schema structs. A
// destination counts when its type is SCHEMA-SHAPED — a struct with a
// `json:"fields"` member that is a slice of structs — so `bootstrapSchema`,
// which is a hand-kept mirror of CollectionSchema under a different name and
// was invisible to a pass keyed on the canonical type, is caught by what it IS.

// rawDecodeAllowed maps "path::destination" to why that site decodes the schema
// RAW. Every entry is a site where the collection DEFINITION is the subject, so
// stripping the declaration would be wrong rather than merely unnecessary.
var rawDecodeAllowed = map[string]string{
	"internal/models/collection_schema_item_fields.go::schema": "UnmarshalItemFieldSchema IS the predicate — it decodes raw and then strips, which is the whole point of it",

	// INTERFACE destinations. The type checker cannot see what these decode
	// into, so they are reported rather than assumed harmless; each is a
	// GENERIC decode helper whose callers choose the concrete type, and none
	// of them can be converted without breaking every other thing they decode.
	// A new one shows up here and has to be reasoned about, which is the point.
	"internal/cli/client.go::result":             "generic HTTP response decoder: the caller supplies the destination, so this call has no schema to strip — a caller that wants an item-field schema must decode one itself",
	"internal/oauth/storage.go::session":         "fosite session hydration: decodes an OAuth session blob, never a collection schema",
	"internal/server/server.go::v":               "the shared request-body decoder (BUG-2803): decodes a handler's own input struct, and a handler taking a schema goes through the collection-definition doors above",
	"scripts/decision-eval/attention/main.go::v": "offline eval tool (TASK-3137): readJSON decodes local `pad item show|comments|history` and activity dumps into items, comments, versions and activity rows; nothing it reads is a collection definition",
	"internal/models/fields_json.go::v":          "DecodeJSONKeepingNumbers (BUG-3202): the number-preserving decoder for item FIELD blobs and field-value request members on the write doors. Its callers decode item fields, never a collection definition; a schema decoded through it would bypass this guard the same way one decoded through client.go::result would",
	"internal/server/handlers_bootstrap.go::s":   "trimRedundantSchemaLabels decodes into the parallel bootstrapSchema struct and strips reserved keys in the same loop — the parallel-struct twin of UnmarshalItemFieldSchema, and it cannot call it because the whole point of that struct is a different FieldDef shape",

	"internal/server/handlers_collections.go::schema":     "collection create/update INPUT — the declaration is what is being validated, by validateNoReservedFieldKeys",
	"internal/server/handlers_collections.go::prevSchema": "the grandfather test's own baseline: a stripped prevSchema would reclassify every EXISTING declaration as newly introduced and refuse every update to such a collection",
	"cmd/pad/cmd_collection.go::schema":                   "CLI --schema parse and the collection list's own status-option rendering — the collection's definition, not any item's fields",
	"internal/mcp/dispatch_http_routes.go::schema":        "MCP collection schema INPUT, the remote twin of handlers_collections.go",

	// The move / copy family. These decode raw and strip at the point of use
	// via items.SchemaForMigratedFields (BUG-2674), which is the same
	// predicate; they are listed rather than converted because the preflight
	// distinguishes the raw source schema from the stripped one in its
	// reporting, and collapsing the two is a separate change with its own
	// regression surface. See the BUG-2685 trail.
	"internal/server/handlers_items.go::sourceSchema":                "move: stripped at use by items.SchemaForMigratedFields",
	"internal/server/handlers_items.go::targetSchema":                "move: stripped at use by items.SchemaForMigratedFields",
	"internal/server/handlers_items_bulk.go::sourceSchema":           "bulk move: stripped at use by items.SchemaForMigratedFields",
	"internal/server/handlers_items_bulk.go::targetSchema":           "bulk move: stripped at use by items.SchemaForMigratedFields",
	"internal/server/handlers_items_copy.go::targetSchema":           "copy: stripped at use by items.SchemaForMigratedFields",
	"internal/server/handlers_items_copy_preflight.go::sourceSchema": "copy preflight: the RAW source schema is the subject of its dropped-key reporting",
	"internal/server/handlers_items_copy_preflight.go::targetSchema": "copy preflight: stripped at use by items.SchemaForMigratedFields",
	"internal/store/items_cross_workspace_copy.go::sourceSchema":     "store copy: the RAW source schema is the subject of its dropped-key reporting",
	"internal/store/items_cross_workspace_copy.go::targetSchema":     "store copy: stripped at use by items.SchemaForMigratedFields",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

type decodeSite struct {
	path string
	line int
	dest string
}

func (s decodeSite) key() string { return s.path + "::" + s.dest }

// schemaShaped reports whether t is, under its pointers and named types, a
// struct carrying a `json:"fields"` member that is a slice of structs — the
// shape of a collection schema.
//
// BY SHAPE, NOT BY NAME, for the reason the bootstrap hole taught: a mirror
// struct under a different name is the same contract, and a guard keyed on the
// canonical type cannot see one. An ITEM's `fields` is its VALUES — a string, a
// json.RawMessage, a map, a named struct — and there are seven of those in the
// tree, all excluded by the slice-of-structs test.
func schemaShaped(t types.Type) bool {
	for i := 0; i < 8; i++ {
		// types.Unalias FIRST: since Go 1.24 a type alias materialises as
		// *types.Alias rather than resolving to the aliased Named, so a loop
		// that only unwraps Pointer and Named stops dead on
		// `type S = models.CollectionSchema`. Found by the fixture, which is
		// exactly the job it was kept for.
		t = types.Unalias(t)
		switch u := t.(type) {
		case *types.Pointer:
			t = u.Elem()
			continue
		case *types.Named:
			t = u.Underlying()
			continue
		}
		break
	}
	st, ok := t.(*types.Struct)
	if !ok {
		return false
	}
	for i := 0; i < st.NumFields(); i++ {
		// An EMBEDDED schema carries the same JSON members at the same level,
		// so a struct embedding models.CollectionSchema decodes a schema
		// declaration exactly as the schema itself does.
		if st.Field(i).Embedded() && schemaShaped(st.Field(i).Type()) {
			return true
		}
		if !jsonNameIsFields(st.Field(i).Name(), st.Tag(i)) {
			continue
		}
		ft := st.Field(i).Type()
		for j := 0; j < 8; j++ {
			ft = types.Unalias(ft)
			if n, ok := ft.(*types.Named); ok {
				ft = n.Underlying()
				continue
			}
			break
		}
		sl, ok := ft.(*types.Slice)
		if !ok {
			continue
		}
		el := sl.Elem()
		for j := 0; j < 8; j++ {
			el = types.Unalias(el)
			// `[]*models.FieldDef` decodes the same JSON as `[]models.FieldDef`.
			if pt, ok := el.(*types.Pointer); ok {
				el = pt.Elem()
				continue
			}
			if n, ok := el.(*types.Named); ok {
				el = n.Underlying()
				continue
			}
			break
		}
		if _, ok := el.(*types.Struct); ok {
			return true
		}
	}
	return false
}

// jsonNameIsFields reports whether a struct field is serialised as `fields`.
//
// encoding/json matches an UNTAGGED field by NAME, case-insensitively, so
// `struct{ Fields []models.FieldDef }` decodes the same JSON as the tagged
// version and is the same schema (codex round 12). Requiring the tag was a
// property of how the shape test was written rather than of what JSON does.
func jsonNameIsFields(name, tag string) bool {
	if v, ok := reflect.StructTag(tag).Lookup("json"); ok {
		jsonName := v
		if idx := strings.Index(v, ","); idx >= 0 {
			jsonName = v[:idx]
		}
		if jsonName == "-" {
			return false
		}
		if jsonName != "" {
			return jsonName == "fields"
		}
		// `json:",omitempty"` keeps the field NAME; fall through.
	}
	return strings.EqualFold(name, "fields")
}

// modulePath is this repository's Go module path. A package outside it is a
// dependency, whatever directory its files happen to sit in.
const modulePath = "github.com/PerpetualSoftware/pad"

// insideModule reports whether a package path belongs to this module.
func insideModule(pkgPath string) bool {
	return pkgPath == modulePath || strings.HasPrefix(pkgPath, modulePath+"/")
}

// isVendored reports whether a repo-relative path lies under any vendor/
// directory. Checked per SEGMENT rather than as a prefix, so a nested vendor
// tree is excluded too and a legitimate path like `internal/vendorimport.go` is
// not.
func isVendored(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "vendor" {
			return true
		}
	}
	return false
}

// destName is the allow-list key's second half: the name a reader would use for
// the destination. Derived from the expression rather than from its type, so the
// key stays the one a person would write down.
func destName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.UnaryExpr:
		return destName(x.X)
	case *ast.StarExpr:
		return destName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.IndexExpr:
		return destName(x.X)
	case *ast.CallExpr:
		return destName(x.Fun)
	case *ast.CompositeLit:
		if x.Type != nil {
			return destName(x.Type)
		}
	}
	return "<expr>"
}

// collectSchemaDecodes returns every RAW decode whose destination is
// schema-shaped: a call to encoding/json.Unmarshal, or to a Decode method on an
// encoding/json Decoder, whose last argument has such a type.
//
// Calls to models.UnmarshalItemFieldSchema are not decodes for this purpose —
// that IS the predicate — and are excluded by callee identity rather than by
// name matching.
// loadCache memoises the type-checked walk. Four tests ask the same question of
// the same tree and a full go/packages load is ~5s each; without this the guard
// alone is most of the package's runtime.
var (
	loadMu    sync.Mutex
	loadCache = map[string][]decodeSite{}
)

func collectSchemaDecodes(t *testing.T, dir string, patterns ...string) []decodeSite {
	t.Helper()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	cacheKey := dir + "|" + strings.Join(patterns, ",")
	loadMu.Lock()
	if cached, ok := loadCache[cacheKey]; ok {
		loadMu.Unlock()
		return cached
	}
	loadMu.Unlock()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
		Dir:   dir,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, p.PkgPath+": "+e.Error())
		}
	})
	if len(loadErrs) > 0 {
		// A guard that certifies a tree it could not type-check certifies
		// nothing — this is the "went blind while passing" failure with the
		// type checker's own voice.
		t.Fatalf("the tree did not type-check, so this guard proves nothing:\n  %s",
			strings.Join(loadErrs[:min(len(loadErrs), 5)], "\n  "))
	}

	root := dir
	var out []decodeSite
	seen := map[string]bool{}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.TypesInfo == nil {
			return
		}
		// SCOPE BY MODULE PATH. packages.Visit walks the whole DEPENDENCY
		// GRAPH, not just the packages matched by the pattern — which is
		// invisible in an ordinary checkout, where a dependency's files live in
		// the module cache OUTSIDE the root and the relative-path check below
		// discards them. In a VENDORED tree they live under <root>/vendor, so
		// that check passes and every dependency's own decode is enumerated:
		// the Nix job builds vendored and found 57 sites where this tree has
		// 20. The module path is the exact predicate.
		//
		// MEASURED, so the comment does not overclaim: with a vendored tree,
		// this check alone and the vendor-segment check below alone EACH
		// suffice — removing either one leaves the guard green, so neither is
		// individually load-bearing today and no mutant dies alone. Both are
		// kept because they answer different questions: the module path is
		// about PROVENANCE (a dependency is not ours to classify, wherever its
		// files sit), the vendor segment is about LOCATION and is the one case
		// module scoping cannot cover — a vendored copy of THIS module's own
		// path. That case is not constructed here; it is why the cheaper check
		// stays rather than a claim that it fires today.
		if !insideModule(p.PkgPath) {
			return
		}
		for _, f := range p.Syntax {
			pos := p.Fset.Position(f.Pos())
			if strings.HasSuffix(pos.Filename, "_test.go") {
				continue
			}
			rel, err := filepath.Rel(root, pos.Filename)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue // outside the tree under test (a dependency)
			}
			// VENDORED code is not ours to classify, and it is not hypothetical:
			// this guard passed locally and FAILED the Nix job, which builds
			// from a `vendor/` tree that `./...` then walks. 57 sites instead of
			// 20, every extra one a dependency's own decode caught by the
			// interface-destination arm. A guard whose verdict depends on
			// whether the checkout happens to be vendored is not a guard.
			if isVendored(rel) {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				if !isDecodeCall(call, p.TypesInfo) {
					return true
				}
				// The DESTINATION is the last argument for both
				// json.Unmarshal(data, dst) and dec.Decode(dst). The source
				// argument routinely mentions a schema too.
				arg := call.Args[len(call.Args)-1]
				tv, ok := p.TypesInfo.Types[arg]
				if !ok {
					return true
				}
				// An INTERFACE destination hides its concrete type from the
				// type checker — `var dst any = &schema` decodes a schema and
				// resolves as `any`. Tracking it properly is dataflow analysis;
				// instead every such decode is REPORTED for classification.
				// There are three in the tree, so the cost is three allow-list
				// entries and the hole is closed rather than documented.
				//
				// A POINTER to an interface (`json.Unmarshal(b, &v)` where v is
				// `any`) is DELIBERATELY NOT included, and the number is the
				// reason: including it takes the population from 20 to 38, and
				// the 18 additions are all the ordinary decode-arbitrary-JSON
				// idiom. Eighteen entries whose reason is the same sentence is
				// an allow-list nobody reads, which is a worse guard than the
				// hole. The residual needs someone to place a schema POINTER
				// inside an `any` and then pass its address — a shape no site
				// in this tree uses and no converted site could become without
				// the conversion being visible in the same diff.
				if !schemaShaped(tv.Type) && !types.IsInterface(tv.Type) {
					return true
				}
				cpos := p.Fset.Position(call.Pos())
				s := decodeSite{
					path: filepath.ToSlash(rel),
					line: cpos.Line,
					dest: destName(arg),
				}
				k := fmt.Sprintf("%s:%d:%s", s.path, s.line, s.dest)
				if seen[k] {
					return true
				}
				seen[k] = true
				out = append(out, s)
				return true
			})
		}
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		return out[i].dest < out[j].dest
	})

	loadMu.Lock()
	loadCache[cacheKey] = out
	loadMu.Unlock()
	return out
}

// isDecodeCall reports whether call is a JSON decode — a call to
// encoding/json.Unmarshal or to a Decode method on its Decoder, DIRECTLY or
// through a function VALUE holding one.
//
// The function-value arm exists because `decode := json.Unmarshal` followed by
// `decode(b, &schema)` resolves to no *types.Func at all. It is matched by
// SIGNATURE, which would be far too wide on its own — the tree has 1710 two-arg
// calls through function values (measured) — and is only ever reached for a
// call whose LAST ARGUMENT is already schema-shaped, which is what makes it
// narrow.
func isDecodeCall(call *ast.CallExpr, info *types.Info) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if fn, ok := info.Uses[sel.Sel].(*types.Func); ok {
			return isJSONDecodeFunc(fn)
		}
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		if fn, ok := info.Uses[id].(*types.Func); ok {
			return isJSONDecodeFunc(fn)
		}
	}
	tv, ok := info.Types[call.Fun]
	if !ok {
		return false
	}
	sig, ok := types.Unalias(tv.Type).(*types.Signature)
	if !ok || sig.Recv() != nil {
		return false
	}
	// json.Unmarshal: func([]byte, any) error. Decoder.Decode: func(any) error.
	if sig.Results().Len() != 1 || sig.Results().At(0).Type().String() != "error" {
		return false
	}
	switch sig.Params().Len() {
	case 1:
		return types.IsInterface(sig.Params().At(0).Type())
	case 2:
		p0, ok := sig.Params().At(0).Type().(*types.Slice)
		return ok && p0.Elem().String() == "byte" && types.IsInterface(sig.Params().At(1).Type())
	}
	return false
}

// isJSONDecodeFunc reports whether fn is encoding/json's Unmarshal, or the
// Decode method on its Decoder.
//
// By CALLEE IDENTITY rather than by the receiver's spelling: an aliased import
// and a `json.NewDecoder(r).Decode(...)` chain both resolve to the same object,
// which is what makes the alias hole disappear rather than get another arm.
func isJSONDecodeFunc(fn *types.Func) bool {
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil {
		return false
	}
	if recv := sig.Recv(); recv != nil {
		if fn.Name() != "Decode" {
			return false
		}
		t := recv.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		named, ok := t.(*types.Named)
		if !ok || named.Obj().Pkg() == nil {
			return false
		}
		return named.Obj().Pkg().Path() == "encoding/json" && named.Obj().Name() == "Decoder"
	}
	return fn.Pkg() != nil && fn.Pkg().Path() == "encoding/json" && fn.Name() == "Unmarshal"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestReservedFieldDecodeSitesAreClassified(t *testing.T) {
	root := repoRoot(t)
	sites := collectSchemaDecodes(t, root)
	if len(sites) == 0 {
		t.Fatal("the guard found no decode sites at all — it has gone blind, " +
			"which would let every future site through silently")
	}
	for _, s := range sites {
		if _, ok := rawDecodeAllowed[s.key()]; !ok {
			t.Errorf("%s:%d decodes a collection schema RAW into %q.\n"+
				"  If it reasons about an ITEM's fields, call models.UnmarshalItemFieldSchema instead — a\n"+
				"  grandfathered reserved-key FieldDef must not reach it (BUG-2685).\n"+
				"  If the collection DEFINITION is the subject, add %q to rawDecodeAllowed with the reason.",
				s.path, s.line, s.dest, s.key())
		}
	}
}

// TestGuardAllowListHasNoDeadEntries keeps the allow-list honest: an entry whose
// site was converted or deleted is a standing permission for a site that no
// longer asks for it, and the next raw decode with that name inherits it.
func TestGuardAllowListHasNoDeadEntries(t *testing.T) {
	live := map[string]bool{}
	for _, s := range collectSchemaDecodes(t, repoRoot(t)) {
		live[s.key()] = true
	}
	for k := range rawDecodeAllowed {
		if !live[k] {
			t.Errorf("rawDecodeAllowed has no live site for %q — remove the entry", k)
		}
	}
}

// rawDecodeSiteCount is how many raw decode sites the guard must find.
//
// A RECEIPT, not a magic number: 20 on the BUG-2685 tree — 16 decoding a
// models.CollectionSchema, plus the one decoding the parallel `bootstrapSchema`
// (which a pass keyed on the canonical type could not see, and which is why
// this guard matches on SHAPE), plus the three whose destination is an
// INTERFACE and so cannot be resolved at all. The last three are reported
// rather than assumed harmless; the allow-list says what each actually decodes.
//
// It is a count and not just a key set because two sites can share a
// `path::dest` key — cmd_collection.go and handlers_collections.go each have
// two — so key-set equality alone would pass if the guard went blind to one of
// a pair. A DROP with no conversion in the same change means the guard stopped
// seeing sites, not that the sites stopped existing.
//
// 22 since BUG-3202 added models/fields_json.go's generic number-preserving
// decoder, a fourth INTERFACE destination.
const rawDecodeSiteCount = 22

func TestGuardSeesEveryKnownDecodeSite(t *testing.T) {
	sites := collectSchemaDecodes(t, repoRoot(t))
	if len(sites) != rawDecodeSiteCount {
		var got []string
		for _, s := range sites {
			got = append(got, fmt.Sprintf("%s:%d(%s)", s.path, s.line, s.dest))
		}
		t.Errorf("guard found %d raw decode sites, expected %d.\n  found: %s",
			len(sites), rawDecodeSiteCount, strings.Join(got, "\n         "))
	}
}

// TestGuardResolvesEveryDestinationShape runs the resolver over the fixture in
// testdata/decodeshapes and requires it to see every destination spelling
// written down there.
//
// THE FIXTURE OUTLIVED THE GUARD IT WAS WRITTEN FOR, which is why it is still
// here. It was built for the go/ast approximation, where five review rounds
// each named another spelling that approximation could not see; the type
// checker answers all of them by construction, so this is no longer a list of
// holes to plug but a REGRESSION TEST on the resolver — it fails if a future
// change to schemaShaped, isJSONDecodeFunc, or the destination-argument rule
// narrows what the guard can resolve.
//
// It also pins the NEGATIVE half: the fixture's last function decodes through
// the correct helper with a SOURCE argument that mentions a schema, and must
// NOT be reported.
func TestGuardResolvesEveryDestinationShape(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "models", "testdata", "decodeshapes")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}

	sites := collectSchemaDecodes(t, dir, ".")
	got := map[string]bool{}
	for _, s := range sites {
		got[s.dest] = true
	}

	want := []string{
		"localDest",        // &local
		"paramDest",        // bare pointer param
		"fieldDest",        // &struct.field
		"new",              // new(T) — the call, which is the destination itself
		"CollectionSchema", // &T{} names the TYPE, not a variable
		"sliceDest",        // &container[i]
		"makeDest",         // the result of a helper returning a schema
		"decoderDest",      // json.Decoder.Decode, aliased import
		"shortDest",        // x := T{}
		"inferredDest",     // var x = T{}
		"newDest",          // var x = new(T)
		"aliasDest",        // x := &alreadyASchema
		"aliasedPkgDest",   // models imported under an alias
		"typeAliasDest",    // a type alias for the schema
		"helperResultDest", // x := helperReturningASchemaPointer()
		"closureDest",      // a closure parameter
		"inlineDest",       // a schema-shaped type that is not CollectionSchema
		"namedSliceDest",   // the fields member is a NAMED slice type
		"funcValueDest",    // called through a function value holding json.Unmarshal
		"ifaceDest",        // an interface destination, reported not resolved
		"embeddedDest",     // a struct embedding a schema
		"ptrElemDest",      // fields whose element type is a pointer
		"untaggedDest",     // an UNTAGGED fields member, matched by name
	}
	// `excludedDest` is deliberately absent: its fields member is `json:"-"`,
	// so it is not a schema on the wire and must NOT be reported.
	for _, w := range want {
		if !got[w] {
			t.Errorf("the resolver cannot see the destination shape named by %q in testdata/decodeshapes", w)
		}
	}

	// Twenty-three decodes are destinations; the fixture's last call is the
	// negative case and goes through the helper, so it is not a raw decode at
	// all. BELOW twenty-three means a shape went missing; ABOVE means the source
	// argument is matching again.
	if len(sites) != 23 {
		var lines []string
		for _, s := range sites {
			lines = append(lines, fmt.Sprintf("%s:%d(%s)", s.path, s.line, s.dest))
		}
		t.Errorf("fixture yielded %d raw decode sites, expected 23:\n  %s",
			len(sites), strings.Join(lines, "\n  "))
	}
}

// TestGuardScopesToThisModule pins the two path predicates and, more
// importantly, the INVARIANT on the guard's output: nothing it enumerates may
// come from outside this module or from a vendor tree.
//
// The invariant is the part that matters. The predicates are easy to get right
// and easy to stop calling; the output assertion fails either way, and it is
// the assertion that was false before this fix — with a vendored tree the guard
// enumerated 57 sites, 37 of them dependencies' own decodes.
func TestGuardScopesToThisModule(t *testing.T) {
	for _, tc := range []struct {
		pkgPath string
		want    bool
	}{
		{"github.com/PerpetualSoftware/pad", true},
		{"github.com/PerpetualSoftware/pad/internal/models", true},
		{"github.com/PerpetualSoftware/pad/internal/models/testdata/decodeshapes", true},
		{"github.com/gogo/protobuf/jsonpb", false},
		{"go.opentelemetry.io/otel/baggage", false},
		{"encoding/json", false},
		// A path that merely STARTS with the module path but is a different
		// module — the reason the check is not a bare HasPrefix.
		{"github.com/PerpetualSoftware/pad-web/internal/models", false},
	} {
		if got := insideModule(tc.pkgPath); got != tc.want {
			t.Errorf("insideModule(%q) = %v, want %v", tc.pkgPath, got, tc.want)
		}
	}

	for _, tc := range []struct {
		rel  string
		want bool
	}{
		{"vendor/github.com/gogo/protobuf/jsonpb/jsonpb.go", true},
		{"internal/models/vendor/x/y.go", true},
		{"internal/models/item.go", false},
		// NOT vendored: the segment test is why, and a prefix test would get
		// this wrong.
		{"internal/vendorimport.go", false},
		{"internal/vendors/list.go", false},
	} {
		if got := isVendored(tc.rel); got != tc.want {
			t.Errorf("isVendored(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}

	// THE INVARIANT, asserted on real output rather than on the predicates.
	for _, s := range collectSchemaDecodes(t, repoRoot(t)) {
		if isVendored(s.path) {
			t.Errorf("the guard enumerated a VENDORED site: %s:%d — a dependency's decode is not ours to classify", s.path, s.line)
		}
		if strings.HasPrefix(s.path, "..") || filepath.IsAbs(s.path) {
			t.Errorf("the guard enumerated a site outside the tree: %s:%d", s.path, s.line)
		}
	}
}
