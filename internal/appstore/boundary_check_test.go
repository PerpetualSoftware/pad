package appstore

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// The boundary checker behind boundary_test.go (SPEC-6 §4, TASK-3388). It is
// a GUARD, not a proof: it closes the gaps named below and nothing more.
//
// It loads a root package with every dependency type-checked from source in
// ONE universe (NeedDeps), so a callee's *types.Func is the same object in
// the caller's Info as in its own package's Defs. It then walks every
// function the root package can REACH inside this module: any use of a
// module function or method, called or passed as a value, is followed into
// that function's body. Interface calls cannot be followed statically, which
// is why rule 4 refuses the ones an internal/store type could answer.

const (
	modulePrefix   = "github.com/PerpetualSoftware/pad/"
	storePkgPath   = "github.com/PerpetualSoftware/pad/internal/store"
	attachmentsPkg = "github.com/PerpetualSoftware/pad/internal/attachments"
)

// allowedStoreMethods are the *store.Store methods reachable from outside the
// fence door. A store method joins only after review (U2 onward); any other
// one is a violation, so a human mutation method can never be reached.
var allowedStoreMethods = map[string]bool{
	"BeginFenced": true,
}

// allowedStoreMethodsInsideDoor are the *store.Store helpers the door itself
// (BeginFenced, FencedTx methods, and the reviewed helpers below) may call. A
// helper on this list is part of the door: its body is walked with the door's
// rules. Each entry records what it WRITES, confirmed by reading it; "reads"
// means it executes no INSERT, UPDATE or DELETE. None opens a transaction of
// its own: every one runs on the transaction it is handed.
var allowedStoreMethodsInsideDoor = map[string]string{
	"q":                       "rebinds placeholders; no SQL",
	"acquireWorkspaceSeqLock": "pg_advisory_xact_lock on the workspace; writes nothing",
	// U2a (TASK-3390).
	"getItemTx":                "reads the item row",
	"getItemScanQ":             "reads the item row",
	"doneFieldKeyQ":            "reads the collection's schema and settings",
	"getCollectionSlugTx":      "reads collections.slug",
	"getCollectionQ":           "reads the collection row",
	"scanCollectionRow":        "scans a collection row; no SQL of its own",
	"enforceWorkspaceLimitTx":  "reads plan, settings and the item count under the caller's lock",
	"checkLimitOn":             "reads plan, settings and counts",
	"featureCountOn":           "reads a count",
	"resolveLimitQ":            "reads workspace owner, user plan and platform settings",
	"GetUserQ":                 "reads the user row",
	"GetPlatformSettingQ":      "reads platform_settings",
	"decryptUserTOTP":          "decrypts a field in memory; no SQL",
	"decrypt":                  "decrypts in memory; no SQL",
	"HasEncryptionKey":         "reads in-memory config; no SQL",
	"replaceWikiLinks":         "DELETE and INSERT on item_wiki_links WHERE source_item_id = the item it is given, only; target resolution reads",
	"emitItemEventTx":          "INSERT INTO event_outbox (through writeOutboxTx) for the item it is given",
	"emitItemUpdateEventsTx":   "INSERT INTO event_outbox for the item it is given (status_changed and/or updated)",
	"buildItemAppProjectionTx": "reads the item's creator and collection schema",
	"userDisplayTx":            "reads a user's display name",
	"outboxRowCap":             "reads in-memory config; no SQL",
	"outboxClaimableRowCap":    "reads in-memory config; no SQL",
	"enqueueDecisionJobsTx":    "INSERT ... ON CONFLICT DO UPDATE on decision_jobs for the item it is given; no-op without a provider",
	"decisionSetResolver":      "loads an in-memory pointer; no SQL",
	"createActivityQ":          "INSERT INTO activities on the executor it is given (the fence's tx)",
	"recentDebounceCandidateQ": "reads activities",
}

// reviewedStoreFuncs are package-level internal/store functions that are part
// of the door, with what each writes. Same rules as the list above.
var reviewedStoreFuncs = map[string]string{
	"writeOutboxTx":           "INSERT INTO event_outbox ... RETURNING on the tx it is handed",
	"resolveRefTx":            "reads items",
	"resolveTitleTx":          "reads items",
	"resolveWorkspaceSlugTx":  "reads workspaces",
	"itemUpdatedSliceChanged": "compares two item snapshots in memory; no SQL",
	"collapseChanges":         "rewrites a change string in memory; no SQL",
}

