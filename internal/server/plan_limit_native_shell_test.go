package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// PLAN-3291 DR-1 / DR-3, server half (TASK-3293). A plan-limit refusal
// answered to a mobile shell's web view names no upgrade destination, at every
// door that renders one: the single 403, the bulk envelope's per-item failure,
// and the bundle import's own refusal. Each door is driven through ServeHTTP,
// so the tests vouch for the binding and not only for the helper (CONVE-19),
// and each shell leg is paired with the same request without the marker, which
// must still carry upgrade_url (CONVE-12: a fix that dropped the link for
// everyone would pass the shell leg alone).

// shellUA is what the Android shell sends: the platform UA with the marker
// appended (pad-mobile AppUserAgent.kt). The iOS shell's differs only before
// the marker.
const shellUA = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/129.0 Mobile Safari/537.36 PadShell/1"

// upgradeWords are the commerce words a shell-addressed message must not
// carry. The server message is a statement of fact today; this pins that.
var upgradeWords = []string{"upgrade", " pro", "billing", "pricing", "plan →", "/console"}

func (e *planLimitEnv) doUA(method, path, contentType string, body []byte, ua string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+e.pat)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	e.srv.ServeHTTP(rr, req)
	return rr
}

type planLimitEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func assertShellRefusal(t *testing.T, msg string, details map[string]any, shell bool) {
	t.Helper()
	_, has := details["upgrade_url"]
	if shell && has {
		t.Errorf("shell request: details carry upgrade_url %v, want absent", details["upgrade_url"])
	}
	if !shell && !has {
		t.Errorf("browser request: details lack upgrade_url, want /console/billing (details=%v)", details)
	}
	if details["feature"] == nil || details["limit"] == nil {
		t.Errorf("details lost the refusal's facts: %v", details)
	}
	if shell {
		low := strings.ToLower(msg)
		for _, w := range upgradeWords {
			if strings.Contains(low, w) {
				t.Errorf("shell message %q contains %q", msg, w)
			}
		}
	}
}

func TestPlanLimit_NativeShell_SingleDoor(t *testing.T) {
	for _, shell := range []bool{true, false} {
		t.Run(fmt.Sprintf("shell=%v", shell), func(t *testing.T) {
			e := newPlanLimitEnv(t)
			current := e.countIn(t, `SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`)
			if err := e.srv.store.SetUserPlanOverrides(e.user.ID, fmt.Sprintf(`{"items_per_workspace":%d}`, current)); err != nil {
				t.Fatalf("SetUserPlanOverrides: %v", err)
			}
			ua := ""
			if shell {
				ua = shellUA
			}
			body, _ := json.Marshal(map[string]any{"title": "over the cap"})
			rr := e.doUA("POST", "/api/v1/workspaces/"+e.home.Slug+"/collections/tasks/items", "application/json", body, ua)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
			}
			var env planLimitEnvelope
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != "plan_limit_exceeded" {
				t.Fatalf("code = %q, want plan_limit_exceeded", env.Error.Code)
			}
			assertShellRefusal(t, env.Error.Message, env.Error.Details, shell)
		})
	}
}

