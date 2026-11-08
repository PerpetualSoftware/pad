package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestCommitmentPartialRolesHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, roles, field, code string
		status                   int
		deadline                 bool
	}{
		{"occupied keeps deadline", `{"occupied_days":0,"occupied_secs":7200}`, "", "", 200, true},
		{"occupied window", `{"occupied_days":0,"occupied_secs":7200}`, "", "", 200, false},
		{"add deadline", `{"deadline_days":2,"deadline_secs":0,"deadline_behavior":"lapses"}`, "", "", 200, false},
		{"clear earliest", `{"earliest_days":null,"earliest_secs":null}`, "", "", 200, false},
		{"half earliest", `{"earliest_days":null}`, "earliest", "combination", 400, false},
		{"half occupied", `{"occupied_secs":60}`, "occupied_until", "combination", 400, false},
		{"clear deadline", `{"deadline_days":null,"deadline_secs":null,"deadline_behavior":null}`, "deadline", "combination", 409, true},
		{"behavior only", `{"deadline_behavior":"stays_owed"}`, "deadline", "combination", 409, true},
		{"null target days", `{"target_days":null}`, "target_days", "required", 400, false},
		{"null target secs", `{"target_secs":null}`, "target_secs", "required", 400, false},
		{"null outcome", `{"outcome":null}`, "outcome", "required", 400, false},
		{"clear occupied", `{"occupied_days":null,"occupied_secs":null}`, "", "", 200, false},
		{"half occupied clear", `{"occupied_secs":null}`, "occupied_until", "combination", 400, false},
		{"half deadline", `{"deadline_days":null}`, "deadline", "combination", 400, true},
		{"missing behavior", `{"deadline_days":2,"deadline_secs":0}`, "deadline_behavior", "combination", 400, false},
		{"range", `{"target_secs":-2147483649}`, "roles", "range", 400, false},
		{"enum", `{"deadline_behavior":"bad"}`, "deadline_behavior", "enum", 400, false},
		{"wrong member type", `{"target_days":"1"}`, "", "", 400, false},
		{"wrong behavior type", `{"deadline_behavior":3}`, "", "", 400, false},
		{"wrong outcome type", `{"outcome":[]}`, "", "", 400, false},
		{"empty", `{}`, "roles", "required", 400, false},
		{"null", `null`, "roles", "required", 400, false},
		{"unknown", `{"bogus":0}`, "", "", 400, false},
		{"repeated", `{"target_days":1,"target_days":2}`, "", "", 400, false},
		{"array", `[]`, "", "", 400, false},
		{"string", `"roles"`, "", "", 400, false},
		{"number", `7`, "", "", 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedHTTP(t)
			d, s, e, es, b := 2, 0, -1, 60, "lapses"
			r := models.CommitmentRolesInput{TargetDays: 1, TargetSecs: 3600, EarliestDays: &e, EarliestSecs: &es, Outcome: "delivered"}
			if tc.deadline {
				r.DeadlineDays, r.DeadlineSecs, r.DeadlineBehavior = &d, &s, &b
			}
			_, err := f.srv.store.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, r)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(tc.name, "occupied clear") || tc.name == "clear occupied" {
				if _, err := f.srv.store.DB().Exec(`UPDATE item_schedules SET occupied_days=0,occupied_secs=7200,kind='window',dtend='2099-01-01T11:00:00' WHERE item_id=?`, f.item.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := commitmentSnapshot(t, f)
			req := httptest.NewRequest("PATCH", commitmentPath(f), strings.NewReader(`{"roles":`+tc.roles+`}`))
			req.Header.Set("Authorization", "Bearer "+f.token(f.owner, ""))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status %d want %d: %s", rr.Code, tc.status, rr.Body.String())
			}
			if tc.status != 200 {
				code := "invalid_body"
				if tc.field != "" {
					code = "validation_error"
				}
				if tc.status == 409 {
					code = "hard_deadline"
				}
				details := f.expectErr(rr, tc.status, code, tc.code)
				if tc.field != "" && details["field"] != tc.field {
					t.Fatalf("field %v", details)
				}
				if before != commitmentSnapshot(t, f) {
					t.Fatal("refusal wrote")
				}
				return
			}
			var out struct{ Roles models.CommitmentRoles }
			if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Roles.Outcome != "delivered" || out.Roles.Target == nil || *out.Roles.Target != "2099-01-02T10:00:00Z" {
				t.Fatalf("lost roles: %s", rr.Body.String())
			}
			if tc.name != "clear earliest" && (out.Roles.Earliest == nil || *out.Roles.Earliest != "2098-12-31T09:01:00Z") {
				t.Fatalf("lost earliest: %s", rr.Body.String())
			}
			if tc.deadline && (out.Roles.DeadlineBehavior == nil || *out.Roles.DeadlineBehavior != "lapses") {
				t.Fatal("lost deadline")
			}
		})
	}
}

