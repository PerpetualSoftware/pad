package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// capUpload posts a multipart upload as the session's user.
func capUpload(srv *Server, slug, token string, body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "notes.txt")
	part.Write(body)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	// Double-submit CSRF: the same token in the cookie and the header.
	const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: csrfCookieName(srv.secureCookies), Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// TASK-3619: in cloud mode an upload that would take a Pro workspace past its
// storage limit is refused with the plan-limit 403, naming the plan, the bytes,
// and that existing files stay; under the limit it lands. Self-hosted mode
// (no WithPlanLimit) is not refused.
func TestTASK3619_UploadPastTheStorageLimitIs403(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		srv := testServer(t)
		srv.cloudMode = cloud
		fs, err := attachments.NewFSStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		reg := attachments.NewRegistry()
		reg.Register(attachments.FSPrefix, fs)
		srv.SetAttachments(reg, 0)

		user, token := loginTestUser(t, srv)
		if _, err := srv.store.SetUserPlan(user.ID, store.PlanWrite{Plan: "pro", Source: store.PlanSourceManual, Force: true}); err != nil {
			t.Fatal(err)
		}
		// A small override stands in for the 1 GB cap; it rides the same chain.
		if err := srv.store.SetUserPlanOverrides(user.ID, `{"storage_bytes":40}`); err != nil {
			t.Fatal(err)
		}
		ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Capped", OwnerID: user.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
			t.Fatal(err)
		}

		if rr := capUpload(srv, ws.Slug, token, []byte(strings.Repeat("a", 30))); rr.Code != http.StatusCreated {
			t.Fatalf("cloud=%v under the limit: %d %s", cloud, rr.Code, rr.Body.String())
		}
		rr := capUpload(srv, ws.Slug, token, []byte(strings.Repeat("b", 20)))
		if !cloud {
			if rr.Code != http.StatusCreated {
				t.Fatalf("self-hosted past the limit: %d %s, want 201", rr.Code, rr.Body.String())
			}
			continue
		}
		if rr.Code != http.StatusForbidden {
			t.Fatalf("past the limit: %d %s, want 403", rr.Code, rr.Body.String())
		}
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		e := env.Error
		if e.Code != "plan_limit_exceeded" || e.Details["feature"] != "storage_bytes" || e.Details["plan"] != "pro" ||
			e.Details["limit"] != float64(40) || e.Details["current"] != float64(30) || e.Details["requested"] != float64(20) || e.Details["payer_is_you"] != true {
			t.Fatalf("refusal = %+v", e)
		}
		for _, want := range []string{"20 B", "the owner's account", "40 B", "Pro plan", "30 B used", "Existing files stay available"} {
			if !strings.Contains(e.Message, want) {
				t.Errorf("message %q lacks %q", e.Message, want)
			}
		}
	}
}

func TestTASK3619_HumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "1.5 KB",
		524288000: "500 MB", 1 << 30: "1 GB", 1610612736: "1.5 GB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
