package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3653: every door that adds attachment bytes to a workspace is charged
// against the storage cap TASK-3619 enforces at upload, and is refused when
// over it. Upload was the only door that checked; bundle import and
// cross-workspace copy inserted their rows unchecked.

// capPlan puts user on plan with a tiny storage_bytes override standing in
// for the real cap; it rides the same resolution chain.
func capPlan(t *testing.T, srv *Server, userID, plan string, limit int) {
	t.Helper()
	if _, err := srv.store.SetUserPlan(userID, store.PlanWrite{Plan: plan, Source: store.PlanSourceManual, Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetUserPlanOverrides(userID, `{"storage_bytes":`+strconv.Itoa(limit)+`}`); err != nil {
		t.Fatal(err)
	}
}

func withFSAttachments(t *testing.T, srv *Server) {
	t.Helper()
	fs, err := attachments.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	srv.SetAttachments(reg, 0)
}

// planLimitRefusal decodes a 403 and checks it is the storage refusal.
func planLimitRefusal(t *testing.T, body []byte, code int) map[string]any {
	t.Helper()
	if code != http.StatusForbidden {
		t.Fatalf("status %d %s, want 403 plan_limit_exceeded", code, body)
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "plan_limit_exceeded" || env.Error.Details["feature"] != "storage_bytes" {
		t.Fatalf("refusal = %s, want plan_limit_exceeded on storage_bytes", body)
	}
	return env.Error.Details
}

// Copy: a Pro account's storage is pooled, so copying an item whose file is
// 30 bytes into a sibling workspace takes the account from 30 to 60 bytes,
// past its 40. A direct upload of those bytes is refused; the copy was not.
// Self-hosted (cloud off) is not limited and still copies.
func TestBUG3653_CopyPastTheStorageLimitIs403(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		srv := testServer(t)
		srv.cloudMode = cloud
		withFSAttachments(t, srv)
		user, tok := loginTestUserAs(t, srv, "copy3653@example.test", "Copier", "pw-3653-abcdefgh")
		capPlan(t, srv, user.ID, "pro", 40)

		src := mustCreateOwnedWorkspace(t, srv, "Cap Source 3653", user)
		dst := mustCreateOwnedWorkspace(t, srv, "Cap Dest 3653", user)
		srcColl := mustCollection(t, srv, src.ID, "Tasks")
		dstColl := mustCollection(t, srv, dst.ID, "Tasks")

		rr := capUpload(srv, src.Slug, tok, []byte(strings.Repeat("a", 30)))
		if rr.Code != http.StatusCreated {
			t.Fatalf("cloud=%v upload: %d %s", cloud, rr.Code, rr.Body.String())
		}
		var up struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &up); err != nil || up.ID == "" {
			t.Fatalf("decode upload: %v %s", err, rr.Body.String())
		}
		it, err := srv.store.CreateItem(src.ID, srcColl.ID, models.ItemCreate{
			Title: "Has a file", Content: "[notes](pad-attachment:" + up.ID + ")",
		})
		if err != nil {
			t.Fatal(err)
		}

		rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+src.Slug+"/items/"+it.Slug+"/copy",
			map[string]any{"target_workspace": dst.Slug, "target_collection": dstColl.Slug}, tok)
		total, _ := attachmentRowsFor(t, srv, dst.ID)
		if !cloud {
			if rr.Code != http.StatusCreated || total != 1 {
				t.Fatalf("self-hosted copy: %d %s, %d rows in the destination, want 201 and 1", rr.Code, rr.Body.String(), total)
			}
			continue
		}
		d := planLimitRefusal(t, rr.Body.Bytes(), rr.Code)
		if d["current"] != float64(30) || d["requested"] != float64(30) || d["limit"] != float64(40) {
			t.Errorf("refusal details = %v, want current 30, requested 30, limit 40", d)
		}
		if total != 0 {
			t.Errorf("refused copy left %d attachment rows in the destination", total)
		}
		var items int
		if err := srv.store.DB().QueryRow(srv.store.D().Rebind(`SELECT COUNT(*) FROM items WHERE workspace_id = ?`), dst.ID).Scan(&items); err != nil {
			t.Fatal(err)
		}
		if items != 0 {
			t.Errorf("refused copy left %d items in the destination", items)
		}
	}
}

// Import: a bundle whose file alone exceeds the importer's cap is refused
// with the plan-limit 403 and leaves no workspace behind. Self-hosted imports
// it.
func TestBUG3653_ImportPastTheStorageLimitIs403(t *testing.T) {
	bundle := realBundleWithBlob(t)
	for _, cloud := range []bool{true, false} {
		srv := attachmentsServerOn(t, store.DriverSQLite)
		srv.cloudMode = cloud
		user, tok := memberImporter(t, srv)
		capPlan(t, srv, user.ID, "free", 10)

		rr := importAs(srv, "capped-import", bundle, tok)
		if !cloud {
			if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
				t.Fatalf("self-hosted import: %d %s", rr.Code, rr.Body.String())
			}
			continue
		}
		planLimitRefusal(t, rr.Body.Bytes(), rr.Code)
		var live, rows int
		if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
			`SELECT COUNT(*) FROM workspaces WHERE owner_id = ? AND deleted_at IS NULL`), user.ID).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
			`SELECT COUNT(*) FROM attachments a JOIN workspaces w ON w.id = a.workspace_id WHERE w.owner_id = ? AND a.deleted_at IS NULL`), user.ID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if live != 0 || rows != 0 {
			t.Errorf("refused import left %d live workspaces and %d live attachment rows", live, rows)
		}
	}
}
