package server

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

// bodyReaderSelectors are the members of net/http.Request that read the
// request BODY, or expose what parsing it produced. FormValue, PostFormValue,
// ParseForm, ParseMultipartForm and FormFile parse it; MultipartReader streams
// it; PostForm and MultipartForm hold the parsed result.
var bodyReaderSelectors = map[string]bool{
	"Body": true, "FormValue": true, "PostFormValue": true, "ParseForm": true,
	"ParseMultipartForm": true, "MultipartForm": true, "PostForm": true,
	"MultipartReader": true, "FormFile": true,
}

var (
	censusLoadMu    sync.Mutex
	censusLoadCache = map[string]*packages.Package{}
)

// loadCensusPackage type-checks the one package in dir, once per test binary:
// the reader census and the handoff census ask different questions of the same
// load, and a go/packages load of this package is several seconds.
func loadCensusPackage(t *testing.T, dir string) *packages.Package {
	t.Helper()
	censusLoadMu.Lock()
	defer censusLoadMu.Unlock()
	if p, ok := censusLoadCache[dir]; ok {
		return p
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports |
			packages.NeedDeps | packages.NeedModule,
		Dir:   dir,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("load %s: want 1 package, got %d", dir, len(pkgs))
	}
	if len(pkgs[0].Errors) > 0 {
		t.Fatalf("load %s: %v", dir, pkgs[0].Errors)
	}
	censusLoadCache[dir] = pkgs[0]
	return pkgs[0]
}

// bodyReadSite is one selector expression that reaches a request body.
type bodyReadSite struct {
	file string // base name
	fn   string // enclosing top-level function, "Recv.Method" for methods
	sel  string // the member selected
	line int
}

// key is the unit the accounting table is written in: one function's use of
// one member. Line numbers are not part of it, so an unrelated edit above a
// reader does not churn the table; a SECOND use of the same member in the
// same function changes that key's count instead.
func (s bodyReadSite) key() string { return s.file + "::" + s.fn + "::" + s.sel }

var (
	bodyCensusMu    sync.Mutex
	bodyCensusCache = map[string][]bodyReadSite{}
)

// collectRequestBodyReads type-checks the package in dir and returns every
// selector whose RESOLVED object is one of net/http.Request's body readers
// (BUG-2820).
//
// This asks the type checker, not the spelling. The parser it replaces matched
// the literal `http.Request` in a signature and then names bound to it, so a
// request reached through a struct field, a context value, a type alias, an
// embedded field, an import alias, a dot import, a `var` declaration, a
// call-derived alias, a named result or a range binding was invisible. Here
// a selector counts when go/types resolves it to the SAME object that
// looking the member up on net/http.Request yields, which is what every one of
// those forms has in common. Promotion through embedding resolves to the same
// field or method, and an alias is the same type, so none needs a special
// case. A same-named member of another type (a Response's Body, a struct's
// own Body field) resolves to a different object and is not counted.
func collectRequestBodyReads(t *testing.T, dir string) []bodyReadSite {
	t.Helper()
	bodyCensusMu.Lock()
	defer bodyCensusMu.Unlock()
	if cached, ok := bodyCensusCache[dir]; ok {
		return cached
	}

	pkg := loadCensusPackage(t, dir)

	httpPkg := findImportedPackage(pkg, "net/http")
	if httpPkg == nil {
		t.Fatalf("%s does not import net/http, so there is no request type to look for", dir)
	}
	reqObj := httpPkg.Types.Scope().Lookup("Request")
	if reqObj == nil {
		t.Fatal("net/http.Request not found")
	}
	reqType := reqObj.Type()
	want := map[types.Object]string{}
	for name := range bodyReaderSelectors {
		obj, _, _ := types.LookupFieldOrMethod(reqType, true, nil, name)
		if obj == nil {
			t.Fatalf("net/http.Request has no member %q; the selector list is stale", name)
		}
		want[obj] = name
	}

	var sites []bodyReadSite
	for _, f := range pkg.Syntax {
		file := filepath.Base(pkg.Fset.File(f.Pos()).Name())
		for _, decl := range f.Decls {
			fn := declName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				selection, ok := pkg.TypesInfo.Selections[sel]
				if !ok {
					return true
				}
				if name, hit := want[selection.Obj()]; hit {
					sites = append(sites, bodyReadSite{
						file: file, fn: fn, sel: name,
						line: pkg.Fset.Position(sel.Pos()).Line,
					})
				}
				return true
			})
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].file != sites[j].file {
			return sites[i].file < sites[j].file
		}
		return sites[i].line < sites[j].line
	})
	bodyCensusCache[dir] = sites
	return sites
}

