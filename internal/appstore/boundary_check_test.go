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
	modulePrefix   = "github.com/PerpetualSoftware/pad/internal/"
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

	// Index every module function declaration by its object.
	funcs := map[*types.Func]funcSite{}
	var storePkg *types.Package
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.PkgPath == storePkgPath {
			storePkg = p.Types
		}
		if !strings.HasPrefix(p.PkgPath, modulePrefix) {
			return
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok {
					if obj, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
						funcs[obj] = funcSite{decl: fd, pkg: p}
					}
				}
			}
		}
	})
	if storePkg == nil {
		t.Fatalf("load: internal/store is not in the closure; nothing to guard")
	}
	storeImpls := storeTypes(storePkg)
	result := map[string][]violation{}
	for _, rootPkg := range pkgs {
		result[rootPkg.PkgPath] = walkRoot(rootPkg, funcs, storeImpls)
	}
	return result
}

func walkRoot(rootPkg *packages.Package, funcs map[*types.Func]funcSite, storeImpls []types.Type) []violation {
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
		if iface, ok := v.Type().Underlying().(*types.Interface); ok && !isErrorIface(v.Type()) && implementedByStore(iface, storeImpls) {
			report(rootPkg, id.Pos(), "interface-decl", "%s has interface type %s, which an internal/store type implements", v.Name(), v.Type())
		}
	}

	seen := map[*types.Func]bool{}
	var queue []*types.Func
	enqueue := func(fn *types.Func) {
		if _, ok := funcs[fn]; ok && !seen[fn] {
			seen[fn] = true
			queue = append(queue, fn)
		}
	}
	for obj, site := range funcs {
		if site.pkg == rootPkg {
			enqueue(obj)
		}
	}

	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		site := funcs[fn]
		p := site.pkg
		door := isDoor(fn)
		ast.Inspect(site.decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				sel := p.TypesInfo.Selections[n]
				if sel == nil {
					return true
				}
				recv := sel.Recv()
				// Rule 3: Store's raw *sql.DB.
				if sel.Kind() == types.FieldVal && isStoreType(recv) && sel.Obj().Name() == "db" && fn.Name() != "BeginFenced" {
					report(p, n.Pos(), "raw-db", "%s reads Store.db", fn.FullName())
				}
				m, ok := sel.Obj().(*types.Func)
				if !ok {
					return true
				}
				// Rule 4: an interface call an internal/store type could answer.
				// The door is exempt: it is reviewed code inside the store
				// and calls the store's own dialect interface.
				if iface, ok := recv.Underlying().(*types.Interface); ok {
					if !door && !isErrorIface(recv) && implementedByStore(iface, storeImpls) {
						report(p, n.Pos(), "interface-call", "%s calls %s on interface %s, which an internal/store type implements", fn.FullName(), m.Name(), recv)
					}
					return true
				}
				// Rule 1: *store.Store methods.
				if isStoreType(recv) {
					allowed := allowedStoreMethods[m.Name()] || (door && allowedStoreMethodsInsideDoor[m.Name()])
					if !allowed {
						report(p, n.Pos(), "store-method", "%s calls (*store.Store).%s, which is not on the reviewed allow-list", fn.FullName(), m.Name())
					}
				}
				// Rule 2: database/sql outside the door.
				if isSQLHandle(recv) && !door {
					report(p, n.Pos(), "raw-sql", "%s calls %s.%s outside FencedTx", fn.FullName(), types.TypeString(recv, nil), m.Name())
				}
				// Rule 7: the ordinary attachments Put.
				if m.Pkg() != nil && m.Pkg().Path() == attachmentsPkg && m.Name() == "Put" {
					report(p, n.Pos(), "attachments-put", "%s reaches attachments %s.Put (dedup timing oracle)", fn.FullName(), types.TypeString(recv, nil))
				}
				enqueue(m)
			case *ast.Ident:
				obj := p.TypesInfo.Uses[n]
				if obj == nil {
					return true
				}
				// Rule 6: reflect and unsafe.
				if obj.Pkg() != nil && (obj.Pkg().Path() == "reflect" || obj.Pkg().Path() == "unsafe") {
					report(p, n.Pos(), "reflect-unsafe", "%s uses %s.%s", fn.FullName(), obj.Pkg().Path(), obj.Name())
				}
				if f, ok := obj.(*types.Func); ok {
					if f.Name() == "DB" && isStoreType(recvOf(f)) && fn.Name() != "BeginFenced" {
						report(p, n.Pos(), "raw-db", "%s calls (*store.Store).DB", fn.FullName())
					}
					enqueue(f)
				}
			}
			return true
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return dedupe(out)
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
