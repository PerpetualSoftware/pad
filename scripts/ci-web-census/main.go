// ci-web-census checks all tracked Go sources, independent of build tags.
// It conservatively checks references, including diagnostic strings and fixture
// paths. Constants, concatenations and path joins are resolved without executing
// code. A dynamic suffix after a known web root is an error, even in a listed
// package. Calls and function values propagate through local and imported helpers.
package main

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const module = "github.com/PerpetualSoftware/pad"
const unknown = "\x00"

var webPath = regexp.MustCompile(`(^|[^a-zA-Z0-9_-])web/`)

type source struct {
	file, pkg string
	tree      *ast.File
	imports   map[string]string
}

type unit struct {
	src     *source
	name    string
	node    ast.Node
	refs    map[string]bool
	web, pg bool
}

func packagePath(file string) string {
	dir := path.Dir(file)
	if dir == "." {
		return "."
	}
	return "./" + dir
}

func literal(e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		value, _ := strconv.Unquote(lit.Value)
		return value
	}
	return ""
}

// resolve preserves unknown pieces rather than losing a partially known path.
// Unknown prefixes (a repository or temp root) don't hide a known web suffix;
// unknown suffixes cannot be silently accepted as a resolved reference.
func resolve(e ast.Expr, globals map[string]ast.Expr, seen map[ast.Expr]bool) string {
	if e == nil || seen[e] {
		return unknown
	}
	seen[e] = true
	defer delete(seen, e)
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			return literal(x)
		}
	case *ast.ParenExpr:
		return resolve(x.X, globals, seen)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return resolve(x.X, globals, seen) + resolve(x.Y, globals, seen)
		}
	case *ast.Ident:
		if x.Obj != nil {
			switch d := x.Obj.Decl.(type) {
			case *ast.ValueSpec:
				for i, n := range d.Names {
					if n.Name == x.Name && i < len(d.Values) {
						return resolve(d.Values[i], globals, seen)
					}
				}
			case *ast.AssignStmt:
				for i, n := range d.Lhs {
					if n == x || identName(n) == x.Name {
						if i < len(d.Rhs) {
							return resolve(d.Rhs[i], globals, seen)
						}
					}
				}
			}
		}
		if value, ok := globals[x.Name]; ok {
			return resolve(value, globals, seen)
		}
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			base := identName(sel.X)
			if imported, ok := globals["#import:"+base]; ok {
				switch literal(imported) {
				case "path/filepath":
					base = "filepath"
				case "path":
					base = "path"
				case "fmt":
					base = "fmt"
				default:
					base = ""
				}
			}
			if base == "fmt" && sel.Sel.Name == "Sprintf" && len(x.Args) > 0 {
				format := resolve(x.Args[0], globals, seen)
				args := []any{}
				for _, arg := range x.Args[1:] {
					value := resolve(arg, globals, seen)
					if strings.Contains(value, unknown) {
						return format + unknown
					}
					args = append(args, value)
				}
				return fmt.Sprintf(format, args...)
			}
			if (base == "filepath" || base == "path") && sel.Sel.Name == "Join" {
				parts := []string{}
				for _, arg := range x.Args {
					parts = append(parts, resolve(arg, globals, seen))
				}
				return strings.Join(parts, "/")
			}
			if (base == "filepath" || base == "path") && (sel.Sel.Name == "Clean" || sel.Sel.Name == "FromSlash" || sel.Sel.Name == "Abs") && len(x.Args) == 1 {
				return resolve(x.Args[0], globals, seen)
			}
		}
	}
	return unknown
}

func identName(e ast.Expr) string {
	if x, ok := e.(*ast.Ident); ok {
		return x.Name
	}
	return ""
}

func isWeb(value string) bool {
	value = strings.ReplaceAll(value, "\\", "/")
	return webPath.MatchString(value) || strings.HasSuffix(value, "/web")
}