func TestCommitmentPostRolesRemainFull(t *testing.T) {
	f := newSchedHTTP(t)
	rr := f.as(f.token(f.owner, ""), "POST", commitmentPath(f), map[string]any{"dtstart": "2099-01-01T09:00", "tz": "UTC", "roles": map[string]any{"target_days": 1, "target_secs": 3600}})
	c := readCommitment(t, rr, 201)
	if c.Roles.Deadline != nil {
		t.Fatal("omitted deadline is present")
	}
}

func TestCommitmentPartialRolesHTTPLegacyAndNoop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   map[string]any
		status int
		native bool
	}{
		{"empty legacy", map[string]any{"roles": map[string]any{}}, 400, false},
		{"timing legacy", map[string]any{"dtstart": "2099-01-02T09:00"}, 200, false},
		{"convert legacy", map[string]any{"roles": map[string]any{"target_secs": 60}}, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := timingFixture(t, "legacy_backfill", true)
			before := commitmentSnapshot(t, f)
			rr := f.as(f.token(f.owner, ""), "PATCH", commitmentPath(f), tc.body)
			if rr.Code != tc.status {
				t.Fatalf("%d %s", rr.Code, rr.Body.String())
			}
			if tc.status == 400 {
				d := f.expectErr(rr, 400, "validation_error", "required")
				if d["field"] != "roles" || commitmentSnapshot(t, f) != before {
					t.Fatal("empty converted")
				}
				return
			}
			c := readCommitment(t, rr, 200)
			want := "legacy_backfill"
			if tc.native {
				want = "native"
			}
			if c.Roles.RolesOrigin != want {
				t.Fatalf("origin %s", c.Roles.RolesOrigin)
			}
			if tc.native {
				var n int
				err := f.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND before_json LIKE '%legacy_backfill%' AND after_json LIKE '%native%'`, c.ScheduleID).Scan(&n)
				if err != nil || n != 1 {
					t.Fatalf("conversion marker n=%d %v", n, err)
				}
			}
		})
	}
	t.Run("noop", func(t *testing.T) {
		f := timingFixture(t, "native", false)
		before := commitmentSnapshot(t, f)
		rr := f.as(f.token(f.owner, ""), "PATCH", commitmentPath(f), map[string]any{"roles": map[string]any{"target_secs": 0}})
		readCommitment(t, rr, 200)
		if before != commitmentSnapshot(t, f) {
			t.Fatal("no-op wrote")
		}
	})
	t.Run("new start and occupied", func(t *testing.T) {
		f := timingFixture(t, "native", false)
		rr := f.as(f.token(f.owner, ""), "PATCH", commitmentPath(f), map[string]any{"dtstart": "2099-01-02T13:00", "roles": map[string]any{"occupied_days": 0, "occupied_secs": 7200}})
		c := readCommitment(t, rr, 200)
		if c.Roles.OccupiedUntil == nil || *c.Roles.OccupiedUntil != "2099-01-02T15:00:00Z" {
			t.Fatalf("new start %s", rr.Body.String())
		}
	})
}