// allowedFuncValues are named function values the walk may meet: function
// FullName, then variable name, then why it is safe. Per the lead's ruling the
// func-value rule is never loosened globally; a new case gets its own entry.
var allowedFuncValues = map[string]map[string]string{
	"(*github.com/PerpetualSoftware/pad/internal/store.Store).enqueueDecisionJobsTx": {
		"resolve": "the store's DecisionSetResolver; production installs decision.Registry.SetsFor, an in-memory lookup that runs no SQL",
	},
	"github.com/PerpetualSoftware/pad/internal/store.collapseChanges": {
		"isLossySummary": "a closure defined in the same body and never reassigned; the walk inspects its body",
	},
	"github.com/PerpetualSoftware/pad/internal/store.itemUpdatedSliceChanged": {
		"normalize": "a closure defined in the same body and never reassigned; the walk inspects its body",
	},
}

type violation struct {
	pos  token.Position
	rule string
	what string
}

func (v violation) String() string {
	return fmt.Sprintf("%s:%d: [%s] %s", v.pos.Filename, v.pos.Line, v.rule, v.what)
}

type funcSite struct {
	decl *ast.FuncDecl
	pkg  *packages.Package
}

// checkBoundary loads every root (package paths or ./patterns relative to
// dir) in ONE load, so the module is type-checked once, and returns each
// root's violations, sorted, keyed by the root's package path.
func checkBoundary(t *testing.T, dir string, roots ...string) map[string][]violation {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
		Dir: dir,
	}
	pkgs, err := packages.Load(cfg, roots...)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("load: %d package errors", n)
	}
	if len(pkgs) != len(roots) {
		t.Fatalf("load: %d packages for %d roots", len(pkgs), len(roots))
	}

	ix := &index{funcs: map[*types.Func]funcSite{}, storeClosure: map[string]bool{}}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.PkgPath == storePkgPath {
			ix.storePkg = p.Types
			// The store's own import closure initializes in every pad
			// binary, apps or not; it was reviewed with the human store.
			packages.Visit([]*packages.Package{p}, nil, func(q *packages.Package) {
				ix.storeClosure[q.PkgPath] = true
			})
		}
		if !inModule(p.PkgPath) {
			return
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok {
					if obj, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
						ix.funcs[obj] = funcSite{decl: fd, pkg: p}
					}
				}
			}
		}
	})
	if ix.storePkg == nil {
		t.Fatalf("load: internal/store is not in the closure; nothing to guard")
	}
	ix.storeImpls = storeTypes(ix.storePkg)
	if st, ok := ix.storePkg.Scope().Lookup("Store").Type().Underlying().(*types.Struct); ok {
		for i := 0; i < st.NumFields(); i++ {
			if st.Field(i).Name() == "db" {
				ix.storeDB = st.Field(i)
			}
		}
	}
	if ix.storeDB == nil {
		t.Fatalf("load: Store.db not found")
	}
	result := map[string][]violation{}
	for _, rootPkg := range pkgs {
		result[rootPkg.PkgPath] = ix.walkRoot(rootPkg)
	}
	return result
}

type index struct {
	funcs        map[*types.Func]funcSite
	storeClosure map[string]bool
	storePkg     *types.Package
	storeImpls   []types.Type
	storeDB      *types.Var
}

// unit is one body the walk inspects: a function or method declaration, or a
// package-level variable initializer (fn and decl nil).
type unit struct {
	name string
	fn   *types.Func
	decl *ast.FuncDecl
	node ast.Node
	pkg  *packages.Package
}

func inModule(path string) bool {
	return path == "github.com/PerpetualSoftware/pad" || strings.HasPrefix(path, modulePrefix)
}

