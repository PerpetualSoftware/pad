package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

const hiddenLinkedSentence = "This date is linked to something you can't view. Ask a workspace owner to change or remove the link."

func TestTASK4423_R03LinkedRefusal(t *testing.T) {
	var baseline string
	for _, direction := range []bool{false, true} {
		for _, deleted := range []bool{false, true} {
			for _, n := range []int{1, 2} {
				for _, method := range []string{"PATCH", "DELETE"} {
					t.Run(fmt.Sprintf("parent=%v/deleted=%v/partners=%d/%s", direction, deleted, n, method), func(t *testing.T) {
						f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
						for i := 1; i <= n; i++ {
							p, c := 0, i
							if !direction {
								p, c = i, 0
							}
							pr, cr := "target", "target"
							if i == 2 && !direction {
								cr = "earliest"
								_, err := f.srv.store.DB().Exec(`UPDATE item_schedules SET earliest_days=0,earliest_secs=0 WHERE item_id=?`, f.item.ID)
								if err != nil {
									t.Fatal(err)
								}
							}
							f.link(p, c, pr, cr, "")
						}
						g := f.guest(0)
						if deleted {
							for i := 1; i <= n; i++ {
								if _, err := f.srv.store.DB().Exec(`UPDATE items SET deleted_at='2026-01-01T00:00:00Z' WHERE id=?`, f.items[i].ID); err != nil {
									t.Fatal(err)
								}
							}
						}
						var body any
						if method == "PATCH" {
							body = map[string]any{"dtstart": "2099-02-01T09:00"}
						}
						rr := f.as(f.token(g, ""), method, f.path(f.item), body)
						f.expectErr(rr, 409, "schedule_linked", "")
						if !strings.Contains(rr.Body.String(), hiddenLinkedSentence) {
							t.Fatalf("hidden refusal: %s", rr.Body.String())
						}
						if baseline == "" {
							baseline = rr.Body.String()
						}
						if rr.Body.String() != baseline {
							t.Fatalf("refusal bytes differ: %s", rr.Body.String())
						}
					})
				}
			}
		}
	}
	t.Run("readable control", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
		f.link(0, 1, "target", "target", "")
		rr := f.as(f.token(f.owner, ""), "DELETE", f.path(f.item), nil)
		f.expectErr(rr, 409, "schedule_linked", "")
		if !strings.Contains(rr.Body.String(), store.ErrScheduleLinked.Error()) {
			t.Fatal(rr.Body.String())
		}
	})
}
func TestTASK4423_R04Errors(t *testing.T) {
	for _, readable := range []bool{false, true} {
		t.Run(fmt.Sprintf("schedule-linked message/readable=%v", readable), func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
			f.link(0, 1, "target", "target", "")
			tok := f.token(f.owner, "")
			want := store.ErrScheduleLinked.Error()
			if !readable {
				tok = f.token(f.guest(0), "")
				want = hiddenLinkedSentence
			}
			rr := f.as(tok, "DELETE", f.path(f.item), nil)
			f.expectErr(rr, 409, "schedule_linked", "")
			envelope := schedMap(t, rr)["error"].(map[string]any)
			if envelope["message"] != want || strings.Contains(rr.Body.String(), f.items[1].ID) {
				t.Fatalf("schedule-linked message contract: %s", rr.Body.String())
			}
		})
	}

	rows := []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrCommitmentPreviewRequired, 409, "commitment_preview_required"}, {store.ErrScheduleLinked, 409, "schedule_linked"}, {store.ErrDeadlineApprovalRequired, 409, "deadline_approval_required"}, {store.ErrLinkViolationAck, 409, "link_violation_ack"}, {store.ErrCommitmentChanged, 409, "commitment_changed"}, {store.ErrLinkBudget, 503, "link_budget"}, {store.ErrLinkEditRequired, 403, "link_edit_required"}, {store.ErrScheduleLinkNotFound, 404, "schedule_link_not_found"},
	}
	for _, code := range []string{"link_cycle", "link_self", "link_duplicate_incoming", "link_limit", "link_invalid", "gate_duplicate"} {
		status := 409
		if code == "link_invalid" {
			status = 400
		}
		rows = append(rows, struct {
			err    error
			status int
			code   string
		}{&store.LinkRefusedError{Code: code, Detail: "hidden-sentinel-id"}, status, code})
	}
	for _, field := range []string{"reason", "revision", "role", "to", "cancel_policy"} {
		rows = append(rows, struct {
			err    error
			status int
			code   string
		}{&store.ScheduleFieldError{Field: field, Code: "required", Message: "required"}, 400, "validation_error"})
	}
	for _, row := range rows {
		t.Run(row.err.Error(), func(t *testing.T) {
			rr := httptest.NewRecorder()
			r := httptest.NewRequest("PATCH", "/api/v1/workspaces/w/items/root/commitment", nil)
			(&Server{}).writeResolveError(rr, r, fmt.Errorf("wrapped: %w", row.err))
			if rr.Code != row.status || errCode(t, rr) != row.code {
				t.Errorf("got %d %s want %d %s", rr.Code, rr.Body.String(), row.status, row.code)
			}

			if fld, ok := row.err.(*store.ScheduleFieldError); ok {
				_, details := schedErrDetails(t, rr)
				if len(details) != 2 || details["field"] != fld.Field || details["code"] != fld.Code {
					t.Fatalf("field error details: %s", rr.Body.String())
				}
			}
			if row.code == "commitment_changed" {
				_, details := schedErrDetails(t, rr)
				if len(details) != 2 || details["field"] != "revision" || details["code"] != "stale" {
					t.Fatalf("stale details: %s", rr.Body.String())
				}
			}
			if row.code == "schedule_link_not_found" && rr.Body.String() != `{"error":{"code":"schedule_link_not_found","message":"Commitment link or gate not found"}}`+"\n" {
				t.Fatalf("graph not-found literal changed: %s", rr.Body.String())
			}
			if row.code != "validation_error" && row.code != "commitment_changed" && row.code != "commitment_preview_required" && strings.Contains(rr.Body.String(), `"details"`) {
				t.Fatal("graph refusal exposed details")
			}
			if row.code == "link_budget" && rr.Header().Get("Retry-After") != "1" {
				t.Error("retry header absent")
			}
			if strings.Contains(rr.Body.String(), "hidden-sentinel-id") {
				t.Fatal("hidden id disclosed")
			}
		})
	}
}

