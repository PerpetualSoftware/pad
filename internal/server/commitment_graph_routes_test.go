package server

import (
	"encoding/json"
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
	"time"
)

func TestTASK4423_R08Wire(t *testing.T) {
	t.Run("ended occurrence field contract", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{})
		if _, err := f.srv.store.DB().Exec(`UPDATE item_schedules SET ended_at='2026-01-01T00:00:00Z',next_at=NULL,next_wall=NULL WHERE item_id=?`, f.item.ID); err != nil {
			t.Fatal(err)
		}
		f.expectErr(f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/move", map[string]any{"role": "target", "to": f.dates[0]}), 400, "validation_error", "combination")
	})

	t.Run("routes and preview purity", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
		tok := f.token(f.owner, "")
		path := f.path(f.item)
		body := map[string]any{"child": f.items[1].ID, "parent_role": "target", "child_role": "target", "offset": map[string]any{"mode": "elapsed", "elapsed_secs": 0}, "preview": true}
		rr := f.as(tok, "POST", path+"/links", body)
		if rr.Code != 200 {
			t.Fatalf("link preview: %d %s", rr.Code, rr.Body.String())
		}
		m := schedMap(t, rr)
		if m["link"] != nil || m["moved"] == nil {
			t.Fatalf("LinkWrite: %s", rr.Body.String())
		}
		body["preview"] = false
		rr = f.as(tok, "POST", path+"/links", body)
		if rr.Code != 201 {
			t.Fatalf("create link: %d %s", rr.Code, rr.Body.String())
		}
		link := schedMap(t, rr)["link"].(map[string]any)
		for _, key := range []string{"id", "parent_item_id", "parent_role", "child_item_id", "child_role", "offset", "cancel_policy", "created_at", "updated_at"} {
			if _, ok := link[key]; !ok {
				t.Fatalf("missing Link.%s", key)
			}
		}
		if link["cancel_policy"] != "waiting_on" {
			t.Fatal("default policy")
		}
		id := link["id"].(string)
		if rr = f.as(tok, "GET", path+"/links", nil); rr.Code != 200 || len(schedMap(t, rr)["links"].([]any)) != 1 {
			t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
		}
		if rr = f.as(tok, "PATCH", f.resourcePath("links", id), map[string]any{"cancel_policy": "cancel_child_occurrence"}); rr.Code != 200 {
			t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
		}
		base, _ := time.Parse(time.RFC3339, f.dates[0])
		to := base.Add(3 * time.Hour).Format(time.RFC3339)
		move := map[string]any{"role": "target", "to": to, "reason": "Move"}
		journal, notices := f.count(`SELECT COUNT(*) FROM schedule_changes`), f.count(`SELECT COUNT(*) FROM user_notifications`)
		before, _ := f.srv.store.GetCommitment(f.ws.ID, f.item.ID)
		rr = f.as(tok, "POST", path+"/move-preview", move)
		if rr.Code != 200 {
			t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
		}
		m = schedMap(t, rr)
		if m["revision"] == nil || m["changes"] == nil {
			t.Fatalf("Move: %s", rr.Body.String())
		}
		after, _ := f.srv.store.GetCommitment(f.ws.ID, f.item.ID)
		if *before.Roles.Target != *after.Roles.Target || journal != f.count(`SELECT COUNT(*) FROM schedule_changes`) || notices != f.count(`SELECT COUNT(*) FROM user_notifications`) {
			t.Fatal("preview wrote rows")
		}
		rr = f.as(tok, "POST", path+"/move", move)
		if rr.Code != 200 {
			t.Fatalf("move: %d %s", rr.Code, rr.Body.String())
		}
		after, _ = f.srv.store.GetCommitment(f.ws.ID, f.item.ID)
		if *after.Roles.Target != to {
			t.Fatal("move not committed")
		}
		if rr = f.as(tok, "DELETE", f.resourcePath("links", id), nil); rr.Code != 204 || rr.Body.Len() != 0 {
			t.Fatalf("delete link: %d %s", rr.Code, rr.Body.String())
		}
		bare := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "No schedule")
		gatePath := f.path(bare) + "/gates"
		rr = f.as(tok, "POST", gatePath, map[string]any{"blocker": f.items[1].ID})
		if rr.Code != 201 {
			t.Fatalf("gate: %d %s", rr.Code, rr.Body.String())
		}
		gate := schedMap(t, rr)
		for _, key := range []string{"id", "blocked_item_id", "blocker_item_id", "outcome", "created_at"} {
			if _, ok := gate[key]; !ok {
				t.Fatalf("missing Gate.%s", key)
			}
		}
		rr = f.as(tok, "GET", gatePath, nil)
		if rr.Code != 200 || schedMap(t, rr)["state"] != "waiting" {
			t.Fatalf("no schedule GateView: %d %s", rr.Code, rr.Body.String())
		}
		if rr = f.as(tok, "DELETE", f.resourcePath("gates", gate["id"].(string)), nil); rr.Code != 204 || rr.Body.Len() != 0 {
			t.Fatalf("delete gate: %d %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("strict bodies", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
		tok := f.token(f.owner, "")
		for _, route := range []string{"links", "gates", "move", "move-preview"} {
			for _, raw := range []string{`{"unknown":1}`, `{"role":"target","role":"target"}`, `{"preview":"yes"}`, `[]`, `null`, `{"reason":"\u0000"}`} {
				f.expectErr(f.as(tok, "POST", f.path(f.item)+"/"+route, raw), 400, "invalid_body", "")
			}
		}
		for _, field := range []string{"child", "parent_role", "child_role", "offset"} {
			body := map[string]any{"child": f.items[1].ID, "parent_role": "target", "child_role": "target", "offset": map[string]any{"mode": "elapsed", "elapsed_secs": 0}}
			delete(body, field)
			d := f.expectErr(f.as(tok, "POST", f.path(f.item)+"/links", body), 400, "validation_error", "required")
			if d["field"] != field || len(d) != 2 {
				t.Fatalf("details=%v", d)
			}
		}
		for _, off := range []string{`{"mode":"elapsed","elapsed_secs":0,"days":0}`, `{"mode":"civil","days":0,"clock_secs":0,"all_day":false,"zone":"UTC","elapsed_secs":0}`} {
			var offset any
			if err := json.Unmarshal([]byte(off), &offset); err != nil {
				t.Fatal(err)
			}
			f.expectErr(f.as(tok, "POST", f.path(f.item)+"/links", map[string]any{"child": f.items[1].ID, "parent_role": "target", "child_role": "target", "offset": offset}), 400, "validation_error", "combination")
		}
		for _, tc := range []struct{ route, raw, field, code string }{
			{"gates", `{}`, "blocker", "required"},
			{"move", `{"to":"2030-01-01T00:00:00Z"}`, "role", "required"},
			{"move-preview", `{"role":"target"}`, "to", "required"},
			{"move", `{"role":"unknown","to":"2030-01-01T00:00:00Z"}`, "role", "enum"},
			{"move", `{"role":"target","to":"tomorrow"}`, "to", "format"},
			{"move", `{"role":"target","to":"2030-01-01T00:00:00Z","revision":"invalid"}`, "revision", "format"},
		} {
			d := f.expectErr(f.as(tok, "POST", f.path(f.item)+"/"+tc.route, tc.raw), 400, "validation_error", tc.code)
			if d["field"] != tc.field || len(d) != 2 {
				t.Fatalf("details=%v", d)
			}
		}
		for _, raw := range []string{`{"role":"target","role":"target","to":"2030-01-01T00:00:00Z"}`, `{"role":"target","to":"2030-01-01T00:00:00Z","revision":"opaque"}`} {
			f.expectErr(f.as(tok, "POST", f.path(f.item)+"/move-preview", raw), 400, "invalid_body", "")
		}

	})
}
func TestTASK4423_R13MoveAck(t *testing.T) {
	t.Run("ack and revoked permission", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{}, graphHard())
		f.link(0, 1, "target", "target", "")
		g := f.guest(0, 1)
		tok := f.token(g, "")
		to := time.Now().UTC().Truncate(time.Second).Add(30 * 24 * time.Hour).Format(time.RFC3339)
		body := map[string]any{"role": "target", "to": to, "reason": "Replan"}
		rr := f.as(tok, "POST", f.path(f.item)+"/move-preview", body)
		if rr.Code != 200 {
			t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
		}
		f.expectErr(f.as(tok, "POST", f.path(f.item)+"/move", body), 400, "validation_error", "required")
		body["revision"] = schedMap(t, rr)["revision"]
		f.expectErr(f.as(tok, "POST", f.path(f.item)+"/move", body), 409, "link_violation_ack", "")
		body["acknowledge_violations"] = true
		if rr = f.as(tok, "POST", f.path(f.item)+"/move", body); rr.Code != 200 {
			t.Fatalf("apply: %d %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("deadline grant revoked", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{}, graphHard())
		f.link(0, 1, "target", "deadline", "")
		g := f.guest(0, 1)
		tok := f.token(g, "")
		body := map[string]any{"role": "target", "to": time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339), "reason": "Replan"}
		rr := f.as(tok, "POST", f.path(f.item)+"/move-preview", body)
		if rr.Code != 200 {
			t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
		}
		body["revision"] = schedMap(t, rr)["revision"]
		body["acknowledge_violations"] = true
		if _, err := f.srv.store.DB().Exec(`DELETE FROM item_grants WHERE item_id=? AND user_id=?`, f.items[1].ID, g.ID); err != nil {
			t.Fatal(err)
		}
		before, _ := f.srv.store.GetCommitment(f.ws.ID, f.items[1].ID)
		f.expectErr(f.as(tok, "POST", f.path(f.item)+"/move", body), 409, "deadline_approval_required", "")
		after, _ := f.srv.store.GetCommitment(f.ws.ID, f.items[1].ID)
		if *before.Roles.Deadline != *after.Roles.Deadline {
			t.Fatal("revoked grant moved deadline")
		}
	})
	t.Run("root deadline assignee guard", func(t *testing.T) {
		f := newGraphHTTP(t, graphHard())
		g := f.guest(0)
		restore := f.srv.store.SetBeforeCommitmentPinHookForTesting(func() {
			if _, err := f.srv.store.DB().Exec(`UPDATE items SET assigned_user_id=? WHERE id=?`, f.owner.ID, f.item.ID); err != nil {
				t.Fatal(err)
			}
		})
		defer restore()
		before := f.count(`SELECT COUNT(*) FROM schedule_changes`)
		f.expectErr(f.as(f.token(g, ""), "POST", f.path(f.item)+"/move-preview", map[string]any{"role": "deadline", "to": f.dates[0], "reason": "Replan"}), 409, "commitment_changed", "stale")
		if f.count(`SELECT COUNT(*) FROM schedule_changes`) != before {
			t.Fatal("stale assignee moved deadline")
		}
	})

	t.Run("root deadline authority", func(t *testing.T) {
		f := newGraphHTTP(t, graphHard())
		g := f.guest(0)
		if _, err := f.srv.store.DB().Exec(`UPDATE items SET assigned_user_id=? WHERE id=?`, f.owner.ID, f.item.ID); err != nil {
			t.Fatal(err)
		}
		f.expectErr(f.as(f.token(g, ""), "POST", f.path(f.item)+"/move", map[string]any{"role": "deadline", "to": f.dates[0], "reason": "Replan"}), 403, "deadline_owner_required", "authority")
	})
}
