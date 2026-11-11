package server

import (
	"fmt"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/google/uuid"
)

const graphMissingBody = "{\"error\":{\"code\":\"schedule_link_not_found\",\"message\":\"Commitment link or gate not found\"}}\n"

func (f *graphHTTP) resourceID(kind string) string {
	f.t.Helper()
	var id string
	if err := f.srv.store.DB().QueryRow("SELECT id FROM schedule_" + kind + " LIMIT 1").Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f *graphHTTP) resourcePath(kind, id string) string {
	return "/api/v1/workspaces/" + f.ws.Slug + "/commitment-" + kind + "/" + id
}
func (f *graphHTTP) graphResource(kind string) string {
	f.t.Helper()
	if kind == "links" {
		f.link(0, 1, "target", "target", "")
	} else if _, err := f.srv.store.CreateScheduleGate(f.ws.ID, f.items[0].ID, f.items[1].ID, f.access); err != nil {
		f.t.Fatal(err)
	}
	return f.resourceID(kind)
}
func TestTASK4423_R02OwnerUnlink(t *testing.T) {
	for _, kind := range []string{"links", "gates"} {
		for _, owner := range []string{"workspace", "member"} {
			for _, deleted := range []int{-1, 0, 1} {
				t.Run(fmt.Sprintf("%s/%s/deleted%d", kind, owner, deleted), func(t *testing.T) {
					f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
					hidden := mustCollection(t, f.srv, f.ws.ID, "Hidden")
					if _, err := f.srv.store.DB().Exec(`UPDATE items SET collection_id=? WHERE id=?`, hidden.ID, f.items[1].ID); err != nil {
						t.Fatal(err)
					}
					id := f.graphResource(kind)
					u := f.member("unlink-owner@example.com", "owner")
					if owner == "workspace" {
						u = f.owner
						if _, err := f.srv.store.DB().Exec(`UPDATE workspace_members SET role='editor' WHERE workspace_id=? AND user_id=?`, f.ws.ID, u.ID); err != nil {
							t.Fatal(err)
						}
					}
					if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", []string{f.coll.ID}); err != nil {
						t.Fatal(err)
					}
					if deleted >= 0 {
						if _, err := f.srv.store.DB().Exec(`UPDATE items SET deleted_at='2026-01-01T00:00:00Z' WHERE id=?`, f.items[deleted].ID); err != nil {
							t.Fatal(err)
						}
					}
					rr := f.as(f.token(u, ""), "DELETE", f.resourcePath(kind, id), nil)
					if rr.Code != 204 || rr.Body.Len() != 0 {
						t.Fatalf("owner remove: %d %s", rr.Code, rr.Body.String())
					}
					if n := f.count("SELECT COUNT(*) FROM schedule_"+kind+" WHERE id=?", id); n != 0 {
						t.Fatal("resource retained")
					}
				})
			}
		}
	}
}
func TestTASK4423_R07Doors(t *testing.T) {
	t.Run("disappearing under pin", testGraphResourceDisappears)
	for _, kind := range []string{"links", "gates"} {
		for _, state := range []string{"hidden", "deleted", "foreign", "missing"} {
			for _, held := range []int{0, 1} {
				t.Run(fmt.Sprintf("%s/%s/held%d", kind, state, held), func(t *testing.T) {
					f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
					id := f.graphResource(kind)
					g := f.guest(held)
					tok := f.token(g, "")
					other := 1 - held
					if state == "deleted" {
						if _, err := f.srv.store.DB().Exec(`UPDATE items SET deleted_at='2026-01-01T00:00:00Z' WHERE id=?`, f.items[other].ID); err != nil {
							t.Fatal(err)
						}
					}
					ref := f.items[1].ID
					if state == "foreign" {
						ws := mustWorkspace(t, f.srv, "Foreign", f.owner.ID)
						c := mustCollection(t, f.srv, ws.ID, "Foreign tasks")
						it := mustItem(t, f.srv, ws.ID, c.ID, "Foreign")
						ref = it.ID
					}
					if state == "missing" {
						ref = uuid.NewString()
					}
					body := map[string]any{"blocker": ref}
					if kind == "links" {
						body = map[string]any{"child": ref, "parent_role": "target", "child_role": "target", "offset": map[string]any{"mode": "elapsed", "elapsed_secs": 0}}
					}
					requests := []struct {
						method, path string
						body         any
					}{{"POST", f.path(f.item) + "/" + kind, body}, {"DELETE", f.resourcePath(kind, id), nil}, {"DELETE", f.resourcePath(kind, "malformed"), nil}, {"DELETE", f.resourcePath(kind, uuid.NewString()), nil}}
					if kind == "links" {
						requests = append(requests, struct {
							method, path string
							body         any
						}{"PATCH", f.resourcePath(kind, id), map[string]any{"cancel_policy": "waiting_on"}})
					}
					for _, req := range requests {
						rr := f.as(tok, req.method, req.path, req.body)
						if rr.Code != 404 || rr.Body.String() != graphMissingBody {
							t.Fatalf("%s %s: %d %q", req.method, req.path, rr.Code, rr.Body.String())
						}
					}
				})
			}
		}
	}
	for _, kind := range []string{"links", "gates"} {
		t.Run(kind+"/read-both-edit-one", func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
			id := f.graphResource(kind)
			g := f.guest(0)
			if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.items[1].ID, g.ID, "view", f.owner.ID); err != nil {
				t.Fatal(err)
			}
			f.expectErr(f.as(f.token(g, ""), "DELETE", f.resourcePath(kind, id), nil), 403, "link_edit_required", "")
		})
	}
}

