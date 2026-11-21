package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/secret"
)

// TASK-4323: the credentials this package holds (server fields, values read
// from a request header or cookie, request-body fields) are secret.Secret, so
// a %v, a slog attribute or a JSON dump shows the marker, not the value.
//
// The tests reach the values through any, so the rendering checks read the
// same on a string-typed field as on a Secret one.

// renderEveryWay prints v through each fmt verb and both slog handlers.
func renderEveryWay(v any) string {
	var b strings.Builder
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		b.WriteString(fmt.Sprintf(f, v))
		b.WriteByte('\n')
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "v", v)
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "v", v)
	b.Write(buf.Bytes())
	return b.String()
}

func assertNeverRenders(t *testing.T, label string, v any, fixture string) {
	t.Helper()
	out := renderEveryWay(v)
	if strings.Contains(out, fixture) {
		t.Errorf("%s: the value shows when rendered:\n%s", label, out)
	}
	if !strings.Contains(out, secret.Redacted) {
		t.Errorf("%s: %q missing from the rendering:\n%s", label, secret.Redacted, out)
	}
}

// revealAny returns the value a credential carries, whether it is a string
// or something with a Reveal method.
func revealAny(t *testing.T, v any) string {
	t.Helper()
	switch x := v.(type) {
	case string:
		return x
	case interface{ Reveal() string }:
		return x.Reveal()
	}
	t.Fatalf("revealAny: unexpected type %T", v)
	return ""
}

func assertRevealable(t *testing.T, label string, v any, want string) {
	t.Helper()
	r, ok := v.(interface{ Reveal() string })
	if !ok {
		t.Errorf("%s: %T has no Reveal method", label, v)
		return
	}
	if got := r.Reveal(); got != want {
		t.Errorf("%s: Reveal() = %q, want %q", label, got, want)
	}
}

func TestTASK4323_ServerHoldersNeverRender(t *testing.T) {
	srv := testServer(t)
	srv.SetEmailSender(nil, "fixture-SECRET-1")
	srv.SetMetricsToken("fixture-SECRET-2")
	srv.SetCloudMode("fixture-SECRET-3, fixture-SECRET-4")
	srv.SetBootstrapToken("fixture-SECRET-5", "")

	assertNeverRenders(t, "emailAPIKey", srv.emailAPIKey, "fixture-SECRET-1")
	assertNeverRenders(t, "metricsToken", srv.metricsToken, "fixture-SECRET-2")
	assertNeverRenders(t, "cloudSecrets", srv.cloudSecrets, "fixture-SECRET-3")
	assertNeverRenders(t, "cloudSecrets", srv.cloudSecrets, "fixture-SECRET-4")
	srv.bootstrapMu.Lock()
	bt := srv.bootstrapToken
	srv.bootstrapMu.Unlock()
	assertNeverRenders(t, "bootstrapToken", bt, "fixture-SECRET-5")

	assertRevealable(t, "emailAPIKey", srv.emailAPIKey, "fixture-SECRET-1")
	assertRevealable(t, "metricsToken", srv.metricsToken, "fixture-SECRET-2")
	assertRevealable(t, "bootstrapToken", bt, "fixture-SECRET-5")
	rv := reflect.ValueOf(srv.cloudSecrets)
	if rv.Len() != 2 {
		t.Fatalf("cloudSecrets: %d entries, want 2", rv.Len())
	}
	assertRevealable(t, "cloudSecrets[0]", rv.Index(0).Interface(), "fixture-SECRET-3")
	assertRevealable(t, "cloudSecrets[1]", rv.Index(1).Interface(), "fixture-SECRET-4")

	// The rotated key still authenticates, and a near miss does not.
	if r := doMetricsRequest(srv, "203.0.113.5:1", "Bearer fixture-SECRET-2"); r.Code == http.StatusUnauthorized {
		t.Errorf("metrics with the configured token: got 401")
	}
	srv.bootstrapMu.Lock()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/bootstrap", nil)
	req.Header.Set(BootstrapTokenHeader, "fixture-SECRET-5")
	okTok := srv.checkBootstrapToken(req)
	req.Header.Set(BootstrapTokenHeader, "fixture-SECRET-")
	nearMiss := srv.checkBootstrapToken(req)
	srv.bootstrapMu.Unlock()
	if !okTok || nearMiss {
		t.Errorf("checkBootstrapToken: configured=%v near-miss=%v, want true false", okTok, nearMiss)
	}

	// A cleared email key is empty, not the marker.
	srv.SetEmailSender(nil, "")
	if got := revealAny(t, srv.emailAPIKey); got != "" {
		t.Errorf("emailAPIKey after clear = %q", got)
	}
}