func (ix *index) walkRoot(rootPkg *packages.Package) []violation {
	var out []violation
	report := func(p *packages.Package, pos token.Pos, rule, format string, args ...any) {
		out = append(out, violation{pos: p.Fset.Position(pos), rule: rule, what: fmt.Sprintf(format, args...)})
	}

	// Rule 5: no interface-typed declaration in the root package that an
	// internal/store type implements.
	for id, obj := range rootPkg.TypesInfo.Defs {
		v, ok := obj.(*types.Var)
		if !ok || id == nil {
			continue
		}
		if iface, ok := v.Type().Underlying().(*types.Interface); ok && !isErrorIface(v.Type()) && implementedByStore(iface, ix.storeImpls) {
			report(rootPkg, id.Pos(), "interface-decl", "%s has interface type %s, which an internal/store type implements", v.Name(), v.Type())
		}
	}

	seen := map[*types.Func]bool{}
	var queue []unit
	walkedPkgs := map[*packages.Package]bool{}
	enqueue := func(fn *types.Func) {
		fn = fn.Origin()
		site, ok := ix.funcs[fn]
		if !ok || seen[fn] {
			return
		}
		seen[fn] = true
		queue = append(queue, unit{name: fn.FullName(), fn: fn, decl: site.decl, node: site.decl, pkg: site.pkg})
	}
	for obj, site := range ix.funcs {
		if site.pkg == rootPkg {
			enqueue(obj)
		}
	}
	// Package initialization: the root package, and every module package it
	// brings in that the human store does not already import, contribute
	// their init functions and package-level variable initializers. (A
	// method value in a package var, `var remove = (*store.Store).DeleteItem`,
	// is otherwise never walked.) The store's own closure initializes in
	// every pad binary whether or not an app exists, and is excluded.
	packages.Visit([]*packages.Package{rootPkg}, nil, func(p *packages.Package) {
		if !inModule(p.PkgPath) || ix.storeClosure[p.PkgPath] {
			return
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil && d.Name.Name == "init" {
						queue = append(queue, unit{name: p.PkgPath + ".init", decl: d, node: d, pkg: p})
					}
				case *ast.GenDecl:
					if d.Tok != token.VAR {
						continue
					}
					for _, spec := range d.Specs {
						vs := spec.(*ast.ValueSpec)
						for _, v := range vs.Values {
							queue = append(queue, unit{name: p.PkgPath + " package var", node: v, pkg: p})
						}
					}
				}
			}
		}
	})

	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		p := u.pkg
		walkedPkgs[p] = true
		door := u.fn != nil && isDoor(u.fn)
		inBeginFenced := u.fn != nil && u.fn.Name() == "BeginFenced" && u.fn.Pkg() != nil && u.fn.Pkg().Path() == storePkgPath
		ast.Inspect(u.node, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				// Rule 8: a call through a function VALUE has no static
				// target the walk can follow. Refused outright, with no
				// attempt to verify where the value came from: every such
				// verification codex probed (reassignment, aliases, method
				// expressions, interface dispatch) had a bypass, and this
				// is a guard, not a proof. App code calls named functions.
				if !isStaticOrBuiltin(p, n.Fun) && !allowedFuncValue(u, p, n.Fun) {
					report(p, n.Pos(), "dynamic-call", "%s calls a function value the walk cannot resolve", u.name)
				}
			case *ast.SelectorExpr:
				sel := p.TypesInfo.Selections[n]
				if sel == nil {
					return true
				}
				// Rule 3: Store's raw *sql.DB, however it is reached
				// (embedding included: the selected field is compared).
				if sel.Kind() == types.FieldVal && sel.Obj() == ix.storeDB && !inBeginFenced {
					report(p, n.Pos(), "raw-db", "%s reads Store.db", u.name)
				}
				// Rule 9: a FIELD of function type is a function value.
				if sel.Kind() == types.FieldVal && isFuncType(sel.Obj().Type()) {
					report(p, n.Pos(), "func-value", "%s uses function-typed field %s", u.name, sel.Obj().Name())
				}
				inst, ok := sel.Obj().(*types.Func)
				if !ok {
					return true
				}
				// Rule 4: an interface call an internal/store type could
				// answer, judged on the INSTANTIATED interface: Mutator[string]
				// may be implemented where Mutator[T] is not. The door is
				// exempt: it is reviewed code inside the store and calls the
				// store's own dialect interface.
				if irecv := recvOf(inst); irecv != nil {
					if iface, ok := irecv.Underlying().(*types.Interface); ok {
						// A generic body is walked once, with its type
						// parameters unbound, so no interface it dispatches
						// through can be checked against what will
						// implement it: Mutator[T], an anonymous constraint,
						// a type parameter hidden in a func, alias, slice or
						// map argument. Codex rounds 3 and 4 found each in
						// turn; the class is closed by refusing interface
						// dispatch inside ANY generic body (error excepted).
						if !door && isGenericUnit(u) && !isErrorIface(irecv) {
							report(p, n.Pos(), "interface-call", "%s calls %s on interface %s inside a generic body", u.name, inst.Name(), irecv)
						} else if !door && !isErrorIface(irecv) && implementedByStore(iface, ix.storeImpls) {
							report(p, n.Pos(), "interface-call", "%s calls %s on interface %s, which an internal/store type implements", u.name, inst.Name(), irecv)
						}
						return true
					}
				}
				m := inst.Origin()
				// The DECLARING receiver, not the selection's: a method
				// promoted through an embedded *sql.DB or *store.Store is
				// still that type's method.
				recv := recvOf(m)
				// Rule 1: *store.Store methods.
				if isStoreType(recv) {
					if m.Name() == "DB" && !inBeginFenced {
						report(p, n.Pos(), "raw-db", "%s calls (*store.Store).DB", u.name)
					}
					_, insideDoor := allowedStoreMethodsInsideDoor[m.Name()]
					allowed := allowedStoreMethods[m.Name()] || (door && insideDoor)
					if !allowed {
						report(p, n.Pos(), "store-method", "%s calls (*store.Store).%s, which is not on the reviewed allow-list", u.name, m.Name())
					}
				}
				// Rule 2: database/sql outside the door.
				if isSQLHandle(recv) && !door {
					report(p, n.Pos(), "raw-sql", "%s calls %s.%s outside FencedTx", u.name, types.TypeString(recv, nil), m.Name())
				}
				// Rule 7: the ordinary attachments Put.
				if m.Pkg() != nil && m.Pkg().Path() == attachmentsPkg && m.Name() == "Put" {
					report(p, n.Pos(), "attachments-put", "%s reaches attachments %s.Put (dedup timing oracle)", u.name, types.TypeString(recv, nil))
				}
				enqueue(m)
			case *ast.Ident:
				obj := p.TypesInfo.Uses[n]
				if obj == nil {
					return true
				}
				// Rule 6: reflect and unsafe.
				if obj.Pkg() != nil && (obj.Pkg().Path() == "reflect" || obj.Pkg().Path() == "unsafe") {
					report(p, n.Pos(), "reflect-unsafe", "%s uses %s.%s", u.name, obj.Pkg().Path(), obj.Name())
				}
				if f, ok := obj.(*types.Func); ok {
					enqueue(f)
				}
				// Rule 9: a variable or parameter of function type is a
				// function value the walk cannot resolve, whether it is
				// called here or handed to code that calls it (sync.Once.Do).
				if v, ok := obj.(*types.Var); ok && !v.IsField() && isFuncType(v.Type()) && allowedFuncValues[u.name][v.Name()] == "" {
					report(p, n.Pos(), "func-value", "%s uses function-typed variable %s", u.name, v.Name())
				}
			}
			return true
		})
	}

	// Rule 6, file level: an unsafe import (blank included, which is what
	// go:linkname needs) or a go:linkname directive in any package the walk
	// entered. Neither is visible to an identifier walk.
	for p := range walkedPkgs {
		for _, f := range p.Syntax {
			for _, imp := range f.Imports {
				if imp.Path.Value == `"unsafe"` {
					report(p, imp.Pos(), "reflect-unsafe", "%s imports unsafe", p.PkgPath)
				}
			}
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					if strings.HasPrefix(c.Text, "//go:linkname") {
						report(p, c.Pos(), "linkname", "%s has a go:linkname directive", p.PkgPath)
					}
				}
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return dedupe(out)
}