func TestPlanLimit_NativeShell_BulkDoor(t *testing.T) {
	for _, shell := range []bool{true, false} {
		t.Run(fmt.Sprintf("shell=%v", shell), func(t *testing.T) {
			e := newRestoreLimitEnv(t)
			gone := e.archived(t, 1)
			e.setCap(t, e.liveItems(t))
			ua := ""
			if shell {
				ua = shellUA
			}
			body, _ := json.Marshal(map[string]any{"ids": []string{gone[0].Ref}, "op": "restore"})
			rr := e.doUA("POST", "/api/v1/workspaces/"+e.home.Slug+"/items/bulk", "application/json", body, ua)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
			}
			var env struct {
				Failed []struct {
					Code    string         `json:"code"`
					Error   string         `json:"error"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"failed"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(env.Failed) != 1 || env.Failed[0].Code != "plan_limit_exceeded" {
				t.Fatalf("failed = %+v, want one plan_limit_exceeded (body=%s)", env.Failed, rr.Body.String())
			}
			f := env.Failed[0]
			assertShellRefusal(t, f.Error+" "+f.Message, f.Details, shell)
		})
	}
}

// The bundle import's own refusal (handlers_import_bundle.go, BUG-3103) is
// the one that names `requested`: a bundle carrying more items than the cap.
func TestPlanLimit_NativeShell_BundleImportDoor(t *testing.T) {
	for _, shell := range []bool{true, false} {
		t.Run(fmt.Sprintf("shell=%v", shell), func(t *testing.T) {
			srv, _ := testServerWithAttachments(t)
			srv.cloudMode = true
			u := mintTestUser(t, srv, "bundle-shell@example.com")
			tok := loginUser(t, srv, "bundle-shell@example.com", "correct-horse-battery-staple")
			if err := srv.store.SetUserPlan(u.ID, "free", ""); err != nil {
				t.Fatalf("SetUserPlan: %v", err)
			}
			src, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Bundle Source", OwnerID: u.ID})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			if err := srv.store.SeedCollectionsFromTemplate(src.ID, "startup"); err != nil {
				t.Fatalf("Seed: %v", err)
			}
			coll, err := srv.store.GetCollectionBySlug(src.ID, "tasks")
			if err != nil || coll == nil {
				t.Fatalf("tasks: %v, %v", coll, err)
			}
			for i := 0; i < 3; i++ {
				if _, err := srv.store.CreateItem(src.ID, coll.ID, models.ItemCreate{Title: fmt.Sprintf("t%d", i)}); err != nil {
					t.Fatalf("CreateItem: %v", err)
				}
			}
			export, err := srv.store.ExportWorkspace(src.Slug)
			if err != nil {
				t.Fatalf("ExportWorkspace: %v", err)
			}
			if err := srv.store.SetUserPlanOverrides(u.ID, `{"items_per_workspace":1,"workspaces":10}`); err != nil {
				t.Fatalf("SetUserPlanOverrides: %v", err)
			}
			payload, _ := json.Marshal(export)
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			if err := tw.WriteHeader(&tar.Header{Name: "pad-export.json", Mode: 0o644, Size: int64(len(payload))}); err != nil {
				t.Fatalf("header: %v", err)
			}
			if _, err := tw.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			tw.Close()
			gzw.Close()

			req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=shell-bundle", bytes.NewReader(buf.Bytes()))
			req.Header.Set("Content-Type", "application/gzip")
			if shell {
				req.Header.Set("User-Agent", shellUA)
			}
			req.RemoteAddr = "192.0.2.1:1234"
			req.AddCookie(&http.Cookie{Name: "pad_session", Value: tok})
			const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: testCSRF})
			req.Header.Set("X-CSRF-Token", testCSRF)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
			}
			var env planLimitEnvelope
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// requested proves this is the in-import refusal, not the
			// workspaces pre-check above it.
			if env.Error.Code != "plan_limit_exceeded" || env.Error.Details["requested"] == nil {
				t.Fatalf("want the in-import items refusal with `requested`, got %s", rr.Body.String())
			}
			assertShellRefusal(t, env.Error.Message, env.Error.Details, shell)
		})
	}
}

func TestFromNativeShell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ua   string
		want bool
	}{
		{shellUA, true},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 PadShell/1", true},
		{"… PadShell/2", true},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1", false},
		{"pad-cli/0.16", false},
		{"", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("User-Agent", c.ua)
		if got := fromNativeShell(r); got != c.want {
			t.Errorf("fromNativeShell(%q) = %v, want %v", c.ua, got, c.want)
		}
	}
	if fromNativeShell(nil) {
		t.Error("a nil request is not an app")
	}
}

func TestPlanLimitDetails_NativeShellOmitsOnlyUpgradeURL(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("User-Agent", shellUA)
	res := &store.LimitResult{Feature: "items_per_workspace", Limit: 100, Current: 100, Plan: "free"}
	got := planLimitDetails(r, res)
	want := map[string]interface{}{"feature": "items_per_workspace", "limit": 100, "current": 100, "plan": "free"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("shell details = %v, want %v", got, want)
	}
	if msg := planLimitMessage(r, res); msg != "You've reached the 100-item limit on the free plan." {
		t.Errorf("shell message = %q", msg)
	}
}

// The invitee's member-limit refusal (BUG-3098) is its own message, not the
// plan-limit helpers', and it says "upgrade their plan". Both accept doors,
// the existing-account accept and register-with-code, must drop that clause
// for a shell and keep it for a browser (codex r1 on TASK-3293).
func TestMemberLimitRefusal_NativeShell(t *testing.T) {
	for _, door := range []string{"existing", "register"} {
		for _, shell := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/shell=%v", door, shell), func(t *testing.T) {
				e := newAcceptLimitEnv(t)
				inv := e.invite(t, door+"@example.com")
				e.setMemberCap(t, e.members(t))

				var req *http.Request
				if door == "existing" {
					u, err := e.srv.store.CreateUser(models.UserCreate{Email: door + "@example.com", Name: "Invitee", Password: "pw-invitee-12345"})
					if err != nil {
						t.Fatalf("CreateUser: %v", err)
					}
					tok, err := e.srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
					if err != nil {
						t.Fatalf("CreateSession: %v", err)
					}
					req = httptest.NewRequest("POST", "/api/v1/invitations/"+inv.Code+"/accept", nil)
					const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
					req.AddCookie(&http.Cookie{Name: "pad_session", Value: tok})
					req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: testCSRF})
					req.Header.Set("X-CSRF-Token", testCSRF)
				} else {
					body, _ := json.Marshal(map[string]string{
						"email": door + "@example.com", "name": "New Invitee",
						"password": "correct-horse-battery-staple", "invitation_code": inv.Code,
					})
					req = httptest.NewRequest("POST", "/api/v1/auth/register", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
				}
				req.RemoteAddr = "192.0.2.1:1234"
				if shell {
					req.Header.Set("User-Agent", shellUA)
				}
				rr := httptest.NewRecorder()
				e.srv.ServeHTTP(rr, req)

				var env planLimitEnvelope
				if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || rr.Code != http.StatusForbidden || env.Error.Code != "workspace_member_limit" {
					t.Fatalf("want 403 workspace_member_limit, got %d %s", rr.Code, rr.Body.String())
				}
				hasUpgrade := strings.Contains(strings.ToLower(env.Error.Message), "upgrade")
				if shell && hasUpgrade {
					t.Errorf("shell message offers an upgrade: %q", env.Error.Message)
				}
				if !shell && !hasUpgrade {
					t.Errorf("browser message lost its upgrade clause: %q", env.Error.Message)
				}
				if !strings.Contains(env.Error.Message, "make room") {
					t.Errorf("message lost the remedy: %q", env.Error.Message)
				}
			})
		}
	}
}