func findImportedPackage(pkg *packages.Package, path string) *packages.Package {
	seen := map[*packages.Package]bool{}
	var walk func(*packages.Package) *packages.Package
	walk = func(p *packages.Package) *packages.Package {
		if seen[p] {
			return nil
		}
		seen[p] = true
		if p.PkgPath == path {
			return p
		}
		for _, imp := range p.Imports {
			if found := walk(imp); found != nil {
				return found
			}
		}
		return nil
	}
	return walk(pkg)
}

// declName names a top-level declaration: "Func", "Recv.Method", or "var"/
// "const"/"type" for a GenDecl (a reader inside a package-level initializer
// is still a reader, and still has to be accounted for).
func declName(d ast.Decl) string {
	switch x := d.(type) {
	case *ast.FuncDecl:
		if x.Recv != nil && len(x.Recv.List) > 0 {
			recv := x.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if idx, ok := recv.(*ast.IndexExpr); ok {
				recv = idx.X
			}
			if id, ok := recv.(*ast.Ident); ok {
				return id.Name + "." + x.Name.Name
			}
		}
		return x.Name.Name
	case *ast.GenDecl:
		return x.Tok.String()
	}
	return "?"
}

// TestBodyReaderCensusSeesTheFormsTheParserMissed is the census's control: a
// fixture package with one function per form the old parser could not see, and
// three that must NOT count. A census that went blind to any form fails here,
// by name, rather than by a count drifting in the production table.
func TestBodyReaderCensusSeesTheFormsTheParserMissed(t *testing.T) {
	sites := collectRequestBodyReads(t, filepath.Join("testdata", "bodyreaders"))
	found := map[string]int{}
	for _, s := range sites {
		found[s.fn]++
	}
	for _, fn := range []string{
		// The forms the parser could not see (BUG-2820).
		"StructField", "ContextValue", "TypeAlias", "EmbeddedField", "EmbeddedMethod",
		"ImportAlias", "DotImport", "VarDecl", "CallDerived", "NamedResult",
		"RangeBinding", "MethodValue",
		// The forms the parser's own controls pinned.
		"DirectParam", "ClosureParam", "LocalAlias", "ValueCopy", "ClosureValueParam",
		"ReassignedWithContext", "MixedShortDecl", "InsideSwitch", "AfterIfInitShadow",
	} {
		if found[fn] != 1 {
			t.Errorf("%s: census counted %d body readers, want 1", fn, found[fn])
		}
	}
	for _, fn := range []string{"NotARequest", "HeaderOnly", "ResponseBody",
		"ShadowedByClosureParam", "ReboundInNestedBlock", "CommentOnly"} {
		if found[fn] != 0 {
			t.Errorf("%s is not a request body reader, but the census counted %d", fn, found[fn])
		}
	}
}

func sitesByKey(sites []bodyReadSite) map[string]int {
	out := map[string]int{}
	for _, s := range sites {
		out[s.key()]++
	}
	return out
}