// calleeExpr strips parentheses and generic instantiation from a call's
// function expression.
func calleeExpr(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		default:
			return e
		}
	}
}

// staticCallee is the *types.Func a call statically targets, or nil.
func staticCallee(p *packages.Package, fun ast.Expr) *types.Func {
	switch x := calleeExpr(fun).(type) {
	case *ast.Ident:
		f, _ := p.TypesInfo.Uses[x].(*types.Func)
		return f
	case *ast.SelectorExpr:
		if sel := p.TypesInfo.Selections[x]; sel != nil {
			if sel.Kind() == types.FieldVal {
				return nil
			}
			f, _ := sel.Obj().(*types.Func)
			return f
		}
		f, _ := p.TypesInfo.Uses[x.Sel].(*types.Func)
		return f
	}
	return nil
}

// isStaticOrBuiltin: a call to a named function or method, a builtin, a type
// conversion, or an immediately invoked function literal.
func isStaticOrBuiltin(p *packages.Package, fun ast.Expr) bool {
	e := calleeExpr(fun)
	if _, ok := e.(*ast.FuncLit); ok {
		return true
	}
	if tv, ok := p.TypesInfo.Types[e]; ok && (tv.IsType() || tv.IsBuiltin()) {
		return true
	}
	return staticCallee(p, fun) != nil
}

