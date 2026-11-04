package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixture = "credential-fixture"

// capture routes the default logger through SetupWriter, the production
// handler, and returns the buffer it writes to.
func capture(t *testing.T, format string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	SetupWriter(&buf, "debug", format)
	return &buf
}

func TestCredentialKeyNamesFromTheCensus(t *testing.T) {
	for _, k := range []string{
		"auth", "Auth", "sig", "sid", "session", "pwd", "pass", "csrf",
		"id_token_hint", "idTokenHint", "password_confirmation", "passwordConfirm", "new_password",
		"token_1", "token_2", "access_token_10", "Session", "x-csrf",
	} {
		if !credentialKey(k) {
			t.Errorf("credentialKey(%q) = false", k)
		}
	}
	// Names that merely contain one of the words stay readable.
	for _, k := range []string{
		"author", "authority", "passenger", "passport", "signal", "signature_count", "session_count",
		"sidecar", "state", "key", "stage1", "sha256", "codec", "http2",
	} {
		if credentialKey(k) {
			t.Errorf("credentialKey(%q) = true", k)
		}
	}
	// A query string names its parameters with no context, so a bare key= and
	// state= are credentials there. As slog attribute names they are ordinary
	// diagnostics (a field key, a limiter key, a workflow state).
	for _, k := range []string{"key", "Key", "state", "api_key"} {
		if !queryCredentialKey(k) {
			t.Errorf("queryCredentialKey(%q) = false", k)
		}
	}
	for _, k := range []string{"keyboard", "statement", "limit"} {
		if queryCredentialKey(k) {
			t.Errorf("queryCredentialKey(%q) = true", k)
		}
	}
}