// TestEveryRequestBodyReaderIsAccountedFor is the completeness claim for the
// request-body rule (BUG-2803): a JSON body reaches the store through
// decodeJSON / decodeJSONWithLimit, which refuse a decoded NUL and apply the
// size cap, so every OTHER reader of a request body has to be a deliberate,
// written-down decision.
//
// Since BUG-2820 the readers come from the TYPE CHECKER
// (collectRequestBodyReads), not a parser. The parser matched the spelling
// `http.Request` and names bound to it, so a request reached through a struct
// field, a context value, a type alias or an embedded field was invisible, and
// its accounting was per FILE with a reader count to stop an entry absorbing a
// new reader. Both workarounds are gone: a reader is found wherever its type
// says it is one, and the table below is per CALL SITE, keyed by
// file::function::member, with the number of uses of that member in that
// function. A new reader anywhere, including a second one in an accounted
// function, changes the table and fails here until someone writes it down.
//
// Run against the tree when this was built, the type checker found exactly the
// readers the parser had found, file for file (measured, not assumed): the
// blind spots were real but nothing in the package was using them yet. The
// fixture control, TestBodyReaderCensusSeesTheFormsTheParserMissed, is what
// shows the census would see one.
//
// It asserts BOTH directions: an unaccounted site fails, and an accounted site
// that no longer exists fails, so the table cannot rot into stale excuses.
func TestEveryRequestBodyReaderIsAccountedFor(t *testing.T) {
	// WHY each file reads a body. Every site below must name a file here.
	bodyReaderFileWhy := map[string]string{
		"middleware_request_text.go": "the chokepoint itself: readBodyForDecode reads the body under the caller's cap so bodyDecodesNUL can scan it",
		"handlers_import_bundle.go":  "tar.gz bundle import — streams the body through gzip, and its pad-export.json is checked with bodyDecodesNUL before ImportWorkspace",
		"handlers_attachments.go":    "multipart upload — the body is binary blob content, not text, and must NOT be scanned for text validity",
		"artifact_import.go":         "raw artifact TEXT (not JSON) — checked with bindableText, the same predicate ValidatePath and ValidateQuery apply",
		"handlers_cloud.go":          "bodyHasCloudSecret PEEKS at the body and restores the first 64 KiB of it — a larger body loses its tail, a bound that file documents and accepts; the real decode still happens through decodeJSON downstream",
		"middleware_mcp_audit.go":    "audit capture — parses the body ITSELF and binds the decoded method / params.name to mcp_audit_log.tool_name, so it is a second READER, not a pass-through. That the MCP dispatcher decodes the body again is true and says nothing about what this middleware persists — the earlier rationale here made exactly that mistake and certified it safe (codex round 20). parseMCPRequestBody now runs both caller-derived returns through sanitiseStoredText",
		"handlers_tokens.go":         "guards on r.Body != nil && r.ContentLength != 0, then decodes THROUGH decodeJSON — so the body is read by the chokepoint, which applies the cap and the NUL rule. The earlier reason here said it never reads the body, which was simply false (codex round 29): a wrong reason in this list is the same defect as a missing entry, since both let a reader pass as reviewed",
		"handlers_oauth.go":          "the OAuth handlers read FORM-encoded bodies (r.Form/FormValue). Every POST route they serve is wrapped in ValidateFormBody (BUG-2811), which applies bindableText to the body before these reads. The transport rules cover the query half of r.Form; ValidateFormBody covers the body half",
		"middleware_form_body.go":    "the form-body chokepoint itself: ValidateFormBody reads a form-encoded body up to net/http's own ParseForm cap, checks it with validQueryText, and hands the same bytes (or the same read error) back to the handler (BUG-2811)",
		"handlers_watches.go":        "guards on r.Body != nil && r.ContentLength != 0, then decodes THROUGH decodeJSON — the closing-round-4 fix for the chunked-body drop; the one reader expression is the nil check itself, and the body bytes flow through the chokepoint",
		"handlers_item_lease.go":     "guards on r.Body != nil && r.ContentLength != 0, then decodes THROUGH decodeJSON — same shape as handlers_watches.go; the one reader expression is the nil check, and the body (optional holder/ttl_seconds) flows through the chokepoint, so the caller-text holder gets BUG-2803's NUL refusal before it can reach items.lease_holder",
		"import_read_deadline.go":    "a PASS-THROUGH, not a reader: it replaces r.Body with a wrapper whose Read arms the connection deadline and delegates, byte for byte, to the original body (BUG-3184). Nothing in the file inspects the bytes; the import route's two decoders (decodeJSONWithLimit, and the bundle path's bodyDecodesNUL on pad-export.json) read them afterwards, unchanged",
	}

	// Every call site, counted. MEASURED from the census, not written from
	// expectation.
	bodyReaderSites := map[string]int{
		"artifact_import.go::parseArtifactRequest::Body":                             1,
		"handlers_attachments.go::Server.handleUploadAttachment::Body":               2,
		"handlers_attachments.go::Server.handleUploadAttachment::MultipartForm":      2,
		"handlers_attachments.go::Server.handleUploadAttachment::ParseMultipartForm": 1,
		"handlers_attachments.go::Server.handleUploadAttachment::FormFile":           1,
		"handlers_attachments.go::multipartValues::MultipartForm":                    2,
		"handlers_cloud.go::hasCloudSecretMarker::Body":                              1,
		"handlers_cloud.go::bodyHasCloudSecret::Body":                                2,
		"handlers_import_bundle.go::Server.handleImportWorkspaceBundle::Body":        3,
		"handlers_item_lease.go::Server.resolveLeaseRequest::Body":                   1,
		"middleware_form_body.go::ValidateFormBody::Body":                            6,
		"handlers_oauth.go::Server.handleOAuthAuthorize::ParseForm":                  1,
		"handlers_oauth.go::Server.handleOAuthAuthorizeDecide::ParseForm":            1,
		"handlers_oauth.go::Server.handleOAuthAuthorizeDecide::FormValue":            1,
		"handlers_oauth.go::Server.parseConsentPayload::FormValue":                   4,
		"handlers_oauth.go::Server.parseConsentPayload::PostForm":                    2,
		"handlers_oauth.go::Server.handleOAuthToken::ParseForm":                      1,
		"handlers_oauth.go::Server.handleOAuthRevoke::ParseForm":                     1,
		"handlers_oauth.go::Server.handleOAuthRevoke::PostForm":                      1,
		"handlers_oauth.go::Server.validateConsentCSRFToken::FormValue":              1,
		"handlers_tokens.go::Server.handleRotateUserToken::Body":                     1,
		"handlers_watches.go::Server.handleCreateWatch::Body":                        1,
		"import_read_deadline.go::Server.withImportReadDeadline::Body":               2,
		"middleware_mcp_audit.go::Server.MCPAuditLog::Body":                          5,
		"middleware_mcp_audit.go::Server.emitMCPAuditDenied::Body":                   2,
		"middleware_request_text.go::readBodyForDecode::Body":                        4,
	}

	sites := collectRequestBodyReads(t, ".")
	// The census must have found something: a load that silently resolved no
	// request type would pass the comparisons below with an empty table.
	if len(sites) < 20 {
		t.Fatalf("census found only %d body readers in the package; the load or the type lookup is broken", len(sites))
	}
	got := sitesByKey(sites)
	for key, n := range got {
		want, ok := bodyReaderSites[key]
		switch {
		case !ok:
			t.Errorf("%s reads the request body %d time(s) and is not accounted for. A body carrying JSON must go "+
				"through decodeJSON/decodeJSONWithLimit, which apply BUG-2803's NUL refusal and the size cap. If this "+
				"reader is legitimate, add it to bodyReaderSites (and its file to bodyReaderFileWhy) WITH the reason.", key, n)
		case n != want:
			t.Errorf("%s has %d body reader(s) but was reviewed with %d. A reader was added or removed in that "+
				"function since it was justified: re-read it and update the count.", key, n, want)
		}
		file := strings.SplitN(key, "::", 2)[0]
		if _, ok := bodyReaderFileWhy[file]; !ok {
			t.Errorf("%s has no reason in bodyReaderFileWhy", file)
		}
	}
	for key := range bodyReaderSites {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is accounted for but no longer reads a request body; remove the entry so it cannot "+
				"cover a future reader", key)
		}
	}
	for file := range bodyReaderFileWhy {
		live := false
		for key := range got {
			if strings.HasPrefix(key, file+"::") {
				live = true
			}
		}
		if !live {
			t.Errorf("bodyReaderFileWhy names %s, which no longer reads a request body; remove it", file)
		}
	}
}

