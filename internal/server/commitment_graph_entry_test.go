package server

import (
	"context"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

type graphHTTP struct {
	*schedHTTP
	items  []*models.Item
	dates  []string
	access models.CommitmentAccess
}

func newGraphHTTP(t *testing.T, roles ...models.CommitmentRolesInput) *graphHTTP {
	f := &graphHTTP{schedHTTP: newSchedHTTP(t), access: models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}}
	for i, r := range roles {
		it := f.item
		if i > 0 {
			it = mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Graph item")
		}
		start := time.Now().UTC().Truncate(time.Second).Add(time.Duration(10+i) * 24 * time.Hour)
		sc, err := f.srv.store.CreateCommitmentSchedule(f.ws.ID, it.ID, models.ScheduleCreate{Dtstart: start.Format("2006-01-02T15:04:05"), Tz: "UTC"}, r)
		if err != nil {
			t.Fatal(err)
		}
		f.items = append(f.items, it)
		f.dates = append(f.dates, *sc.NextAt)
		f.access.Readable[it.ID] = true
		f.access.Editable[it.ID] = true
	}
	return f
}
func graphHard() models.CommitmentRolesInput {
	d, s, b := 2, 0, "stays_owed"
	return models.CommitmentRolesInput{DeadlineDays: &d, DeadlineSecs: &s, DeadlineBehavior: &b}
}
func (f *graphHTTP) link(p, c int, pr, cr, policy string) {
	f.t.Helper()
	in := models.ScheduleLinkCreate{ParentItemID: f.items[p].ID, ParentRole: pr, ChildItemID: f.items[c].ID, ChildRole: cr, Offset: models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 86400}, CancelPolicy: policy, Move: models.LinkWriteOptions{Preview: true, Reason: "Initial link"}}
	pvw, err := f.srv.store.CreateScheduleLink(f.ws.ID, in, f.access)
	if err != nil {
		f.t.Fatal(err)
	}
	in.Move.Preview = false
	in.Move.Revision = pvw.Moved.Revision
	in.Move.AcknowledgeViolations = true
	if _, err := f.srv.store.CreateScheduleLink(f.ws.ID, in, f.access); err != nil {
		f.t.Fatal(err)
	}
}
func (f *graphHTTP) guest(edit ...int) *models.User {
	f.t.Helper()
	u := mustUser(f.t, f.srv, "graph-guest@example.com", "graphguest", "")
	for _, i := range edit {
		if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.items[i].ID, u.ID, "edit", f.owner.ID); err != nil {
			f.t.Fatal(err)
		}
	}
	return u
}
func (f *graphHTTP) reneg(tok string, preview bool, revision string, ack bool) *httptest.ResponseRecorder {
	base, _ := time.Parse(time.RFC3339, f.dates[0])
	to := base.Add(15 * 24 * time.Hour).Format(time.RFC3339)
	return f.as(tok, "POST", f.path(f.item)+"/renegotiate", map[string]any{"deadline": to, "reason": "Replan", "preview": preview, "revision": revision, "acknowledge_violations": ack})
}
func TestTASK4423_R13AccessAndAck(t *testing.T) {
	t.Run("transaction access and owner removal", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
		f.link(0, 1, "target", "target", "")
		co := f.member("graph-coowner@example.com", "owner")
		if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, co.ID, "specific", []string{}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.DB().Exec(`UPDATE items SET deleted_at='2026-01-01T00:00:00Z' WHERE id=?`, f.items[1].ID); err != nil {
			t.Fatal(err)
		}
		for _, u := range []*models.User{co, f.owner} {
			r := httptest.NewRequest("GET", f.path(f.item), nil)
			r = r.WithContext(WithCurrentUser(r.Context(), u))
			live, err := f.srv.buildCommitmentAccess(r, f.srv.store.Q(), f.ws.ID, f.item.ID, commitmentAccessLive)
			if err != nil {
				t.Fatal(err)
			}
			remove, err := f.srv.buildCommitmentAccess(r, f.srv.store.Q(), f.ws.ID, f.item.ID, commitmentAccessRemove)
			if err != nil {
				t.Fatal(err)
			}
			if !live.Editable[f.item.ID] || live.Readable[f.items[1].ID] || !remove.Editable[f.items[1].ID] {
				t.Fatalf("owner live/remove access: %+v %+v", live, remove)
			}
		}
		g := f.guest(0)
		f.srv.store.DB().SetMaxOpenConns(1)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		tx, err := f.srv.store.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		r := httptest.NewRequest("GET", f.path(f.item), nil)
		r = r.WithContext(WithCurrentUser(r.Context(), g))
		a, err := f.srv.buildCommitmentAccess(r, tx, f.ws.ID, f.item.ID, commitmentAccessLive)
		if err != nil || ctx.Err() != nil || !a.Readable[f.item.ID] || !a.Editable[f.item.ID] {
			t.Fatalf("transaction-bound access: %+v %v context=%v", a, err, ctx.Err())
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.buildCommitmentAccess(r, tx, f.ws.ID, f.item.ID, commitmentAccessLive); err == nil {
			t.Fatal("failed access query granted access")
		}
	})

	t.Run("cookie admin role reach", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{})
		u := f.member("graph-admin@example.com", "editor")
		u.Role = "admin"
		if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", []string{}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.item.ID, u.ID, "view", f.owner.ID); err != nil {
			t.Fatal(err)
		}
		for _, bearer := range []bool{false, true} {
			r := httptest.NewRequest("GET", f.path(f.item), nil)
			r = r.WithContext(WithCurrentUser(r.Context(), u))
			if bearer {
				r.Header.Set("Authorization", "Bearer test")
			}
			a, err := f.srv.buildCommitmentAccess(r, f.srv.store.Q(), f.ws.ID, f.item.ID, commitmentAccessLive)
			if err != nil {
				t.Fatal(err)
			}
			if !a.Readable[f.item.ID] || a.Editable[f.item.ID] == bearer {
				t.Fatalf("admin cookie reach, bearer=%v: %+v", bearer, a)
			}
		}
	})

	t.Run("request cannot supply authority", func(t *testing.T) {
		f := newGraphHTTP(t, graphHard())
		for _, field := range []string{"access", "CommitmentAccessLoader", "actor_user_id"} {
			body := map[string]any{"reason": "Replan", "preview": true, "deadline": f.dates[0], field: map[string]any{"Readable": map[string]bool{f.item.ID: true}, "Editable": map[string]bool{f.item.ID: true}}}
			f.expectErr(f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/renegotiate", body), 400, "invalid_body", "")
		}
	})
	t.Run("owner access and acknowledgement", func(t *testing.T) {
		f := newGraphHTTP(t, graphHard(), graphHard())
		f.link(0, 1, "deadline", "target", "")
		tok := f.token(f.owner, "")
		p := f.reneg(tok, true, "", false)
		if p.Code != 200 {
			t.Fatalf("preview: %d %s", p.Code, p.Body.String())
		}
		rev, _ := schedMap(t, p)["revision"].(string)
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(rev) {
			t.Fatalf("revision form %q", rev)
		}
		f.expectErr(f.reneg(tok, false, rev, false), 409, "link_violation_ack", "")
		if rr := f.reneg(tok, false, rev, true); rr.Code != 200 {
			t.Fatalf("ack apply: %d %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("dependent authority", func(t *testing.T) {
		f := newGraphHTTP(t, graphHard(), graphHard())
		f.link(0, 1, "deadline", "deadline", "")
		g := f.guest(0)
		f.expectErr(f.reneg(f.token(g, ""), true, "", false), 409, "deadline_approval_required", "")
	})
	t.Run("revoked dependent grant", func(t *testing.T) {
		f := newGraphHTTP(t, graphHard(), graphHard())
		f.link(0, 1, "deadline", "deadline", "")
		g := f.guest(0, 1)
		tok := f.token(g, "")
		p := f.reneg(tok, true, "", false)
		if p.Code != 200 {
			t.Fatalf("preview: %d %s", p.Code, p.Body.String())
		}
		rev, _ := schedMap(t, p)["revision"].(string)
		if _, err := f.srv.store.DB().Exec(`DELETE FROM item_grants WHERE item_id=? AND user_id=?`, f.items[1].ID, g.ID); err != nil {
			t.Fatal(err)
		}
		before, _ := f.srv.store.GetCommitment(f.ws.ID, f.items[1].ID)
		f.expectErr(f.reneg(tok, false, rev, true), 409, "deadline_approval_required", "")
		after, _ := f.srv.store.GetCommitment(f.ws.ID, f.items[1].ID)
		if *before.Roles.Deadline != *after.Roles.Deadline {
			t.Fatal("revoked grant moved deadline")
		}
	})
}
func TestTASK4423_R14ResolvePreview(t *testing.T) {
	for _, role := range []string{"target", "deadline"} {
		t.Run(role, func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, graphHard())
			f.link(0, 1, "target", role, "")
			base, _ := time.Parse(time.RFC3339, f.dates[0])
			to := base.Add(15 * 24 * time.Hour).Format(time.RFC3339)
			before, _ := f.srv.store.GetCommitment(f.ws.ID, f.items[1].ID)
			journal := f.count(`SELECT COUNT(*) FROM schedule_changes`)
			comments := f.count(`SELECT COUNT(*) FROM comments`)
			var prior string
			for n := 0; n < 2; n++ {
				rr := f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/resolve", map[string]any{"state": "move", "occurrence_at": f.dates[0], "until": to, "comment": "Review this move"})
				d := f.expectErr(rr, 409, "commitment_preview_required", "")
				if d["preview"] != "POST "+f.path(f.item)+"/move-preview" || d["commit"] != "POST "+f.path(f.item)+"/move" {
					t.Fatalf("paths: %s", rr.Body.String())
				}
				if n > 0 && rr.Body.String() != prior {
					t.Fatal("retry bytes changed")
				}
				prior = rr.Body.String()
			}

			if role == "deadline" {
				rr := f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/resolve", map[string]any{"state": "move", "occurrence_at": f.dates[0], "until": to})
				f.expectErr(rr, 409, "commitment_preview_required", "")
				if rr.Body.String() != prior {
					t.Fatal("missing review reason changed preview-required bytes")
				}
			}
			after, _ := f.srv.store.GetCommitment(f.ws.ID, f.items[1].ID)
			if *before.Roles.Target != *after.Roles.Target || *before.Roles.Deadline != *after.Roles.Deadline || f.count(`SELECT COUNT(*) FROM comments`) != comments || f.count(`SELECT COUNT(*) FROM schedule_changes`) != journal {
				t.Fatal("refused resolve wrote state")
			}
			body := map[string]any{"role": "target", "to": to, "reason": "Review this move"}
			preview := f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/move-preview", body)
			if preview.Code != 200 {
				t.Fatalf("follow preview path: %d %s", preview.Code, preview.Body.String())
			}
			planned := schedMap(t, preview)
			body["revision"] = planned["revision"]
			body["acknowledge_violations"] = true
			applied := f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/move", body)
			if applied.Code != 200 {
				t.Fatalf("follow commit path: %d %s", applied.Code, applied.Body.String())
			}
			if !reflect.DeepEqual(planned["changes"], schedMap(t, applied)["changes"]) {
				t.Fatal("applied changes differ from preview")
			}

		})
	}
	t.Run("unlinked", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{})
		rr := f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/resolve", map[string]any{"state": "move", "occurrence_at": f.dates[0], "until": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)})
		if rr.Code != 200 {
			t.Fatalf("unlinked: %d %s", rr.Code, rr.Body.String())
		}
	})
}
func TestTASK4423_R11SkipAuthority(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent guest", true: "owner control"}[owner], func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, graphHard())
			f.link(0, 1, "target", "target", models.LinkCancelChildOccurrence)
			tok := f.token(f.owner, "")
			if !owner {
				tok = f.token(f.guest(0), "")
			}

			if !owner {
				f.srv.store.DB().SetMaxOpenConns(1)
				for _, q := range []string{
					`CREATE TEMP TABLE graph_parent_schedule AS SELECT * FROM item_schedules WHERE item_id='` + f.item.ID + `'`,
					`CREATE TEMP TABLE graph_parent_reminder AS SELECT * FROM item_reminders WHERE item_id='` + f.item.ID + `'`,
					`CREATE TRIGGER graph_response_clock AFTER UPDATE ON item_schedules WHEN new.item_id='` + f.item.ID + `' BEGIN UPDATE item_schedules SET updated_at='2026-01-01T00:00:00Z',ended_at=CASE WHEN ended_at IS NULL THEN NULL ELSE '2026-01-01T00:00:00Z' END WHERE id=new.id; END`,
				} {
					if _, err := f.srv.store.DB().Exec(q); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := f.count(`SELECT COUNT(*) FROM item_reminders WHERE item_id=? AND fired_at IS NULL`, f.items[1].ID)
			rr := f.as(tok, "POST", f.path(f.item)+"/resolve", map[string]any{"state": "skip", "occurrence_at": f.dates[0]})
			if rr.Code != 200 {
				t.Fatalf("skip: %d %s", rr.Code, rr.Body.String())
			}
			marks := f.count(`SELECT COUNT(*) FROM schedule_link_overrides WHERE no_longer_needed_at IS NOT NULL`)
			if owner && marks != 1 {
				t.Fatalf("owner access was not passed: marks=%d", marks)
			}
			if !owner {
				if marks != 0 || before != f.count(`SELECT COUNT(*) FROM item_reminders WHERE item_id=? AND fired_at IS NULL`, f.items[1].ID) {
					t.Fatal("unauthorized skip touched child")
				}
				if strings.Contains(rr.Body.String(), f.items[1].ID) {
					t.Fatal("hidden child disclosed")
				}
				for _, q := range []string{
					`DELETE FROM schedule_links`,
					`DELETE FROM item_reminders WHERE item_id='` + f.item.ID + `'`,
					`DELETE FROM schedule_occurrences WHERE schedule_id=(SELECT id FROM graph_parent_schedule)`,
					`DELETE FROM item_schedules WHERE item_id='` + f.item.ID + `'`,
					`INSERT INTO item_schedules SELECT * FROM graph_parent_schedule`,
					`INSERT INTO item_reminders SELECT * FROM graph_parent_reminder`,
				} {
					if _, err := f.srv.store.DB().Exec(q); err != nil {
						t.Fatal(err)
					}
				}
				without := f.as(tok, "POST", f.path(f.item)+"/resolve", map[string]any{"state": "skip", "occurrence_at": f.dates[0]})
				if without.Code != rr.Code || without.Body.String() != rr.Body.String() {
					t.Fatalf("hidden child changed response bytes:\n%s\n%s", rr.Body.String(), without.Body.String())
				}

			}
		})
	}
}