func TestRedactQueryNamesFromTheCensus(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"key=fixture&limit=20", "key=REDACTED&limit=20"},
		{"auth=fixture&sig=fixture&sid=fixture", "auth=REDACTED&sig=REDACTED&sid=REDACTED"},
		{"session=fixture&pwd=fixture&pass=fixture&csrf=fixture", "session=REDACTED&pwd=REDACTED&pass=REDACTED&csrf=REDACTED"},
		{"state=fixture&code=fixture", "state=REDACTED&code=REDACTED"},
		{"id_token_hint=fixture&password_confirmation=fixture", "id_token_hint=REDACTED&password_confirmation=REDACTED"},
		{"token_1=fixture&token_2=fixture&limit=20", "token_1=REDACTED&token_2=REDACTED&limit=20"},
		{"keyboard=us&statement=on", "keyboard=us&statement=on"},
		// A nested value keeps its structure: the pair inside is what is masked.
		{"return_to=token=abc", "return_to=token%3DREDACTED"},
	} {
		if got := RedactQuery(tc.raw); got != tc.want {
			t.Errorf("RedactQuery(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// A semicolon separates parameters for legacy servers, so it is a separator
// here too. One lone ';' no longer costs the whole query.
func TestRedactQuerySemicolonSeparatesParameters(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"limit=20;token=fixture", "limit=20;token=REDACTED"},
		{"a=1;b=2&c=3", "a=1;b=2&c=3"},
		{"q=x;y&token=fixture", "q=x;y&token=REDACTED"},
		{"limit=20;%74oken=fixture;page=2", "limit=20;%74oken=REDACTED;page=2"},
		{";;token=fixture;;", ";;token=REDACTED;;"},
		{"redirect=/verify-email/tok;limit=20", "redirect=%2Fverify-email%2FREDACTED;limit=20"},
	} {
		got := RedactQuery(tc.raw)
		if got != tc.want || strings.Contains(got, "fixture") {
			t.Errorf("RedactQuery(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestRedactTextShapes(t *testing.T) {
	pat := "pad_" + strings.Repeat("a1", 32)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"
	for _, tc := range []struct{ name, in, want string }{
		{"bearer", "upstream said Bearer abcdef0123456789 is invalid", "upstream said Bearer REDACTED is invalid"},
		{"bearer lowercase", "authorization: bearer abcdef0123456789", "authorization: REDACTED"},
		{"basic", "sent Basic dXNlcjpwYXNz to host", "sent Basic REDACTED to host"},
		{"userinfo", "dial https://user:" + fixture + "@example.com/x failed", "dial https://REDACTED@example.com/x failed"},
		{"userinfo no password", "dial postgres://" + fixture + "@db:5432/pad", "dial postgres://REDACTED@db:5432/pad"},
		{"pat", "token " + pat + " rejected", "token REDACTED rejected"},
		{"session token", "got padsess_" + strings.Repeat("Ab3-", 8) + " twice", "got REDACTED twice"},
		{"jwt", "claims " + jwt + " expired", "claims REDACTED expired"},
		{"stripe", "key sk_live_" + strings.Repeat("a", 24) + " bad", "key REDACTED bad"},
		{"github", "ghp_" + strings.Repeat("A", 36), "REDACTED"},
		{"kv equals", "retry token=" + fixture + " attempt=3", "retry token=REDACTED attempt=3"},
		{"kv and", "a=1&access_token=" + fixture + "&b=2", "a=1&access_token=REDACTED&b=2"},
		{"kv json", `{"refresh_token":"` + fixture + `","user":"x"}`, `{"refresh_token":"REDACTED","user":"x"}`},
		{"kv json spaced", `{"password": "` + fixture + ` with spaces", "ok": true}`, `{"password": "REDACTED", "ok": true}`},
		{"kv single quote", "secret='" + fixture + "'", "secret='REDACTED'"},
		{"header line", "Authorization: Basic " + fixture + "\nHost: x", "Authorization: REDACTED\nHost: x"},
		{"cookie header", "Cookie: pad_session=" + fixture + "; theme=dark", "Cookie: REDACTED"},
		{"set-cookie", "Set-Cookie: pad_session=" + fixture + "; Path=/", "Set-Cookie: REDACTED"},
		{"credential path", "GET /reset-password/" + fixture + " 404", "GET /reset-password/REDACTED 404"},
		{"credential path query", "next /verify-email/" + fixture + "?x=1 done", "next /verify-email/REDACTED?x=1 done"},
		{"strconv input", `strconv.ParseInt: parsing "` + fixture + `": invalid syntax`, `strconv.ParseInt: parsing "REDACTED": invalid syntax`},
		{"time input", `parsing time "` + fixture + `" as "2006-01-02": cannot parse`, `parsing time "REDACTED" as "2006-01-02": cannot parse`},
		{"url query in text", "GET https://example.com/cb?code=" + fixture + "&ok=1 failed", "GET https://example.com/cb?code=REDACTED&ok=1 failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactText(tc.in)
			if got != tc.want {
				t.Errorf("RedactText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// What the scrubber must leave alone: it runs on every string a service logs.
func TestRedactTextKeepsDiagnostics(t *testing.T) {
	for _, in := range []string{
		"",
		"plain message",
		"invalid token: expired",
		"oauth2: cannot fetch token: 400 Bad Request",
		"workspace 3f2b1c9e-8a4d-4f6e-9b1a-0c2d3e4f5a6b not found",
		"commit 5c86b005a9fd3b1f0e2c4d6a8b7c9d0e1f2a3b4c merged",
		"status=500 elapsed=12ms retries=3",
		"open /var/lib/pad/s/pad.db: permission denied",
		"user:0dbe0458 ip:10.0.0.7 limiter=api",
		"dial tcp 10.0.0.7:5432: connect: connection refused",
		"key not found in cache",
		"session expired for user 12",
		"GET /api/v1/workspaces/demo/items 200",
		"parsing flag --port: expected a number",
	} {
		if got := RedactText(in); got != in {
			t.Errorf("RedactText changed %q to %q", in, got)
		}
	}
	// Oversize text cannot be examined cheaply, so it is withheld whole.
	if got := RedactText(strings.Repeat("a", maxScrubBytes+1)); strings.Contains(got, "aaaa") {
		t.Errorf("oversize text was passed through: %.40q", got)
	}
	// A cheap input stays cheap: a long run of near-matches is linear.
	done := make(chan struct{})
	go func() {
		RedactText(strings.Repeat("token=", 20000))
		RedactText(strings.Repeat("Bearer ", 20000))
		RedactText(strings.Repeat("a://", 20000))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RedactText did not return in 5s on a hostile input")
	}
}

type apiErr struct{ body string }

func (e *apiErr) Error() string { return "provider returned 401: " + e.body }

func TestRedactAttrScrubsErrorAndStringText(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			buf := capture(t, format)
			_, numErr := strconv.Atoi(fixture)
			_, timeErr := time.Parse("2006-01-02", fixture)
			slog.Info("provider failure", "error", &apiErr{body: `{"error":"bad","api_key":"` + fixture + `","hint":"Bearer ` + fixture + `-bearer"}`})
			slog.Info("wrapped", "error", fmt.Errorf("decode: %w", numErr))
			slog.Info("time", "error", timeErr)
			slog.Info("joined", "error", errors.Join(errors.New("first"), &apiErr{body: "secret=" + fixture}))
			slog.Info("body text", "body", `{"token":"`+fixture+`"}`)
			slog.Info("msg carries password="+fixture, "k", "v")
			slog.Info("plain string", "detail", "Authorization: Bearer "+fixture)
			// The standard log package is routed through the same handler.
			stdlog := slog.NewLogLogger(slog.Default().Handler(), slog.LevelInfo)
			stdlog.Printf("dial https://u:%s@host failed", fixture)
			out := buf.String()
			if strings.Contains(out, fixture) {
				t.Errorf("a credential reached the log:\n%s", out)
			}
			for _, keep := range []string{"provider failure", "provider returned 401", "first", "invalid syntax", "cannot parse"} {
				if !strings.Contains(out, keep) {
					t.Errorf("lost diagnostic %q:\n%s", keep, out)
				}
			}
		})
	}
}

func TestRedactAttrErrorKeepsURLErrorBehaviour(t *testing.T) {
	buf := capture(t, "json")
	slog.Info("x", "error", fmt.Errorf("fetch: %w", &url.Error{Op: "Get", URL: "https://u:" + fixture + "@h/" + fixture, Err: errors.New(fixture)}))
	// The attribute carries a type name and none of the error's text.
	if strings.Contains(buf.String(), fixture) || !strings.Contains(buf.String(), `"error":"*fmt.wrapError"`) {
		t.Errorf("a wrapped url.Error is not reduced to its type: %s", buf.String())
	}
}

type nestedCreds struct {
	Name     string `json:"name"`
	APIKey   string `json:"api_key"`
	Inner    nestedInner
	hidden   string
	Password string
	Tags     []string
	Ptr      *nestedInner
}

type nestedInner struct {
	Label string
	Token string `json:"access_token"`
}

func TestRedactAttrWalksNestedValues(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			buf := capture(t, format)
			slog.Info("map", "params", map[string]string{"token": fixture, "page": "2"})
			slog.Info("map any", "params", map[string]any{"user": map[string]any{"password": fixture}, "n": 1})
			slog.Info("struct", "req", nestedCreds{Name: "n", APIKey: fixture, Inner: nestedInner{Label: "l", Token: fixture}, hidden: fixture, Password: fixture, Tags: []string{"a", "Bearer " + fixture + "xx"}, Ptr: &nestedInner{Token: fixture}})
			slog.Info("ptr", "req", &nestedCreds{APIKey: fixture})
			slog.Info("slice", "rows", []map[string]string{{"secret": fixture}, {"ok": "1"}})
			slog.Info("header", "req", http.Header{"Cookie": {fixture}, "Accept": {"*/*"}})
			slog.Info("values", "q", url.Values{"code": {fixture}, "page": {"2"}})
			slog.Info("bytes", "raw", []byte(`{"password":"`+fixture+`"}`))
			slog.Info("group", slog.Group("outer", slog.Any("params", map[string]string{"api_key": fixture})))
			out := buf.String()
			if strings.Contains(out, fixture) {
				t.Errorf("a nested credential reached the log:\n%s", out)
			}
			for _, keep := range []string{"page", "Accept", "ok"} {
				if !strings.Contains(out, keep) {
					t.Errorf("lost diagnostic %q:\n%s", keep, out)
				}
			}
		})
	}
}

func TestRedactAttrNestedWalkIsBounded(t *testing.T) {
	var deep any = fixture
	for i := 0; i < 200; i++ {
		deep = map[string]any{"k": deep}
	}
	wide := make(map[string]string, 5000)
	for i := 0; i < 5000; i++ {
		wide[strconv.Itoa(i)] = "token=" + fixture
	}
	cyc := map[string]any{}
	cyc["self"] = cyc
	done := make(chan string, 1)
	go func() {
		buf := capture(t, "json")
		slog.Info("deep", "x", deep)
		slog.Info("wide", "x", wide)
		slog.Info("cycle", "x", cyc)
		done <- buf.String()
	}()
	select {
	case out := <-done:
		if strings.Contains(out, fixture) {
			t.Errorf("a credential survived a bounded walk: %.200s", out)
		}
		if !strings.Contains(out, "token=REDACTED") {
			t.Errorf("the wide map's entries were not walked and scrubbed: %.200s", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the nested walk did not return in 10s")
	}
}

// What the backstop must not touch: levels, sources, times, counts, plain
// values and values with their own text form.
func TestRedactAttrLeavesOrdinaryValuesAlone(t *testing.T) {
	buf := capture(t, "json")
	id := "3f2b1c9e-8a4d-4f6e-9b1a-0c2d3e4f5a6b"
	slog.Info("ordinary", "workspace_id", id, "count", 3, "ok", true, "elapsed", 1500*time.Millisecond,
		"at", time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC), "path", "/api/v1/workspaces/demo/items",
		"error", errors.New("open /var/lib/pad/pad.db: permission denied"), "tags", []string{"a", "b"})
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not json: %v\n%s", err, buf.String())
	}
	if rec["level"] != "INFO" || rec["msg"] != "ordinary" || rec["workspace_id"] != id || rec["count"] != float64(3) ||
		rec["ok"] != true || rec["path"] != "/api/v1/workspaces/demo/items" || rec["elapsed"] != float64(1500*time.Millisecond) {
		t.Errorf("an ordinary field changed: %s", buf.String())
	}
	if rec["error"] != "open /var/lib/pad/pad.db: permission denied" {
		t.Errorf("error text changed: %v", rec["error"])
	}
	if tags, _ := rec["tags"].([]any); len(tags) != 2 {
		t.Errorf("a plain slice changed: %v", rec["tags"])
	}
	// AddSource output keeps its shape.
	var sb bytes.Buffer
	h := slog.New(slog.NewJSONHandler(&sb, &slog.HandlerOptions{AddSource: true, ReplaceAttr: redactAttr}))
	h.Info("src")
	var withSource map[string]any
	if err := json.Unmarshal(sb.Bytes(), &withSource); err != nil {
		t.Fatal(err)
	}
	if src, _ := withSource["source"].(map[string]any); src == nil || src["function"] == nil {
		t.Errorf("the source attribute lost its shape: %s", sb.String())
	}
}

// Every log call that names a request path or query must pass it through the
// redactor in the call itself, so a handler that is not Setup's (a test, a
// custom one) is safe too, and so a revert of any one site turns this red. A
// FILESYSTEM path logged under the key "path" would be truncated by the
// URL-path redactor wherever a directory is called s or join, so those sites
// use a different key.
func TestLogPathAndQueryAttrsAreRedactedAtTheCall(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var sites, bad []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		s, b := pathAttrSites(fset, f, rel)
		sites, bad = append(sites, s...), append(bad, b...)
		return nil
	})
	if len(sites) < 15 {
		t.Fatalf("the guard found only %d path/query log sites; its walk is broken: %v", len(sites), sites)
	}
	if len(bad) > 0 {
		t.Errorf("log calls that name a path or query without RedactPath/RedactQuery in the call (a filesystem path belongs under another key, e.g. \"file\"):\n  %s", strings.Join(bad, "\n  "))
	}
}

// The guard must be able to go red: these are the shapes it has to catch, and
// the one shape it has to let through.
func TestPathAttrGuardSeesTheShapesItGuards(t *testing.T) {
	const src = `package p
import "log/slog"
func f(l *slog.Logger, lg any, p, q string) {
	slog.Info("a", "path", p)
	l.Warn("b", "k", 1, "query", q)
	lg.(interface{ Error(string, ...any) }).Error("c", "path", p)
	l = l.With("path", p)
	slog.InfoContext(nil, "d", "reset_path", p)
	_ = slog.Attr{Key: "path", Value: slog.StringValue(p)}
	_ = slog.String("query", q)
	slog.Info("fine", "path", logging.RedactPath(p), "query", logging.RedactQuery(q), "path", "/fixed")
	_ = slog.Attr{Key: "path", Value: slog.StringValue(logging.RedactPath(p))}
	_ = slog.Group("g", "path", logging.RedactPath(p))
	slog.Info("mixed", slog.String("a", "b"), "path", p)
	slog.Info("wrapped", "path", fmt.Sprintf("%s?%s", logging.RedactPath(p), q))
	l.Log(nil, slog.LevelInfo, "log", "path", p)
	slog.Info("mixed fine", slog.String("a", "b"), "path", logging.RedactPath(p))
	l.Log(nil, slog.LevelInfo, "log fine", "path", logging.RedactPath(p))
	l.Log(nil, slog.Level(2), "m", "path", p)
	slog.Info("other package", "path", other.RedactPath(p))
	slog.Info("logging package", "path", logging.RedactPath(p))
	slog.Info("unqualified", "path", RedactPath(p))
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, bad := pathAttrSites(fset, f, "p.go")
	// Inside package logging an unqualified call is the helper itself.
	inPkg, perr := parser.ParseFile(fset, "l.go", "package logging\nimport \"log/slog\"\nfunc f(p string) { slog.Info(\"x\", \"path\", RedactPath(p)) }\n", 0)
	if perr != nil {
		t.Fatal(perr)
	}
	if _, inBad := pathAttrSites(fset, inPkg, "l.go"); len(inBad) != 0 {
		t.Errorf("an unqualified RedactPath inside package logging was reported: %v", inBad)
	}
	want := []int{4, 5, 6, 7, 8, 9, 10, 14, 15, 16, 19, 20, 22}
	var got []int
	for _, b := range bad {
		parts := strings.SplitN(b, ":", 2)
		n, _ := strconv.Atoi(strings.Fields(parts[1])[0])
		got = append(got, n)
	}
	sort.Ints(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("unredacted sites found at lines %v, want %v (%v)", got, want, bad)
	}
}

// pathAttrSites lists the "path", "reset_path" and "query" log attributes in f
// (sites) and those whose value is not a RedactPath/RedactQuery call or a
// string literal (bad), as file:line (key).
func pathAttrSites(fset *token.FileSet, f *ast.File, rel string) (sites, bad []string) {
	ast.Inspect(f, func(n ast.Node) bool {
		report := func(pos token.Pos, key string, val ast.Expr) {
			want := "RedactPath"
			if strings.EqualFold(key, "query") {
				want = "RedactQuery"
			}
			where := fmt.Sprintf("%s:%d (%s)", rel, fset.Position(pos).Line, key)
			sites = append(sites, where)
			if directRedact(val, want, f.Name.Name == "logging") {
				return
			}
			if lit, ok := val.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				return
			}
			bad = append(bad, where)
		}
		watched := func(e ast.Expr) (string, bool) {
			lit, ok := e.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return "", false
			}
			key, _ := strconv.Unquote(lit.Value)
			switch strings.ToLower(key) {
			case "path", "reset_path", "query":
				return key, true
			}
			return "", false
		}
		switch x := n.(type) {
		case *ast.CompositeLit:
			// slog.Attr{Key: "path", Value: ...}
			if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Attr" {
				var key string
				var val ast.Expr
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							switch id.Name {
							case "Key":
								key, _ = watched(kv.Value)
							case "Value":
								val = kv.Value
							}
						}
					}
				}
				if key != "" && val != nil {
					report(x.Pos(), key, val)
				}
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// slog.String("path", v), slog.Any(...) built anywhere.
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "slog" && (sel.Sel.Name == "String" || sel.Sel.Name == "Any") && len(x.Args) == 2 {
				if key, ok := watched(x.Args[0]); ok {
					report(x.Pos(), key, x.Args[1])
				}
				return true
			}
			// Alternating key, value arguments of a logger call, whatever the
			// receiver is called: after the message (after the context for the
			// *Context forms; from the start for With and slog.Group).
			first := 1
			switch sel.Sel.Name {
			case "Debug", "Info", "Warn", "Error":
			case "DebugContext", "InfoContext", "WarnContext", "ErrorContext":
				first = 2
			case "Log":
				// Log(ctx, level, msg, attrs...): starting at 1 reads (level, msg) as a
				// pair and lands on the same key/value parity as starting at 3.
			case "With":
				first = 0
			case "Group":
				first = 1
			default:
				return true
			}
			for i := first; i < len(x.Args); {
				if isAttrArg(x.Args[i]) { // a slog.Attr stands alone and shifts the pairs after it
					i++
					continue
				}
				if i+1 >= len(x.Args) {
					break
				}
				if key, ok := watched(x.Args[i]); ok {
					report(x.Pos(), key, x.Args[i+1])
				}
				i += 2
			}
		}
		return true
	})
	return sites, bad
}

// isAttrArg reports whether e is a slog.Attr: a slog.String(...) style call or
// a slog.Attr literal.
func isAttrArg(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "slog" {
				switch sel.Sel.Name {
				case "String", "Int", "Int64", "Uint64", "Float64", "Bool", "Duration", "Time", "Any", "Group", "GroupAttrs":
					return true
				}
			}
		}
	case *ast.CompositeLit:
		if sel, ok := x.Type.(*ast.SelectorExpr); ok {
			return sel.Sel.Name == "Attr"
		}
	}
	return false
}

// directRedact reports whether e IS a call to name (RedactPath or
// RedactQuery), possibly inside slog.StringValue or slog.AnyValue: not a call
// to it somewhere in a larger expression, which could still carry the raw text.
func directRedact(e ast.Expr, name string, inLogging bool) bool {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
			continue
		case *ast.CallExpr:
			var fn string
			switch f := x.Fun.(type) {
			case *ast.Ident: // the helper itself, only inside package logging
				if inLogging {
					fn = f.Name
				}
			case *ast.SelectorExpr:
				// only logging.RedactPath; another package's function of that name proves nothing
				if id, ok := f.X.(*ast.Ident); ok && id.Name == "logging" {
					fn = f.Sel.Name
				} else if f.Sel.Name == "StringValue" || f.Sel.Name == "AnyValue" {
					fn = f.Sel.Name
				}
			}
			if fn == name {
				return true
			}
			if (fn == "StringValue" || fn == "AnyValue") && len(x.Args) == 1 {
				e = x.Args[0]
				continue
			}
		}
		return false
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func TestRedactTextStructuredAndQueryValues(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"token":{"value":"SECRET","n":1},"ok":true}`, `{"token":REDACTED,"ok":true}`},
		{`{"tokens":["abc","def"],"ok":true}`, `{"tokens":REDACTED,"ok":true}`},
		{`{"secret": [ "a]b", {"c":"d"} ] }`, `{"secret": REDACTED }`},
		{`{"token":{"never":"closes"`, `{"token":REDACTED`},
		{"GET https://x.example/cb?key=AIzaFixture&state=abc&ok=1 failed", "GET https://x.example/cb?key=REDACTED&state=REDACTED&ok=1 failed"},
		{"worker state=running key=cache-1", "worker state=running key=cache-1"},
		// A semicolon separates parameters in a URL query, as in RedactQuery.
		{"GET /cb?limit=1;state=abc;page=2 done", "GET /cb?limit=1;state=REDACTED;page=2 done"},
		{"GET /cb?limit=1&page=2;key=AIzaFixture done", "GET /cb?limit=1&page=2;key=REDACTED done"},
		{"queue a;state=running;key=cache-1", "queue a;state=running;key=cache-1"},
		{"see [/join/abc] and {/reset-password/abc}", "see [/join/REDACTED] and {/reset-password/REDACTED}"},
	} {
		if got := RedactText(tc.in); got != tc.want {
			t.Errorf("RedactText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

type stringerCfg struct {
	Name     string
	Password string
}

func (stringerCfg) String() string { return "cfg" }

type secretish string

type withTimes struct {
	At   time.Time
	Addr netip.Addr
	Note string
}

type nilUnwrapErr struct{ inner error }

func (e *nilUnwrapErr) Error() string { return e.inner.Error() }
func (e *nilUnwrapErr) Unwrap() error { return e.inner }

func TestRedactAttrMarshalersAndNamedStrings(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			buf := capture(t, format)
			slog.Info("stringer", "cfg", stringerCfg{Name: "n", Password: fixture})
			slog.Info("raw", "body", json.RawMessage(`{"password":"`+fixture+`","ok":1}`))
			slog.Info("named", "v", secretish("token="+fixture))
			slog.Info("times", "w", withTimes{At: time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC), Addr: netip.MustParseAddr("10.0.0.7"), Note: "kept"})
			out := buf.String()
			if strings.Contains(out, fixture) {
				t.Errorf("a credential reached the log:\n%s", out)
			}
			for _, keep := range []string{"2026-11-04", "10.0.0.7", "kept"} {
				if !strings.Contains(out, keep) {
					t.Errorf("lost diagnostic %q:\n%s", keep, out)
				}
			}
		})
	}
}