// requestHandoff is a call that passes a request to a function OUTSIDE this
// module, which may read the body where no selector here does.
type requestHandoff struct {
	file, fn, callee string
	line             int
}

func (h requestHandoff) key() string { return h.file + "::" + h.fn + "::" + h.callee }

// collectRequestHandoffs returns every call in the package in dir that passes a
// value of type *net/http.Request or net/http.Request to a function or method
// declared outside this module. The body-reader census sees selectors; a
// library that reads the body on our behalf (fosite parsing an OAuth form) has
// no selector here, so it is counted as a HANDOFF and classified by callee.
//
// What it cannot see, stated: a call through an INTERFACE (http.Handler's
// ServeHTTP, which is how the router and every middleware chain dispatch)
// resolves to the interface method, not to whatever implements it. Those are
// classified as dispatch below, not as readers.
func collectRequestHandoffs(t *testing.T, dir string) []requestHandoff {
	t.Helper()
	pkg := loadCensusPackage(t, dir)
	module := pkg.Module
	if module == nil {
		t.Fatalf("%s has no module information", dir)
	}
	reqType := findImportedPackage(pkg, "net/http").Types.Scope().Lookup("Request").Type()

	var out []requestHandoff
	for _, f := range pkg.Syntax {
		file := filepath.Base(pkg.Fset.File(f.Pos()).Name())
		for _, decl := range f.Decls {
			fn := declName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var id *ast.Ident
				switch x := call.Fun.(type) {
				case *ast.SelectorExpr:
					id = x.Sel
				case *ast.Ident:
					id = x
				}
				if id == nil {
					return true
				}
				callee := pkg.TypesInfo.ObjectOf(id)
				if callee == nil || callee.Pkg() == nil {
					return true
				}
				path := callee.Pkg().Path()
				if path == module.Path || strings.HasPrefix(path, module.Path+"/") {
					return true
				}
				for _, a := range call.Args {
					at := pkg.TypesInfo.TypeOf(a)
					if ptr, ok := at.(*types.Pointer); ok {
						at = ptr.Elem()
					}
					if at != nil && types.Identical(at, reqType) {
						out = append(out, requestHandoff{file: file, fn: fn,
							callee: qualifiedCallee(callee), line: pkg.Fset.Position(call.Pos()).Line})
						break
					}
				}
				return true
			})
		}
	}
	return out
}