func TestTASK4323_RequestCredentialHelpersNeverRender(t *testing.T) {
	srv := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer fixture-SECRET-6")
	r.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: "fixture-SECRET-7"})
	r.AddCookie(&http.Cookie{Name: "csrf-probe", Value: "fixture-SECRET-8"})

	tok, ok := bearerToken(r)
	if !ok {
		t.Fatal("bearerToken: prefix not seen")
	}
	assertNeverRenders(t, "bearerToken", tok, "fixture-SECRET-6")
	assertRevealable(t, "bearerToken", tok, "fixture-SECRET-6")

	ext, ok := extractBearer(r.Header.Get("Authorization"))
	if !ok {
		t.Fatal("extractBearer: not parsed")
	}
	assertNeverRenders(t, "extractBearer", ext, "fixture-SECRET-6")
	assertRevealable(t, "extractBearer", ext, "fixture-SECRET-6")

	app := appBearer(r)
	assertNeverRenders(t, "appBearer", app, "fixture-SECRET-6")
	assertRevealable(t, "appBearer", app, "fixture-SECRET-6")

	sc, ok := srv.sessionCookie(r)
	if !ok {
		t.Fatal("sessionCookie: not found")
	}
	assertNeverRenders(t, "sessionCookie", sc, "fixture-SECRET-7")
	assertRevealable(t, "sessionCookie", sc, "fixture-SECRET-7")

	cs, ok := cookieSecret(r, "csrf-probe")
	if !ok {
		t.Fatal("cookieSecret: not found")
	}
	assertNeverRenders(t, "cookieSecret", cs, "fixture-SECRET-8")
	assertRevealable(t, "cookieSecret", cs, "fixture-SECRET-8")

	// The legacy session cookie name is the fallback when the primary is absent.
	legacy := httptest.NewRequest(http.MethodGet, "/", nil)
	legacy.AddCookie(&http.Cookie{Name: "pad_session", Value: "fixture-SECRET-9"})
	srv.secureCookies = true
	lc, ok := srv.sessionCookie(legacy)
	srv.secureCookies = false
	if !ok {
		t.Fatal("sessionCookie: legacy name not found")
	}
	assertNeverRenders(t, "sessionCookie legacy", lc, "fixture-SECRET-9")
	assertRevealable(t, "sessionCookie legacy", lc, "fixture-SECRET-9")

	if _, ok := cookieSecret(legacy, "absent"); ok {
		t.Error("cookieSecret: an absent cookie reported present")
	}

	// A case-insensitive scheme still parses for the MCP door.
	lower, ok := extractBearer("bearer fixture-SECRET-10")
	if !ok {
		t.Fatal("extractBearer: lower-case scheme not parsed")
	}
	assertNeverRenders(t, "extractBearer lower", lower, "fixture-SECRET-10")
}