func TestRedactAttrKeepsCountsUnderCredentialNames(t *testing.T) {
	buf := capture(t, "json")
	slog.Info("counts", "usage", map[string]int{"input_tokens": 5, "code": 404},
		"row", struct {
			Tokens int
			Code   int
			Token  string
		}{Tokens: 7, Code: 503, Token: fixture})
	out := buf.String()
	for _, keep := range []string{`"input_tokens":5`, `"code":404`, `"Tokens":7`, `"Code":503`} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost count %s:\n%s", keep, out)
		}
	}
	if strings.Contains(out, fixture) {
		t.Errorf("a credential reached the log:\n%s", out)
	}
}

func TestRedactAttrWalkCostIsBounded(t *testing.T) {
	fan := map[string]any{}
	for i := 0; i < 256; i++ {
		fan[strconv.Itoa(i)] = fan
	}
	huge := make(map[int]string, 300000)
	for i := 0; i < 300000; i++ {
		huge[i] = "v"
	}
	done := make(chan struct{})
	go func() {
		buf := capture(t, "json")
		slog.Info("fan", "x", fan)
		slog.Info("huge", "x", huge)
		if buf.Len() > 1<<20 {
			t.Errorf("a bounded walk produced %d bytes", buf.Len())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the walk of a fanned-out or huge value did not return in 10s")
	}
}

func TestRedactAttrNestedErrorsAndOddValues(t *testing.T) {
	buf := capture(t, "json")
	slog.Info("nested", "x", map[string]any{
		"e":    &url.Error{Op: "Post", URL: "https://hooks.example.com/hooks/" + fixture, Err: errors.New("boom")},
		"hash": []byte{0xff, 0xfe, 0x01},
	})
	var nilErr *nilUnwrapErr
	slog.Info("typed nil", "error", error(nilErr))
	out := buf.String()
	if strings.Contains(out, fixture) {
		t.Errorf("a credential reached the log:\n%s", out)
	}
	if !strings.Contains(out, "<3 bytes>") {
		t.Errorf("binary bytes are not summarised:\n%s", out)
	}
	if !strings.Contains(out, "typed nil") {
		t.Errorf("a typed-nil error took the log call down:\n%s", out)
	}
}

func TestRedactAttrURLValuesDoNotKeepUserinfoOrQuery(t *testing.T) {
	buf := capture(t, "json")
	u, err := url.Parse("https://user:" + fixture + "@example.com/cb?token=" + fixture + "&page=2")
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("url value", "target", u, "again", *u)
	out := buf.String()
	if strings.Contains(out, fixture) {
		t.Errorf("a credential in a url.URL reached the log:\n%s", out)
	}
	if !strings.Contains(out, "example.com") {
		t.Errorf("lost the host:\n%s", out)
	}
}

var keyRenders atomic.Int64

// countedKey counts how often the walk renders it, which is how many map
// entries the walk read.
type countedKey int

func (k countedKey) String() string {
	keyRenders.Add(1)
	return strconv.Itoa(int(k))
}

func TestRedactAttrMapReadIsBoundedBeforeSorting(t *testing.T) {
	m := make(map[countedKey]string, 50000)
	for i := 0; i < 50000; i++ {
		m[countedKey(i)] = "v"
	}
	keyRenders.Store(0)
	buf := capture(t, "json")
	slog.Info("big", "x", m)
	if got := keyRenders.Load(); got > maxMapRead {
		t.Errorf("the walk rendered %d of 50000 map keys; it must read at most %d", got, maxMapRead)
	}
	if !strings.Contains(buf.String(), "more") {
		t.Errorf("a truncated map does not say so: %.200s", buf.String())
	}
}

func TestRedactTextDecodesEncodedKeys(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"GET https://host/cb?%74oken=secret ok", "GET https://host/cb?%74oken=REDACTED ok"},
		{"retry %61ccess_token=abc attempt=3", "retry %61ccess_token=REDACTED attempt=3"},
		{"next ?%2574oken=secret done", "next ?%2574oken=REDACTED done"},
		{"retry %70assword=x%20y more", "retry %70assword=REDACTED more"},
		// A valid escape followed by a malformed one cannot be read: masked, as RedactQuery does.
		{"retry %74oken%zz=abc more", "retry %74oken%zz=REDACTED more"},
		// A percent sign in ordinary text is not an encoded credential name.
		{"disk 100%=full limit=20", "disk 100%=full limit=20"},
		{"bad %zz=abc and %=def", "bad %zz=abc and %=def"},
		{"width%20x=3 height=4", "width%20x=3 height=4"},
	} {
		if got := RedactText(tc.in); got != tc.want {
			t.Errorf("RedactText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

type panicKey string

func (panicKey) String() string { panic("a key whose String method panics") }

// fmt itself reports a key whose String method panics, so the walk needs no
// guard of its own; this pins that the log call and its record survive.
func TestRedactAttrSurvivesAMapKeyThatPanics(t *testing.T) {
	buf := capture(t, "json")
	slog.Info("odd keys", "m", map[panicKey]string{"a": "b"}, "ok", "kept")
	out := buf.String()
	if !strings.Contains(out, `"ok":"kept"`) || !strings.Contains(out, "odd keys") {
		t.Errorf("a panicking map key took the log call down or lost the record:\n%s", out)
	}
}

// RedactText runs on every string a service logs, some of it chosen by a
// client (a Host header, the input a parse error repeats), inside a
// synchronous log call. Each shape below was or could be quadratic; at the
// 256 KB cap a linear scan takes milliseconds and a quadratic one seconds.
func TestRedactTextCostIsLinearOnHostileShapes(t *testing.T) {
	const size = maxScrubBytes - 1024
	rep := func(unit string) string { return strings.Repeat(unit, size/len(unit)) }
	shapes := map[string]string{
		"semicolon pairs":            rep("a;a=;"),
		"semicolon pairs in a query": "?" + rep("a;a=;"),
		"ampersand pairs in a query": "?" + rep("a&a=&"),
		"nested query values":        "?" + rep("a=?a=?"),
		"query marks":                rep("?a"),
		"empty queries":              rep("? "),
		"credential keys":            rep("token="),
		"credential pairs spaced":    rep("token = "),
		"quoted keys":                rep(`"password":`),
		"bearer words":               rep("Bearer "),
		"schemes":                    rep("a://"),
		"encoded keys":               rep("%74oken="),
		"double encoded keys":        "?" + rep("%2574oken="),
		"unterminated brackets":      `{"token":` + rep("[{"),
		"long key":                   rep("a") + "=x",
		"parse errors":               rep(`parsing "`),
		"credential paths":           rep(" /join/x"),
	}
	for name, in := range shapes {
		start := time.Now()
		RedactText(in)
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("RedactText on %q (%d bytes) took %v; the scan must stay linear", name, len(in), d)
		}
	}
}

// Hunter findings (r2-r4): shapes the text scanner and the walk missed.
func TestRedactTextEscapedAndGoPrintedShapes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"escaped json", `provider said "{\"api_key\":\"abc\"}"`, `provider said "{\"api_key\":\"REDACTED\"}"`},
		{"escaped json, more pairs", `{\"token\":\"abc\",\"ok\":1}`, `{\"token\":\"REDACTED\",\"ok\":1}`},
		{"escaped json, spaced value", `{\"password\": \"x y z\", \"n\": 2}`, `{\"password\": \"REDACTED\", \"n\": 2}`},
		{"escaped unterminated", `{\"token\":\"never closes`, `{\"token\":\"REDACTED`},
		{"json-escaped ampersand", `GET /cb?a=1\u0026sig=abc ok`, `GET /cb?a=1&sig=REDACTED ok`},
		{"go map", `map[token:x ok:1]`, `map[token:REDACTED ok:1]`},
		{"go struct", `{Password:hunter2 Name:n}`, `{Password:REDACTED Name:n}`},
		{"go url.Values", `map[code:[abc def] page:[2]]`, `map[code:REDACTED page:[2]]`},
		// A colon followed by a space is prose, and other colons are not pairs.
		{"prose colon", "invalid token: expired", "invalid token: expired"},
		{"address", "dial tcp 10.0.0.7:5432: refused; user:0dbe0458 ok", "dial tcp 10.0.0.7:5432: refused; user:0dbe0458 ok"},
		{"scheme", "fetch http://example.com:8080/x", "fetch http://example.com:8080/x"},
		{"dsn with @ in the password", "dial postgres://u:p@ss@host:5432/db failed", "dial postgres://REDACTED@host:5432/db failed"},
		{"x- header dump", "X-Auth-Token: abc\nX-CSRF-Token: def\nAccept: */*", "X-Auth-Token: REDACTED\nX-CSRF-Token: REDACTED\nAccept: */*"},
		{"counts of tokens stay", "max_tokens=4096 input_tokens=5 token=482913 tokens=abc", "max_tokens=4096 input_tokens=5 token=REDACTED tokens=REDACTED"},
		{"code-ending words stay", "decode=utf8 encode=on unicode=yes zipcode=90210", "decode=utf8 encode=on unicode=yes zipcode=90210"},
		{"parse input only for parse errors", `open config: parsing "config.yaml": no such file`, `open config: parsing "config.yaml": no such file`},
		{"strconv input", `strconv.Atoi: parsing "abc": invalid syntax`, `strconv.Atoi: parsing "REDACTED": invalid syntax`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactText(tc.in); got != tc.want {
				t.Errorf("RedactText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCredentialKeyNamesFromTheHunters(t *testing.T) {
	for _, k := range []string{
		"encryption_key", "signing_key", "hmac_key", "master_key", "session_key", "passphrase", "pin", "token_value",
		"private_key_pem", "pad_session", "__Host-pad_session", "connect.sid", "token.value", "x-auth",
	} {
		if !credentialKey(k) {
			t.Errorf("credentialKey(%q) = false", k)
		}
	}
	for _, k := range []string{
		"cache_key", "storage_key", "sort_key", "primary_key", "idempotency_key", "field_key", "redis_key",
		"decode", "encode", "unicode", "zipcode", "session_count", "sidecar", "inside", "outside", "spin", "pinned",
	} {
		if credentialKey(k) {
			t.Errorf("credentialKey(%q) = true", k)
		}
	}
}

type maskedKey string

func (maskedKey) String() string { return "***" }

type wallClock struct{ Hour, Min int }

func (w wallClock) String() string { return fmt.Sprintf("%02d:%02d", w.Hour, w.Min) }

func TestRedactAttrHonoursSelfMaskingStringers(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			buf := capture(t, format)
			slog.Info("stringers", "value", maskedKey("credential-fixture"), "at", wallClock{7, 30},
				"nested", map[string]any{"k": maskedKey("credential-fixture")})
			hook, err := url.Parse("https://hooks.example.com/services/T0/B0/" + fixture)
			if err != nil {
				t.Fatal(err)
			}
			slog.Info("url value", "target", hook, "again", *hook)
			slog.Info("cookie", "c", &http.Cookie{Name: "pad_session", Value: fixture}, "cs", []*http.Cookie{{Name: "x", Value: fixture}})
			out := buf.String()
			if strings.Contains(out, fixture) {
				t.Errorf("a credential reached the log:\n%s", out)
			}
			for _, keep := range []string{"***", "07:30", "hooks.example.com"} {
				if !strings.Contains(out, keep) {
					t.Errorf("lost %q:\n%s", keep, out)
				}
			}
		})
	}
}