func audit(files map[string]string, parity, allow map[string]string, pgPackages map[string]bool, pgAllow map[string]string) []string {
	errors := []string{}
	// Operator reviews classify packages, not graph boundaries. Keep them separate
	// from SQLite reviews so they cannot suppress storetest.Cleanup propagation.
	noTests := map[string]bool{}
	sqliteAllow := map[string]string{}
	for pkg, review := range pgAllow {
		if strings.HasPrefix(review, "no-tests:") {
			noTests[pkg] = true
		} else {
			sqliteAllow[pkg] = review
		}
	}
	sources := []*source{}
	globals := map[string]map[string]ast.Expr{}
	units := []*unit{}
	for file, text := range files {
		pkg := packagePath(file)
		if _, reviewed := noTests[pkg]; reviewed && strings.HasSuffix(file, "_test.go") {
			noTests[pkg] = false
			errors = append(errors, file+": no-tests entry invalid: package must be added to ci-pg-packages.txt, or re-reviewed")
		}
		tree, err := parser.ParseFile(token.NewFileSet(), file, text, parser.ParseComments)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}
		if _, reviewed := noTests[pkg]; reviewed && tree.Name.Name != "main" {
			noTests[pkg] = false
			errors = append(errors, file+": no-tests entry requires package main")
		}
		src := &source{file: file, pkg: pkg, tree: tree, imports: map[string]string{}}
		sources = append(sources, src)
		if globals[src.pkg] == nil {
			globals[src.pkg] = map[string]ast.Expr{}
		}
		for _, imp := range tree.Imports {
			value := literal(imp.Path)
			alias := path.Base(value)
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			src.imports[alias] = value
			if alias == "." && (value == "os" || value == "io/fs" || value == "path" || value == "path/filepath") {
				errors = append(errors, file+": unresolved filesystem dot import")
			}
		}
		for _, decl := range tree.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && (gen.Tok == token.CONST || gen.Tok == token.VAR) {
				for _, spec := range gen.Specs {
					v := spec.(*ast.ValueSpec)
					for i, n := range v.Names {
						if i < len(v.Values) {
							globals[src.pkg][n.Name] = v.Values[i]
						}
					}
				}
			}
		}
	}
	for _, src := range sources {
		for _, decl := range src.tree.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				units = append(units, &unit{src: src, name: d.Name.Name, node: d, refs: map[string]bool{}})
			case *ast.GenDecl:
				if d.Tok == token.IMPORT {
					continue
				}
				for _, spec := range d.Specs {
					if typ, ok := spec.(*ast.TypeSpec); ok {
						units = append(units, &unit{src: src, name: typ.Name.Name, node: typ.Type, refs: map[string]bool{}})
					}
					if v, ok := spec.(*ast.ValueSpec); ok {
						// Each binding has its own node in the reference graph, so constants
						// and function aliases used from another file cannot disappear.
						for i, n := range v.Names {
							if i < len(v.Values) {
								units = append(units, &unit{src: src, name: n.Name, node: v.Values[i], refs: map[string]bool{}})
							}
						}
					}
				}
			}
		}
		for _, group := range src.tree.Comments {
			for _, comment := range group.List {
				if !strings.HasPrefix(comment.Text, "//go:embed ") {
					continue
				}
				for _, pattern := range strings.Fields(strings.TrimPrefix(comment.Text, "//go:embed ")) {
					pattern = strings.Trim(pattern, "\"`")
					if (isWeb(strings.TrimPrefix(pattern, "all:")) || pattern == "web" || pattern == "all:web") && allow[src.file+"\t"+pattern] == "" {
						// Embeds cannot be selected with -run. Cover the whole package or
						// explicitly review the production embed in the allow-file.
						if filter, ok := parity[src.pkg]; !ok || filter != "" {
							errors = append(errors, src.file+": uncovered web embed "+pattern)
						}
					}
				}
			}
		}
	}
	byID := map[string][]*unit{}
	for _, u := range units {
		byID[u.src.pkg+"."+u.name] = append(byID[u.src.pkg+"."+u.name], u)
	}
	for _, u := range units {
		bindings := map[string]ast.Expr{}
		for name, value := range globals[u.src.pkg] {
			bindings[name] = value
		}
		for alias, imported := range u.src.imports {
			bindings["#import:"+alias] = &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(imported)}
		}
		usesSQL, pgDriverPiece := false, false
		selectorNames := map[*ast.Ident]bool{}
		ast.Inspect(u.node, func(n ast.Node) bool {
			if n == nil {
				return true
			}
			if expr, ok := n.(ast.Expr); ok {
				value := resolve(expr, bindings, map[ast.Expr]bool{})
				if strings.Contains(value, "pgx") || strings.Contains(value, "postgres") {
					pgDriverPiece = true
				}
				if isWeb(value) && allow[u.src.file+"\t"+value] == "" {
					u.web = true
					// An unknown root before web is safe for scope classification; a
					// dynamic component beneath it must be made mechanically resolvable.
					start := strings.Index(strings.ReplaceAll(value, "\\", "/"), "web/")
					if start >= 0 && strings.Contains(value[start:], unknown) {
						errors = append(errors, u.src.file+": unresolved web path in "+u.name)
					}
				}
			}
			switch x := n.(type) {
			case *ast.BasicLit:
				value := literal(x)
				if value == "PAD_TEST_"+"POSTGRES_URL" || value == "PAD_TEST_"+"POSTGRES_TOOLS_URL" {
					u.pg = true
				}
			case *ast.Ident:
				if selectorNames[x] {
					break
				}
				u.refs[u.src.pkg+"."+x.Name] = true
				if dotted := u.src.imports["."]; strings.HasPrefix(dotted, module+"/") {
					u.refs["./"+strings.TrimPrefix(dotted, module+"/")+"."+x.Name] = true
				}
				if x.Name == "NewPostgres" || x.Name == "testStorePostgres" || x.Name == "newPostgresConn" {
					u.pg = true
				}
			case *ast.SelectorExpr:
				selectorNames[x.Sel] = true
				imported := u.src.imports[identName(x.X)]
				if imported == "" {
					id := u.src.pkg + "." + x.Sel.Name
					for _, target := range byID[id] {
						if fn, ok := target.node.(*ast.FuncDecl); ok && fn.Recv != nil {
							u.refs[id] = true
						}
					}
				}
				if strings.HasPrefix(imported, module+"/") {
					u.refs["./"+strings.TrimPrefix(imported, module+"/")+"."+x.Sel.Name] = true
				}
				if x.Sel.Name == "NewPostgres" || (strings.Contains(imported, "jackc/pgx") &&
					(x.Sel.Name == "Connect" || x.Sel.Name == "ConnectConfig" || x.Sel.Name == "ConnectWithOptions" || x.Sel.Name == "New" || x.Sel.Name == "NewWithConfig")) {
					u.pg = true
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					imported := u.src.imports[identName(sel.X)]
					if (imported == "os" || imported == "io/fs") && len(x.Args) > 0 {
						if resolve(x.Args[0], bindings, map[ast.Expr]bool{}) == "web" {
							u.web = true
						}
					}
					if imported == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") && len(x.Args) > 0 {
						env := resolve(x.Args[0], bindings, map[ast.Expr]bool{})
						if strings.HasPrefix(env, "PAD_TEST_POSTGRES") {
							u.pg = true
						}
					}
					if imported == "database/sql" && (sel.Sel.Name == "Open" || sel.Sel.Name == "OpenDB") {
						usesSQL = true
						driver := unknown
						if len(x.Args) > 0 {
							driver = resolve(x.Args[0], bindings, map[ast.Expr]bool{})
						}
						if strings.Contains(driver, "pg") || strings.Contains(driver, "postgres") || strings.Contains(driver, unknown) {
							u.pg = true
						}
					}
				}
			}
			return true
		})
		if usesSQL && pgDriverPiece {
			u.pg = true
		}
	}
	// Reviewed nonconnecting helpers are graph boundaries only while their
	// exact source hash matches. Their own PG references still need coverage.
	boundaries := map[string]bool{}
	for id, review := range sqliteAllow {
		if !strings.HasPrefix(review, "sha256:") {
			continue
		}
		for _, u := range byID[id] {
			text := files[u.src.file][int(u.node.Pos())-1 : int(u.node.End())-1]
			want := strings.Fields(review)[0]
			if fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(text))) != want {
				errors = append(errors, u.src.file+": reviewed nonconnecting helper changed: "+u.name)
			} else {
				boundaries[id] = true
			}
		}
	}
	// Resolve wrappers and function values to a fixed point, across packages too.
	for changed := true; changed; {
		changed = false
		for _, u := range units {
			for ref := range u.refs {
				if boundaries[ref] {
					continue
				}
				if sqliteAllow[u.src.pkg] != "" && ref == "./internal/store/storetest.Cleanup" {
					continue
				}
				for _, target := range byID[ref] {
					if target.pg && !u.pg {
						u.pg = true
						changed = true
					}
					if target.web && !u.web {
						u.web = true
						changed = true
					}
				}
			}
		}
	}
	// Without go/types, a selector on an imported receiver cannot be resolved
	// safely. Importing a package exposing PG-capable methods requires review.
	pgMethods := map[string]bool{}
	pgReferences := map[string]bool{}
	for _, u := range units {
		if u.pg {
			pgReferences[u.src.pkg] = true
		}
		if fn, ok := u.node.(*ast.FuncDecl); ok && fn.Recv != nil && ast.IsExported(u.name) && u.pg {
			pgMethods[u.src.pkg] = true
		}
	}
	for _, src := range sources {
		for _, imported := range src.imports {
			pkg := "./" + strings.TrimPrefix(imported, module+"/")
			if pgMethods[pkg] {
				pgReferences[src.pkg] = true
				if !pgPackages[src.pkg] && !noTests[src.pkg] {
					errors = append(errors, src.file+": unresolved PostgreSQL-capable receiver import "+imported)
				}
			}
		}
		// Imports of PG test helpers require review even if calls are indirect or
		// unresolved. SQLite exceptions are deliberately restricted to two symbols.
		for alias, imp := range src.imports {
			if imp != module+"/internal/store/storetest" {
				continue
			}
			pgReferences[src.pkg] = true
			if pgPackages[src.pkg] || noTests[src.pkg] {
				continue
			}
			if sqliteAllow[src.pkg] == "" {
				errors = append(errors, src.file+": unclassified PostgreSQL-capable storetest import")
				continue
			}
			ast.Inspect(src.tree, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && identName(sel.X) == alias && sel.Sel.Name != "NewSQLite" && sel.Sel.Name != "Cleanup" {
					errors = append(errors, src.file+": SQLite-only exception used "+sel.Sel.Name)
				}
				return true
			})
		}
	}
	for pkg := range noTests {
		if !pgReferences[pkg] {
			errors = append(errors, pkg+": stale no-tests entry: package has no PostgreSQL references")
		}
	}
	for _, u := range units {
		if u.pg && !pgPackages[u.src.pkg] && !noTests[u.src.pkg] {
			errors = append(errors, u.src.file+": unlisted PostgreSQL reference/wrapper "+u.name)
		}
		if !u.web {
			continue
		}
		filter, covered := parity[u.src.pkg]
		if !covered {
			errors = append(errors, u.src.file+": uncovered web reference in "+u.name)
			continue
		}
		if filter == "" {
			continue
		}
		re, err := regexp.Compile(filter)
		if err != nil {
			errors = append(errors, "invalid parity regex: "+err.Error())
			continue
		}
		if strings.HasPrefix(u.name, "Test") && !re.MatchString(u.name) {
			errors = append(errors, u.src.file+": web reader "+u.name+" is outside parity -run")
		} else if !strings.HasPrefix(u.name, "Test") {
			found := false
			for _, caller := range units {
				if caller.src.pkg == u.src.pkg && strings.HasPrefix(caller.name, "Test") && caller.web {
					found = true
				}
			}
			if !found {
				errors = append(errors, u.src.file+": unresolved web helper caller "+u.name)
			}
		}
	}
	sort.Strings(errors)
	return errors
}

