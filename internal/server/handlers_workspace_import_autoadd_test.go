package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
)

// BUG-2794. Create added the new workspace to the calling OAuth connection's
// allow-list (maybeAutoAddCreatorConnection); neither import door did, so a
// connection with an explicit allow-list got a 201 for an imported workspace
// it could not then see. All three doors now run finishWorkspaceMint on their
// success path.

// connectionHas reports whether the consent env's OAuth connection lists slug.
func (e *consentEnv) connectionHas(t *testing.T, slug string) bool {
	t.Helper()
	slugs, err := e.srv.store.ListConnectionWorkspaceSlugs(e.requestID)
	if err != nil {
		t.Fatalf("ListConnectionWorkspaceSlugs: %v", err)
	}
	for _, s := range slugs {
		if s == slug {
			return true
		}
	}
	return false
}

// tarBundle exports a scratch workspace from the env's own server as a tar
// bundle, through the real export route, as the PAT caller (no OAuth
// identity, so the export itself adds nothing to any allow-list).
func (e *consentEnv) tarBundle(t *testing.T) []byte {
	t.Helper()
	// The bundle door refuses with 503 attachments_disabled on a server with no
	// attachment storage, so wire one exactly as testServerWithAttachments does.
	fs, err := attachments.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	e.srv.SetAttachments(reg, 0)
	src := e.mustExist(t, "Consent Home")
	rr := e.do("GET", "/api/v1/workspaces/"+src.Slug+"/export?format=tar", "", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("export tar: %d %s", rr.Code, rr.Body.String())
	}
	return rr.Body.Bytes()
}

func TestImportWorkspace_JSON_AutoAddsToTheOAuthConnection(t *testing.T) {
	e := newConsentEnv(t, true)
	rr := e.do("POST", "/api/v1/workspaces/import?name=Imported-JSON",
		"application/json", e.exportBody(t), e.requestID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	ws := e.mustExist(t, "Imported-JSON")
	if !e.connectionHas(t, ws.Slug) {
		t.Errorf("the imported workspace %q is not on the importing OAuth connection's allow-list: "+
			"a 201 for a workspace the caller then cannot see (BUG-2794)", ws.Slug)
	}
}

func TestImportWorkspace_Bundle_AutoAddsToTheOAuthConnection(t *testing.T) {
	e := newConsentEnv(t, true)
	bundle := e.tarBundle(t)
	rr := e.do("POST", "/api/v1/workspaces/import?name=Imported-Bundle",
		"application/gzip", bundle, e.requestID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	ws := e.mustExist(t, "Imported-Bundle")
	if !e.connectionHas(t, ws.Slug) {
		t.Errorf("the bundle-imported workspace %q is not on the importing OAuth connection's allow-list (BUG-2794)", ws.Slug)
	}
}

// A FAILED bundle import can leave a partial workspace (the keep path). It
// must not join the connection's allow-list: the auto-add is a success-path
// step. The precondition asserts the partial workspace really exists, so the
// absence below is not just the absence of any workspace.
func TestImportWorkspace_FailedBundle_DoesNotAutoAddThePartialWorkspace(t *testing.T) {
	e := newConsentEnv(t, true)
	bad := withUndecodableManifest(t, e.tarBundle(t))
	rr := e.do("POST", "/api/v1/workspaces/import?name=Imported-Partial",
		"application/gzip", bad, e.requestID)
	if rr.Code == http.StatusCreated {
		t.Fatalf("PRECONDITION: the corrupted bundle imported (201); this leg needs a failing import")
	}
	ws := e.lookupByName(t, "Imported-Partial")
	if ws == nil {
		t.Skipf("the failing import left no partial workspace (status %d), so there is nothing that could have been wrongly added", rr.Code)
	}
	if e.connectionHas(t, ws.Slug) {
		t.Errorf("a FAILED bundle import (status %d) put its partial workspace %q on the OAuth connection's allow-list", rr.Code, ws.Slug)
	}
}

// The structural half: maybeAutoAddCreatorConnection has exactly ONE caller,
// finishWorkspaceMint, so a future mint door cannot call one side effect and
// skip another, and cannot skip them all by forgetting a line only create had.
func TestAutoAddHasOneCaller_FinishWorkspaceMint(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "maybeAutoAddCreatorConnection" {
						callers = append(callers, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if len(callers) != 1 || callers[0] != "finishWorkspaceMint" {
		t.Errorf("maybeAutoAddCreatorConnection is called from %v, want exactly [finishWorkspaceMint]: "+
			"a mint door calls finishWorkspaceMint, not the side effects one by one", callers)
	}
}