func TestRedactAttrSourceNamedAttrIsStillWalked(t *testing.T) {
	buf := capture(t, "json")
	slog.Info("x", "source", struct{ Token string }{Token: fixture})
	if strings.Contains(buf.String(), fixture) {
		t.Errorf("an attribute merely named source skipped the walk:\n%s", buf.String())
	}
}

func TestRedactAttrWalkSpendsABoundedNumberOfBytes(t *testing.T) {
	big := strings.Repeat("a", 60<<10)
	rows := make([]string, 200)
	for i := range rows {
		rows[i] = big
	}
	done := make(chan string, 1)
	go func() {
		buf := capture(t, "json")
		slog.Info("many big strings", "rows", rows)
		done <- buf.String()
	}()
	select {
	case out := <-done:
		if len(out) > 4<<20 {
			t.Errorf("200 strings of 60 KiB produced %d bytes; the walk's byte budget did not apply", len(out))
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("strings past the budget were not withheld:\n%.200s", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the walk of many large strings did not return in 10s")
	}
}

// Round-5 hunter findings: diagnostics the text scrubber destroyed.
func TestRedactTextKeepsCodesAndQueryMarksThatAreNotCredentials(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"json status code", `{"code":404,"message":"x"}`, `{"code":404,"message":"x"}`},
		{"exit code", "exit code=1 after 3 tries", "exit code=1 after 3 tries"},
		{"grpc code", "rpc error: code = NotFound desc = no such item", "rpc error: code = NotFound desc = no such item"},
		{"go printed code", "{Code:404 Msg:x}", "{Code:404 Msg:x}"},
		{"one-time numeric code", "sent code=482913 to the user", "sent code=REDACTED to the user"},
		{"four digit code", "pin code=4829 entered", "pin code=REDACTED entered"},
		{"opaque code", "exchange code=abc123XYZ789 failed", "exchange code=REDACTED failed"},
		{"lowercase word code", "code=expired and more", "code=REDACTED and more"},
		{"sql placeholders", "exec INSERT INTO t (a,b,c,d,e,f) VALUES (?,?,?,?,?,?)", "exec INSERT INTO t (a,b,c,d,e,f) VALUES (?,?,?,?,?,?)"},
		{"numbered placeholders", "VALUES (?1,?2,?3,?4,?5,?6,?7,?8)", "VALUES (?1,?2,?3,?4,?5,?6,?7,?8)"},
		{"equals placeholder", "WHERE a=? AND b=? AND c=?", "WHERE a=? AND b=? AND c=?"},
		{"question in prose", "ok?100%done and what?", "ok?100%done and what?"},
		{"query still masked", "GET /cb?token=x&ok=1 done", "GET /cb?token=REDACTED&ok=1 done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactText(tc.in); got != tc.want {
				t.Errorf("RedactText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
	if got := RedactText("see /x?/verify-email/tok123 done"); strings.Contains(got, "tok123") {
		t.Errorf("a credential path as a bare query part leaked: %q", got)
	}
}

func TestRedactTextMasksMoreHeaderNames(t *testing.T) {
	in := "PRIVATE-TOKEN: glpat-abc\nApi-Key: k1\nX-Api-Key: k2\nAccept: */*"
	want := "PRIVATE-TOKEN: REDACTED\nApi-Key: REDACTED\nX-Api-Key: REDACTED\nAccept: */*"
	if got := RedactText(in); got != want {
		t.Errorf("RedactText(%q)\n got %q\nwant %q", in, got, want)
	}
}

// Round-6 hunter findings.
func TestRedactTextStillReadsEncodedAndBarePathQueries(t *testing.T) {
	for _, in := range []string{
		"GET /x?%2Fverify-email%2FTOK123 done",
		"GET /x?next/verify-email/TOK123 done",
		"GET /x?token%3DSECRET123 done",
		"GET /x?%74oken%3dSECRET123&ok=1 done",
	} {
		if got := RedactText(in); strings.Contains(got, "TOK123") || strings.Contains(got, "SECRET123") {
			t.Errorf("RedactText(%q) leaked: %q", in, got)
		}
	}
}

func TestRedactTextKeepsStatusCodesAndPunctuation(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"negative number", "closed with code=-1", "closed with code=-1"},
		{"in parentheses", "failed (code=404) twice", "failed (code=404) twice"},
		{"sentence end", "failed with code=404.", "failed with code=404."},
		{"word at sentence end", "failed with code=NotFound.", "failed with code=NotFound."},
		// One-time codes still go.
		{"four digits", "code=1006 given", "code=REDACTED given"},
		{"lowercase word", "code=expired given", "code=REDACTED given"},
		{"alnum", "code=ab12cd34ef given", "code=REDACTED given"},
		{"snake case", "code=ab_cd given", "code=REDACTED given"},
		{"api error code", "code=not_found given", "code=REDACTED given"},
		{"snake with digits", "code=ab_12_cd given", "code=REDACTED given"},
		{"single letters in snake", "code=a_b_c given", "code=REDACTED given"},
		{"word-shaped code", "code=Abcdef given", "code=REDACTED given"},
		{"capitalised word that is not a status", "code=Correct given", "code=REDACTED given"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactText(tc.in); got != tc.want {
				t.Errorf("RedactText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactTextKeylessQueryAndPadTokenPrefixes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"keyless opaque run", "GET /x?abcXYZ123456 failed", "GET /x?REDACTED failed"},
		{"placeholders", "INSERT VALUES (?,?,?,?,?,?,?,?,?,?)", "INSERT VALUES (?,?,?,?,?,?,?,?,?,?)"},
		{"short prose", "really?yes", "really?yes"},
		{"reset token", "no reset for padres_0123456789abcdef", "no reset for REDACTED"},
		{"verify token", "padver_0123456789abcdef bad", "REDACTED bad"},
		{"webhook secret", "padwh_0123456789abcdef", "REDACTED"},
		{"app secret", "padapp_0123456789abcdef", "REDACTED"},
		{"install code", "padic_0123456789abcdef", "REDACTED"},
		{"ctx token", "padctx_0123456789abcdef", "REDACTED"},
		{"pad token with suffix", "pad_" + strings.Repeat("a", 64) + "_x", "REDACTED_x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactText(tc.in); got != tc.want {
				t.Errorf("RedactText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNumericSecretsAreMasked(t *testing.T) {
	if a := redactAttr(nil, slog.Int("otp", 482913)); a.Value.String() != redacted {
		t.Errorf("otp attr: %v", a)
	}
	if a := redactAttr(nil, slog.Int("pin", 4829)); a.Value.String() != redacted {
		t.Errorf("pin attr: %v", a)
	}
	if a := redactAttr(nil, slog.Int("code", 404)); a.Value.String() != "404" {
		t.Errorf("status code attr: %v", a)
	}
	if a := redactAttr(nil, slog.Int("code", 482913)); a.Value.String() != redacted {
		t.Errorf("long code attr: %v", a)
	}
	if a := redactAttr(nil, slog.Int("input_tokens", 482913)); a.Value.String() != "482913" {
		t.Errorf("count attr: %v", a)
	}
	got := sanitize(reflect.ValueOf(map[string]any{"verification_code": 123456, "code": 500, "n": 5}), 0, &walkBudget{nodes: 100, bytes: 1 << 20})
	m, _ := got.(map[string]any)
	if m["verification_code"] != redacted || m["code"] != 500 || m["n"] != 5 {
		t.Errorf("map walk: %v", got)
	}
}

func TestRound9KeylessDigitsMapKeysAndURLAttrs(t *testing.T) {
	if got := RedactText("GET /x?482913 failed"); got != "GET /x?REDACTED failed" {
		t.Errorf("digits run: %q", got)
	}
	if got := RedactText("GET /x?1234 failed"); got != "GET /x?1234 failed" {
		t.Errorf("short digits run: %q", got)
	}
	a := redactAttr(nil, slog.String("webhook_url", "https://hooks.slack.com/services/T0/B0/SECRET"))
	if strings.Contains(a.Value.String(), "SECRET") {
		t.Errorf("webhook_url leaked: %v", a)
	}
	got := sanitize(reflect.ValueOf(map[string]int{"https://h/cb?token=abcdef": 3}), 0, &walkBudget{nodes: 100, bytes: 1 << 20})
	if m, _ := got.(map[string]any); len(m) != 1 {
		t.Errorf("map: %v", got)
	} else {
		for k := range m {
			if strings.Contains(k, "abcdef") {
				t.Errorf("map key leaked: %q", k)
			}
		}
	}
}
