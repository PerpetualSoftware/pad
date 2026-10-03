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
// (BeginFenced and FencedTx methods) may call. Each was reviewed as opening no
// transaction of its own and writing nothing.
var allowedStoreMethodsInsideDoor = map[string]bool{
	"q":                       true,
	"acquireWorkspaceSeqLock": true,
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

	// Call sites of root-package functions, for the dynamic-call rule.
	rootUses := map[*types.Func][]ast.Expr{}
	callArgs := map[*types.Func][][]ast.Expr{}
	for _, f := range rootPkg.Syntax {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if fn, ok := rootPkg.TypesInfo.Uses[n].(*types.Func); ok && fn.Pkg() == rootPkg.Types {
					rootUses[fn.Origin()] = append(rootUses[fn.Origin()], n)
				}
			case *ast.CallExpr:
				if fn := staticCallee(rootPkg, n.Fun); fn != nil && fn.Pkg() == rootPkg.Types {
					callArgs[fn.Origin()] = append(callArgs[fn.Origin()], n.Args)
				}
			}
			return true
		})
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
				// target. Allowed only for a parameter of an unexported
				// root-package function whose every call site passes a
				// function literal or a named module function, both of
				// which the walk inspects.
				if !isStaticOrBuiltin(p, n.Fun) && !ix.dynamicCallVerified(rootPkg, u, n.Fun, rootUses, callArgs) {
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
				m, ok := sel.Obj().(*types.Func)
				if !ok {
					return true
				}
				m = m.Origin()
				// The DECLARING receiver, not the selection's: a method
				// promoted through an embedded *sql.DB or *store.Store is
				// still that type's method.
				recv := recvOf(m)
				// Rule 4: an interface call an internal/store type could answer.
				// The door is exempt: it is reviewed code inside the store
				// and calls the store's own dialect interface.
				if recv != nil {
					if iface, ok := recv.Underlying().(*types.Interface); ok {
						if !door && !isErrorIface(recv) && implementedByStore(iface, ix.storeImpls) {
							report(p, n.Pos(), "interface-call", "%s calls %s on interface %s, which an internal/store type implements", u.name, m.Name(), recv)
						}
						return true
					}
				}
				// Rule 1: *store.Store methods.
				if isStoreType(recv) {
					if m.Name() == "DB" && !inBeginFenced {
						report(p, n.Pos(), "raw-db", "%s calls (*store.Store).DB", u.name)
					}
					allowed := allowedStoreMethods[m.Name()] || (door && allowedStoreMethodsInsideDoor[m.Name()])
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

// dynamicCallVerified: the callee is a parameter of the unexported root
// function being walked, that function is only ever CALLED in the root
// package (never taken as a value), and at every call site the argument for
// that parameter is a function literal or a named function.
func (ix *index) dynamicCallVerified(rootPkg *packages.Package, u unit, fun ast.Expr, uses map[*types.Func][]ast.Expr, calls map[*types.Func][][]ast.Expr) bool {
	if u.pkg != rootPkg || u.fn == nil || u.fn.Exported() {
		return false
	}
	id, ok := calleeExpr(fun).(*ast.Ident)
	if !ok {
		return false
	}
	v, ok := rootPkg.TypesInfo.Uses[id].(*types.Var)
	if !ok {
		return false
	}
	sig := u.fn.Type().(*types.Signature)
	idx := -1
	for i := 0; i < sig.Params().Len(); i++ {
		if sig.Params().At(i) == v {
			idx = i
		}
	}
	if idx < 0 {
		return false
	}
	fn := u.fn.Origin()
	sites := calls[fn]
	if len(sites) == 0 || len(sites) != len(uses[fn]) {
		return false
	}
	for _, args := range sites {
		if idx >= len(args) {
			return false
		}
		a := calleeExpr(args[idx])
		if _, ok := a.(*ast.FuncLit); ok {
			continue
		}
		if staticCallee(rootPkg, a) != nil {
			continue
		}
		return false
	}
	return true
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
	if isStoreType(recvOf(fn)) && allowedStoreMethodsInsideDoor[fn.Name()] {
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