// TestTASK4323_BearerParseUnchanged pins the three Authorization parses as
// they were before the conversion: the type moved, the meaning did not.
func TestTASK4323_BearerParseUnchanged(t *testing.T) {
	cases := []struct {
		header string
		// bearerToken: value, prefix seen
		bt   string
		btOK bool
		// extractBearer
		ex   string
		exOK bool
		// appBearer
		app string
	}{
		{"", "", false, "", false, ""},
		{"Bearer tok", "tok", true, "tok", true, "tok"},
		{"Bearer  tok ", "tok", true, "tok", true, "tok"},
		{"bearer tok", "bearer tok", false, "tok", true, "tok"},
		{"BEARER tok", "BEARER tok", false, "tok", true, "tok"},
		{"Bearer ", "", true, "", false, ""},
		{"Bearer", "Bearer", false, "", false, ""},
		{"Basic dXNlcjpwYXNz", "Basic dXNlcjpwYXNz", false, "", false, ""},
		{"Bearer\ttok", "Bearer\ttok", false, "", false, ""},
		{"tok", "tok", false, "", false, ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		bt, btOK := bearerToken(r)
		if got := revealAny(t, bt); got != c.bt || btOK != c.btOK {
			t.Errorf("bearerToken(%q) = %q,%v want %q,%v", c.header, got, btOK, c.bt, c.btOK)
		}
		ex, exOK := extractBearer(c.header)
		if got := revealAny(t, ex); got != c.ex || exOK != c.exOK {
			t.Errorf("extractBearer(%q) = %q,%v want %q,%v", c.header, got, exOK, c.ex, c.exOK)
		}
		if got := revealAny(t, appBearer(r)); got != c.app {
			t.Errorf("appBearer(%q) = %q want %q", c.header, got, c.app)
		}
	}
}

func TestTASK4323_DecodeOnlyDTOsNeverRender(t *testing.T) {
	var push pushDeviceBody
	if err := json.Unmarshal([]byte(`{"platform":"android","token":"fixture-SECRET-11","app_version":"1"}`), &push); err != nil {
		t.Fatalf("pushDeviceBody: %v", err)
	}
	assertNeverRenders(t, "pushDeviceBody", push, "fixture-SECRET-11")
	assertRevealable(t, "pushDeviceBody.Token", push.Token, "fixture-SECRET-11")

	var dec decisionSettingsInput
	if err := json.Unmarshal([]byte(`{"api_key":"fixture-SECRET-12"}`), &dec); err != nil {
		t.Fatalf("decisionSettingsInput: %v", err)
	}
	if dec.APIKey == nil {
		t.Fatal("decisionSettingsInput: api_key not decoded")
	}
	assertNeverRenders(t, "decisionSettingsInput", dec, "fixture-SECRET-12")
	assertNeverRenders(t, "decisionSettingsInput.APIKey", *dec.APIKey, "fixture-SECRET-12")
	assertRevealable(t, "decisionSettingsInput.APIKey", *dec.APIKey, "fixture-SECRET-12")

	// Absent and null leave the key unset; a number is refused as before.
	for _, body := range []string{`{}`, `{"api_key":null}`} {
		var in decisionSettingsInput
		if err := json.Unmarshal([]byte(body), &in); err != nil || in.APIKey != nil {
			t.Errorf("%s: err=%v set=%v, want nil false", body, err, in.APIKey != nil)
		}
	}
	var num decisionSettingsInput
	if err := json.Unmarshal([]byte(`{"api_key":42}`), &num); err == nil {
		t.Error(`{"api_key":42}: decoded, want an error`)
	}
}

// TestTASK4323_DecodeOnlyDTOsAnswerAsBefore drives the admin endpoint: an
// empty key and a number-typed key get the 400s they got before.
func TestTASK4323_DecodeOnlyDTOsAnswerAsBefore(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/decision-provider", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+sessionToken)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}
	for _, c := range []struct {
		body string
		want string
	}{
		{`{"api_key":""}`, "api_key is empty"},
		{`{"api_key":"   "}`, "api_key is empty"},
		{`{"api_key":42}`, "Invalid request body"},
	} {
		w := put(c.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want 400 with %q", c.body, w.Code, w.Body.String(), c.want)
		}
	}
}

// TestTASK4323_EmptyCredentialIsRefused: an empty Secret equals an empty
// string, so every compare keeps its empty-value guard in front of Equal.
func TestTASK4323_EmptyCredentialIsRefused(t *testing.T) {
	t.Run("bootstrap", func(t *testing.T) {
		srv := &Server{}
		check := func() bool {
			srv.bootstrapMu.Lock()
			defer srv.bootstrapMu.Unlock()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/bootstrap", nil)
			r.Header.Set(BootstrapTokenHeader, "")
			return srv.checkBootstrapToken(r)
		}
		if check() {
			t.Error("no token configured, empty header: accepted")
		}
		srv.SetBootstrapToken("fixture-SECRET-13", "")
		if check() {
			t.Error("token configured, empty header: accepted")
		}
	})
	t.Run("metrics", func(t *testing.T) {
		srv := newMetricsTestServer(t, "fixture-SECRET-14")
		for _, h := range []string{"Bearer ", "Bearer    "} {
			if r := doMetricsRequest(srv, "127.0.0.1:1", h); r.Code != http.StatusUnauthorized {
				t.Errorf("metrics %q: %d, want 401", h, r.Code)
			}
		}
	})
	t.Run("csrf", func(t *testing.T) {
		srv := testServer(t)
		bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
		sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(`{"name":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", "")
		req.RemoteAddr = "192.0.2.1:1234"
		req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
		req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: ""})
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("empty CSRF cookie and header: %d, want 403", w.Code)
		}
	})
	t.Run("consent csrf", func(t *testing.T) {
		srv := &Server{}
		form := url.Values{"csrf_token": {""}}
		req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: csrfCookieName(false), Value: ""})
		if err := srv.validateConsentCSRFToken(req); err == nil {
			t.Error("empty consent CSRF cookie and field: accepted")
		}
	})
}

// task4323CredentialName matches a json field name that carries a credential.
var task4323CredentialName = regexp.MustCompile(`(^|_)(password|secret|token|api_key|code|verifier|proof)$`)

// task4323NotSecret lists the credential-named string fields that stay
// string, keyed file::Owner.Field (Owner is the named type, or the function
// holding an anonymous struct), with the reason. A response struct must stay
// string: a Secret there would put the marker on the wire.
var task4323NotSecret = map[string]string{
	"app_item_actions.go::appRedeemContext.Code":                        "a context code that names an item for an action; it authenticates nobody",
	"handlers_agenda.go::agendaUnavailable.Code":                        "an error code in a response",
	"handlers_apps_lifecycle.go::appInstallStateResponse.InstallCode":   "response: the install code returned to the admin who provisions the app",
	"handlers_apps_provision.go::appInstallConfirmResponse.InstallCode": "response: the install code returned to the admin who provisions the app",
	"handlers_auth.go::invitationNotJoined.Code":                        "an error code in a response",
	"handlers_calendar_feeds.go::handleCreateCalendarFeed.Token":        "response: the new feed token returned to its owner",
	"handlers_items_bulk.go::bulkItemFailure.Code":                      "an error code in a response",
	"handlers_members.go::handleListMembers.Code":                       "response: a pending invitation's code in the member list",
	"handlers_schedules.go::scheduleView.ErrorCode":                     "an error code in a response",
}

// TestTASK4323_EveryCredentialDTOFieldIsSecret: a json-tagged field in this
// package whose name says credential is secret.Secret (or a pointer or slice
// of one), or is listed above with its reason. A listed field that no longer
// exists, or that is now a Secret, fails too.
func TestTASK4323_EveryCredentialDTOFieldIsSecret(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	var bad []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scan := func(owner string, root ast.Node) {
			ast.Inspect(root, func(n ast.Node) bool {
				st, ok := n.(*ast.StructType)
				if !ok {
					return true
				}
				for _, fld := range st.Fields.List {
					if fld.Tag == nil {
						continue
					}
					tag, _ := strconv.Unquote(fld.Tag.Value)
					jn := strings.Split(reflect.StructTag(tag).Get("json"), ",")[0]
					if jn == "" || jn == "-" || !task4323CredentialName.MatchString(jn) {
						continue
					}
					typ := exprString(fld.Type)
					base := strings.TrimLeft(typ, "*[]")
					isSecret := base == "secret.Secret"
					if !isSecret && base != "string" {
						continue // a flag or a count, not a carrier
					}
					for _, id := range fld.Names {
						key := path + "::" + owner + "." + id.Name
						if _, listed := task4323NotSecret[key]; listed {
							seen[key] = true
							if isSecret {
								bad = append(bad, fmt.Sprintf("%s (json %q) is %s but listed as not secret", key, jn, typ))
							}
							continue
						}
						if !isSecret {
							bad = append(bad, fmt.Sprintf("%s (json %q at %s) is %s", key, jn, fset.Position(fld.Pos()), typ))
						}
					}
				}
				return true
			})
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				scan(d.Name.Name, d)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						scan(sp.Name.Name, sp)
					case *ast.ValueSpec:
						scan(sp.Names[0].Name, sp)
					}
				}
			}
		}
	}
	for key := range task4323NotSecret {
		if !seen[key] {
			bad = append(bad, key+": listed as not secret but no such field")
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.ArrayType:
		return "[]" + exprString(x.Elt)
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.MapType:
		return "map[" + exprString(x.Key) + "]" + exprString(x.Value)
	}
	return fmt.Sprintf("%T", e)
}