const ownerLinkedSentence = "This date is linked to something you can't view. You can remove the link."

// A restricted owner is told they can remove a link to a hidden end, and given
// only the link to delete. Anyone the remove bypass does not cover, an app
// principal with an owner membership included, gets the generic sentence.
func TestTASK4423_R03OwnerLinkedRefusal(t *testing.T) {
	for _, who := range []string{"workspace", "member", "app"} {
		t.Run(who, func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
			hidden := mustCollection(t, f.srv, f.ws.ID, "Hidden")
			if _, err := f.srv.store.DB().Exec(`UPDATE items SET collection_id=? WHERE id=?`, hidden.ID, f.items[1].ID); err != nil {
				t.Fatal(err)
			}
			f.link(0, 1, "target", "target", "")
			id := f.resourceID("links")
			u := f.member("refusal-owner@example.com", "owner")
			switch who {
			case "workspace":
				u = f.owner
				if _, err := f.srv.store.DB().Exec(`UPDATE workspace_members SET role='editor' WHERE workspace_id=? AND user_id=?`, f.ws.ID, u.ID); err != nil {
					t.Fatal(err)
				}
			case "app":
				if _, err := f.srv.store.DB().Exec(`UPDATE users SET kind='app' WHERE id=?`, u.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", []string{f.coll.ID}); err != nil {
				t.Fatal(err)
			}
			if who == "app" {
				// A bot reaches the API only through its install, so drive the
				// adapter with the context RequireWorkspaceAccess would build.
				bot, err := f.srv.store.GetUser(u.ID)
				if err != nil || !bot.IsApp() {
					t.Fatalf("app principal: %v %+v", err, bot)
				}
				r := httptest.NewRequest("PATCH", f.path(f.item), nil)
				rc := chi.NewRouteContext()
				rc.URLParams.Add("slug", f.ws.Slug)
				rc.URLParams.Add("itemSlug", f.item.ID)
				ctx := context.WithValue(WithCurrentUser(r.Context(), bot), chi.RouteCtxKey, rc)
				ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, f.ws.ID)
				ctx = context.WithValue(ctx, ctxWorkspaceRole, "owner")
				rr := httptest.NewRecorder()
				if !f.srv.writeCommitmentGraphError(rr, r.WithContext(ctx), store.ErrScheduleLinked) {
					t.Fatal("not handled")
				}
				f.expectErr(rr, 409, "schedule_linked", "")
				if !strings.Contains(rr.Body.String(), hiddenLinkedSentence) || strings.Contains(rr.Body.String(), id) {
					t.Fatalf("app principal refusal: %s", rr.Body.String())
				}
				return
			}
			rr := f.as(f.token(u, ""), "PATCH", f.path(f.item), map[string]any{"dtstart": "2099-02-01T09:00"})
			f.expectErr(rr, 409, "schedule_linked", "")
			want := "{\"error\":{\"code\":\"schedule_linked\",\"details\":{\"remove\":[\"DELETE /api/v1/workspaces/" + f.ws.Slug + "/commitment-links/" + id + "\"]},\"message\":\"" + ownerLinkedSentence + "\"}}\n"
			if rr.Body.String() != want {
				t.Fatalf("owner refusal:\n got %q\nwant %q", rr.Body.String(), want)
			}
			if strings.Contains(rr.Body.String(), f.items[1].ID) {
				t.Fatal("owner refusal names the hidden end")
			}
			rr = f.as(f.token(u, ""), "DELETE", f.resourcePath("links", id), nil)
			if rr.Code != 204 {
				t.Fatalf("offered removal: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