// isGenericUnit: a function or method with type parameters, its own or its
// receiver's.
func isGenericUnit(u unit) bool {
	if u.fn == nil {
		return false
	}
	sig, ok := u.fn.Type().(*types.Signature)
	return ok && (sig.TypeParams().Len() > 0 || sig.RecvTypeParams().Len() > 0)
}

// allowedFuncValue: the call's callee is a variable named on
// allowedFuncValues for the unit being walked.
func allowedFuncValue(u unit, p *packages.Package, fun ast.Expr) bool {
	id, ok := calleeExpr(fun).(*ast.Ident)
	if !ok {
		return false
	}
	v, ok := p.TypesInfo.Uses[id].(*types.Var)
	return ok && allowedFuncValues[u.name][v.Name()] != ""
}

func isFuncType(t types.Type) bool {
	_, ok := t.Underlying().(*types.Signature)
	return ok
}

func dedupe(vs []violation) []violation {
	var out []violation
	for i, v := range vs {
		if i == 0 || v.String() != vs[i-1].String() {
			out = append(out, v)
		}
	}
	return out
}

// isDoor: BeginFenced and every FencedTx method, the reviewed code that holds
// the transaction handle.
func isDoor(fn *types.Func) bool {
	if fn.Pkg() == nil || fn.Pkg().Path() != storePkgPath {
		return false
	}
	if fn.Name() == "BeginFenced" {
		return true
	}
	// A reviewed helper the door may call is part of the door.
	if _, ok := allowedStoreMethodsInsideDoor[fn.Name()]; ok && isStoreType(recvOf(fn)) {
		return true
	}
	if _, ok := reviewedStoreFuncs[fn.Name()]; ok && recvOf(fn) == nil {
		return true
	}
	r := recvOf(fn)
	if r == nil {
		return false
	}
	if p, ok := r.(*types.Pointer); ok {
		r = p.Elem()
	}
	n, ok := r.(*types.Named)
	return ok && n.Obj().Name() == "FencedTx"
}

func recvOf(fn *types.Func) types.Type {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil
	}
	return sig.Recv().Type()
}

func isStoreType(t types.Type) bool {
	if t == nil {
		return false
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == storePkgPath && n.Obj().Name() == "Store"
}

func isSQLHandle(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil || n.Obj().Pkg().Path() != "database/sql" {
		return false
	}
	switch n.Obj().Name() {
	case "DB", "Tx", "Conn", "Stmt":
		return true
	}
	return false
}

func isErrorIface(t types.Type) bool {
	return types.Identical(t, types.Universe.Lookup("error").Type())
}

// storeTypes returns every named type in internal/store and its pointer.
func storeTypes(p *types.Package) []types.Type {
	var out []types.Type
	for _, name := range p.Scope().Names() {
		tn, ok := p.Scope().Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		if _, isIface := tn.Type().Underlying().(*types.Interface); isIface {
			continue
		}
		out = append(out, tn.Type(), types.NewPointer(tn.Type()))
	}
	return out
}

func implementedByStore(iface *types.Interface, impls []types.Type) bool {
	if iface.NumMethods() == 0 {
		return false
	}
	for _, t := range impls {
		if types.Implements(t, iface) {
			return true
		}
	}
	return false
}