// TestEveryRequestHandoffIsClassified extends the accounting past this
// package's own selectors (BUG-2820): a request handed to third-party code
// must be classified by what that code does with the body. A new callee fails
// here until someone decides. Body-READING callees are pinned per call site,
// like the readers above; the rest are pinned by callee.
func TestEveryRequestHandoffIsClassified(t *testing.T) {
	notReaders := map[string]string{
		"github.com/go-chi/chi/v5.URLParam":               "reads the route context, not the body",
		"net/http.NotFound":                               "writes a 404; reads nothing of the request body",
		"net/http.Redirect":                               "reads the URL to build the Location header",
		"net/http.ServeContent":                           "reads range and conditional HEADERS",
		"github.com/gorilla/websocket.IsWebSocketUpgrade": "reads headers",
		"github.com/gorilla/websocket.Upgrader.Upgrade":   "hijacks the connection after checking headers; a GET upgrade has no body",
		"net/http.Handler.ServeHTTP":                      "interface DISPATCH: the router and middleware chains passing the request on to the next handler, whose own reads are counted where they are written. Which handler runs is not resolvable statically",
		"github.com/go-chi/chi/v5.Mux.ServeHTTP":          "dispatch into the router, as above",
	}
	// Body readers by delegation: fosite parses the OAuth form body itself.
	// Each POST handler here sits behind ValidateFormBody (BUG-2811), so the
	// body fosite parses has already been checked; counted per call site
	// rather than hidden behind the handler file's own ParseForm calls.
	readers := map[string]int{
		"handlers_oauth.go::Server.handleOAuthAuthorize::github.com/ory/fosite.OAuth2Provider.NewAuthorizeRequest":       1,
		"handlers_oauth.go::Server.handleOAuthAuthorizeDecide::github.com/ory/fosite.OAuth2Provider.NewAuthorizeRequest": 1,
		"handlers_oauth.go::Server.handleOAuthToken::github.com/ory/fosite.OAuth2Provider.NewAccessRequest":              1,
		"handlers_oauth.go::Server.handleOAuthRevoke::github.com/ory/fosite.OAuth2Provider.NewRevocationRequest":         1,
		"handlers_oauth.go::Server.handleOAuthIntrospect::github.com/ory/fosite.OAuth2Provider.NewIntrospectionRequest":  1,
	}

	handoffs := collectRequestHandoffs(t, ".")
	if len(handoffs) < 10 {
		t.Fatalf("found only %d handoffs; the census is broken", len(handoffs))
	}
	got := map[string]int{}
	seenCallee := map[string]bool{}
	for _, h := range handoffs {
		seenCallee[h.callee] = true
		if _, ok := notReaders[h.callee]; ok {
			continue
		}
		got[h.key()]++
	}
	for key, n := range got {
		if want, ok := readers[key]; !ok {
			t.Errorf("%s hands a request to third-party code %d time(s) and is not classified. Decide whether that "+
				"callee reads the body: if not, add the callee to notReaders with the reason; if it does, add this site "+
				"to readers and make sure the body it reads is covered", key, n)
		} else if n != want {
			t.Errorf("%s: %d handoff(s), classified with %d", key, n, want)
		}
	}
	for key := range readers {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is classified as a reader but no longer exists; remove it", key)
		}
	}
	for callee := range notReaders {
		if !seenCallee[callee] {
			t.Errorf("notReaders names %s, which nothing calls with a request any more; remove it", callee)
		}
	}
}

// qualifiedCallee names a function "pkg/path.Name" and a method
// "pkg/path.Recv.Name", so an interface method (http.Handler's ServeHTTP) and a
// concrete one (chi's Mux) are told apart.
func qualifiedCallee(obj types.Object) string {
	name := obj.Pkg().Path() + "."
	if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			rt := sig.Recv().Type()
			if ptr, ok := rt.(*types.Pointer); ok {
				rt = ptr.Elem()
			}
			if named, ok := rt.(*types.Named); ok {
				name += named.Obj().Name() + "."
			}
		}
	}
	return name + obj.Name()
}