func readLines(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	lines := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func run() error {
	lines, err := readLines("scripts/ci-web-parity.txt")
	if err != nil {
		return err
	}
	parity := map[string]string{}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 2 || !strings.HasPrefix(fields[0], "./") {
			return fmt.Errorf("invalid parity entry: %q", line)
		}
		filter := ""
		if len(fields) == 2 {
			filter = fields[1]
			if _, err := regexp.Compile(filter); err != nil {
				return err
			}
		}
		if _, exists := parity[fields[0]]; exists {
			return fmt.Errorf("duplicate parity package: %s", fields[0])
		}
		parity[fields[0]] = filter
	}
	allow := map[string]string{}
	lines, err = readLines("scripts/ci-web-census-allow.txt")
	if err != nil {
		return err
	}
	for _, line := range lines {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || fields[2] == "" {
			return fmt.Errorf("allow entry needs file, exact pattern, reason: %q", line)
		}
		allow[fields[0]+"\t"+fields[1]] = fields[2]
	}
	pg := map[string]bool{}
	lines, err = readLines("scripts/ci-pg-packages.txt")
	if err != nil {
		return err
	}
	for _, line := range lines {
		pg[strings.TrimSpace(line)] = true
	}
	pgAllow := map[string]string{}
	lines, err = readLines("scripts/ci-pg-census-allow.txt")
	if err != nil {
		return err
	}
	for _, line := range lines {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || fields[1] == "" {
			return fmt.Errorf("PostgreSQL allow entry needs package and reason: %q", line)
		}
		pgAllow[fields[0]] = fields[1]
		if strings.HasPrefix(fields[1], "no-tests:") {
			// Check the directory too: even untracked or ignored test files invalidate
			// the review. The source audit below also checks tracked, build-tagged tests.
			entries, err := os.ReadDir(fields[0])
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), "_test.go") {
					return fmt.Errorf("%s: no-tests entry invalid: package must be added to ci-pg-packages.txt, or re-reviewed (%s exists)", fields[0], entry.Name())
				}
			}
		}
	}
	output, err := exec.Command("git", "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		return err
	}
	files := map[string]string{}
	for _, file := range strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00") {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		files[file] = string(data)
	}
	if len(files) == 0 {
		return fmt.Errorf("no tracked Go sources")
	}
	errors := audit(files, parity, allow, pg, pgAllow)
	if len(errors) > 0 {
		return fmt.Errorf("source census failed:\n%s", strings.Join(errors, "\n"))
	}
	fmt.Printf("Web and PostgreSQL census passed (%d tracked Go files).\n", len(files))
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
