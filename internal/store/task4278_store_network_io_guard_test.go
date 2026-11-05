package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// IsDBOutcomeUnknown reads *net.OpError as "the database connection failed"
// (busy_error.go). That is only sound while the store does no other network
// I/O, so a non-test file in this package may not import a package that
// makes such calls: every "net/..." path (net/url only parses strings), "net"
// itself outside busy_error.go (the classifier), crypto/tls, syscall,
// golang.org/x/net, and the HTTP, S3 and RPC client modules.
//
// Reach through internal packages (TASK-4278): the store
// imports internal/attachments, which itself imports net/http, but only for
// http.DetectContentType (a pure byte sniff) and a local-filesystem store the
// package store never constructs. The store CALLS only pure helpers from it
// (storeAttachmentsAllowed below); no store path reaches a blob or network
// backend. The other internal imports (appmanifest, collections, items,
// models, ...) reach "net" only for string parsing (net.ParseIP and the like).
// A new use of internal/attachments from the store fails the second test.
var storeBannedImports = []string{
	"crypto/tls", "syscall", "golang.org/x/net",
	"github.com/aws/", "github.com/minio/", "github.com/go-resty/", "github.com/valyala/fasthttp",
	"github.com/gorilla/websocket", "github.com/redis/go-redis", "google.golang.org/grpc",
	"cloud.google.com/", "github.com/Azure/", "gocloud.dev",
}

func storeImportBanned(path, file string) bool {
	if path == "net" {
		return file != "busy_error.go"
	}
	if strings.HasPrefix(path, "net/") {
		return path != "net/url"
	}
	for _, b := range storeBannedImports {
		if path == strings.TrimSuffix(b, "/") || strings.HasPrefix(path, b) {
			return true
		}
	}
	return false
}

func TestStoreHasNoNonDatabaseNetworkIO(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if storeImportBanned(path, f) {
				t.Errorf("%s imports %q: store network I/O would be misread as a database interruption by IsDBOutcomeUnknown; classify it or keep it out of this package", f, path)
			}
		}
	}
	if checked < 50 {
		t.Fatalf("only %d store files checked; the glob is wrong", checked)
	}
}

// storeAttachmentsAllowed is every internal/attachments name the store may
// use: pure string and enum helpers, no store (blob) type.
var storeAttachmentsAllowed = map[string]bool{
	"DroppedFilenameRune": true, "ServedFilename": true, "FilenameSource": true,
	"FilenameSourceUnknown": true, "ValidFilenameSource": true,
}

func TestStoreUsesOnlyPureAttachmentsHelpers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		n, bad := scanAttachmentsUses(f, af)
		seen += n
		for _, b := range bad {
			t.Error(b)
		}
	}
	if seen == 0 {
		t.Fatal("no attachments.X use found; the scan is wrong")
	}
}

// scanAttachmentsUses counts the uses of the internal/attachments package in
// af and reports every one outside the audited pure helpers. The local name
// comes from the ImportSpec, so an alias is followed, a dot import is
// checked at each bare identifier, and a blank import is ignored.
func scanAttachmentsUses(f string, af *ast.File) (int, []string) {
	local, dot := "", false
	for _, imp := range af.Imports {
		if !strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/internal/attachments") {
			continue
		}
		switch {
		case imp.Name == nil:
			local = "attachments"
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name == "_":
		default:
			local = imp.Name.Name
		}
	}
	if local == "" && !dot {
		return 0, nil
	}
	seen := 0
	var bad []string
	// The Sel of a selector (foo.NewFSStore) is a field or method name, not a
	// bare identifier a dot import could have supplied. A SelectorExpr is
	// visited before its children, so the set is filled in time.
	sels := map[*ast.Ident]bool{}
	ast.Inspect(af, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			sels[x.Sel] = true
			if id, ok := x.X.(*ast.Ident); ok && local != "" && id.Name == local {
				seen++
				if !storeAttachmentsAllowed[x.Sel.Name] {
					bad = append(bad, fmt.Sprintf("%s uses %s.%s: only the pure helpers %v are audited as free of blob and network I/O", f, local, x.Sel.Name, storeAttachmentsAllowed))
				}
			}
		case *ast.Ident:
			// A dot import: any bare identifier naming a non-allowed
			// attachments export. Only exported names that the package
			// declares can be reached this way, and an unexported local
			// cannot collide with them in the audited helper set.
			if dot && !sels[x] && x.Obj == nil && ast.IsExported(x.Name) && !storeAttachmentsAllowed[x.Name] && storeAttachmentsExports[x.Name] {
				seen++
				bad = append(bad, fmt.Sprintf("%s dot-imports attachments and uses %s: only the pure helpers %v are audited", f, x.Name, storeAttachmentsAllowed))
			}
		}
		return true
	})
	return seen, bad
}

// storeAttachmentsExports lists exports of internal/attachments that a dot
// import could reach; it is built from the package source so the list cannot
// go stale.
var storeAttachmentsExports = func() map[string]bool {
	out := map[string]bool{}
	files, _ := filepath.Glob("../attachments/*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			continue
		}
		for name := range af.Scope.Objects {
			if ast.IsExported(name) {
				out[name] = true
			}
		}
	}
	return out
}()

// The scan must follow an aliased and a dot import; a blank one uses nothing.
func TestAttachmentsScanFollowsAliasAndDotImports(t *testing.T) {
	const imp = `"github.com/PerpetualSoftware/pad/internal/attachments"`
	if !storeAttachmentsExports["NewFSStore"] {
		t.Fatal("NewFSStore is not an export of internal/attachments; the fixture is stale")
	}
	cases := map[string]struct {
		src     string
		wantBad bool
	}{
		"aliased helper":                  {"package s\nimport att " + imp + "\nvar _ = att.NewFSStore\n", true},
		"plain helper":                    {"package s\nimport " + imp + "\nvar _ = attachments.NewFSStore\n", true},
		"dot helper":                      {"package s\nimport . " + imp + "\nvar _ = NewFSStore\n", true},
		"aliased pure":                    {"package s\nimport att " + imp + "\nvar _ = att.ValidFilenameSource\n", false},
		"blank":                           {"package s\nimport _ " + imp + "\n", false},
		"dot, unrelated selector":         {"package s\nimport . " + imp + "\nvar foo struct{ NewFSStore int }\nvar _ = foo.NewFSStore\n", false},
		"dot, bare use beside a selector": {"package s\nimport . " + imp + "\nvar foo struct{ NewFSStore int }\nvar _ = foo.NewFSStore\nvar _ = NewFSStore\n", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			af, err := parser.ParseFile(token.NewFileSet(), "snippet.go", tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, bad := scanAttachmentsUses("snippet.go", af)
			if (len(bad) > 0) != tc.wantBad {
				t.Errorf("violations = %v, want bad=%v", bad, tc.wantBad)
			}
		})
	}
}