// The workspace pin is held by a separate transaction. Delete the resource
// while the request waits, then verify the handler does not use its stale read.
func testGraphResourceDisappears(t *testing.T) {
	srv, _ := testServerPostgres(t)
	f := &graphHTTP{schedHTTP: newSchedHTTPOn(t, srv), access: models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}}
	for i := 0; i < 2; i++ {
		it := f.item
		if i > 0 {
			it = mustItem(t, srv, f.ws.ID, f.coll.ID, "Other")
		}
		sc, err := srv.store.CreateCommitmentSchedule(f.ws.ID, it.ID, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"}, models.CommitmentRolesInput{})
		if err != nil {
			t.Fatal(err)
		}
		f.items = append(f.items, it)
		f.dates = append(f.dates, *sc.NextAt)
		f.access.Readable[it.ID] = true
		f.access.Editable[it.ID] = true
	}
	tok := f.token(f.owner, "")
	for _, kind := range []string{"links", "gates"} {
		for _, method := range []string{"DELETE", "PATCH"} {
			if kind == "gates" && method == "PATCH" {
				continue
			}
			t.Run(kind+"/"+method, func(t *testing.T) {
				id := f.graphResource(kind)
				tx, err := srv.store.DB().Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var locked string
				if err := tx.QueryRow(`SELECT id FROM workspaces WHERE id=$1 FOR NO KEY UPDATE`, f.ws.ID).Scan(&locked); err != nil {
					t.Fatal(err)
				}
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					var body any
					if method == "PATCH" {
						body = map[string]any{"cancel_policy": "waiting_on"}
					}
					done <- f.as(tok, method, f.resourcePath(kind, id), body)
				}()
				deadline := time.Now().Add(10 * time.Second)
				for {
					var n int
					if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active' AND wait_event_type='Lock'`).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n > 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("request did not wait for pin")
					}
					runtime.Gosched()
				}
				if _, err := tx.Exec(`DELETE FROM schedule_`+kind+` WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				select {
				case rr := <-done:
					if rr.Code != 404 || rr.Body.String() != graphMissingBody {
						t.Fatalf("disappearing %s: %d %q", kind, rr.Code, rr.Body.String())
					}
				case <-time.After(15 * time.Second):
					t.Fatal("request did not finish")
				}
			})
		}
	}
}
