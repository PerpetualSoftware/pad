package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The snapshot includes the complete row, journal and managed reminder, so a
// refusal cannot hide a partial roles, timing or reminder write.
func commitmentSnapshot(t *testing.T, f *schedHTTP) string {
	t.Helper()
	out := []any{}
	for _, table := range []string{"item_schedules", "schedule_changes", "item_reminders", "schedule_occurrence_overrides"} {
		rows, err := f.srv.store.DB().Query("SELECT * FROM " + table + " ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		data := [][]any{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for j := range vals {
				ptrs[j] = &vals[j]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			data = append(data, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		out = append(out, data)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func timingFixture(t *testing.T, origin string, deadline bool) *schedHTTP {
	t.Helper()
	f := newSchedHTTP(t)
	in := models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}
	var err error
	if origin == "legacy_backfill" {
		in.Kind = "deadline_hard"
		_, err = f.srv.store.CreateSchedule(f.ws.ID, f.item.ID, in)
	} else {
		roles := models.CommitmentRolesInput{}
		if deadline {
			d, s, b := 1, 0, "stays_owed"
			roles.DeadlineDays = &d
			roles.DeadlineSecs = &s
			roles.DeadlineBehavior = &b
		}
		_, err = f.srv.store.CreateCommitmentSchedule(f.ws.ID, f.item.ID, in, roles)
	}
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCommitmentTimingMatrix(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"native deadline", "native check", "legacy_backfill"} {
		for _, actor := range []string{"owner", "assignee"} {
			for _, tc := range []struct {
				name        string
				value, want any
			}{
				{"dtstart", "2099-01-02T09:00", "2099-01-02T09:00:00"},
				{"tz", "Europe/Paris", "Europe/Paris"},
				{"floating", true, true},
				{"rrule", "FREQ=WEEKLY", "FREQ=WEEKLY"},
				{"anchor", "completion", "completion"},
				{"rdates", []string{"2099-01-02T10:00"}, []any{"2099-01-02T10:00:00"}},
				{"exdates", []string{"2099-01-02T09:00"}, []any{"2099-01-02T09:00:00"}},
			} {
				t.Run(origin+"/"+actor+"/"+tc.name, func(t *testing.T) {
					f := timingFixture(t, strings.Fields(origin)[0], origin == "native deadline")
					user := f.owner
					if actor == "assignee" {
						user = f.member("timing-assignee@example.com", "editor")
						if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &user.ID}); err != nil {
							t.Fatal(err)
						}
					}
					before := commitmentSnapshot(t, f)
					rr := f.as(f.token(user, ""), "PATCH", commitmentPath(f), map[string]any{tc.name: tc.value})
					if origin == "native deadline" {
						d := f.expectErr(rr, 409, "hard_deadline", "combination")
						if d["field"] != "deadline" || !strings.Contains(rr.Body.String(), "POST .../commitment/renegotiate") {
							t.Fatalf("refusal: %s", rr.Body.String())
						}
						if commitmentSnapshot(t, f) != before {
							t.Fatal("refusal wrote")
						}
					} else {
						readCommitment(t, rr, 200)
						if got := schedMap(t, rr)[tc.name]; !reflect.DeepEqual(got, tc.want) {
							t.Fatalf("%s=%v want %v", tc.name, got, tc.want)
						}
						if commitmentSnapshot(t, f) == before {
							t.Fatal("edit did not apply")
						}
					}
				})
			}
		}
	}
}

func TestCommitmentTimingExceptionsAndAtomicity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"audience", map[string]any{"audience": "human"}, 200},
		{"lead", map[string]any{"lead_minutes": 30}, 200},
		{"noop", map[string]any{"dtstart": "2099-01-01T09:00"}, 200},
		{"invalid roles", map[string]any{"dtstart": "2099-01-02T09:00", "roles": map[string]any{"earliest_days": 1}}, 400},
		{"compensating offset", map[string]any{"dtstart": "2099-01-02T09:00", "roles": map[string]any{"deadline_days": 0, "deadline_secs": 0, "deadline_behavior": "stays_owed"}}, 409},
		{"ended", map[string]any{"rrule": "FREQ=WEEKLY"}, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := timingFixture(t, "native", true)
			if tc.name == "ended" {
				if _, err := f.srv.store.DB().Exec("UPDATE item_schedules SET ended_at='2098-01-01T00:00:00Z',next_at=NULL,next_wall=NULL WHERE item_id=?", f.item.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := commitmentSnapshot(t, f)
			rr := commitmentCall(f, "PATCH", tc.body)
			if tc.want == 409 {
				f.expectErr(rr, 409, "hard_deadline", "combination")
			} else if tc.want == 400 {
				f.expectErr(rr, 400, "validation_error", "combination")
			} else {
				readCommitment(t, rr, 200)
			}
			if (tc.want != 200 || tc.name == "noop") && before != commitmentSnapshot(t, f) {
				t.Fatal("refusal or no-op wrote")
			}
		})
	}
	f := timingFixture(t, "native", false)
	rr := commitmentCall(f, "PATCH", map[string]any{"dtstart": "2099-01-02T09:00", "roles": map[string]any{"deadline_days": 1, "deadline_secs": 0, "deadline_behavior": "stays_owed"}})
	c := readCommitment(t, rr, 200)
	if c.Roles.Deadline == nil || *c.Roles.Deadline != "2099-01-03T09:00:00Z" {
		t.Fatalf("first deadline: %+v", c)
	}
}

func TestCommitmentCreateContractAndResponse(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"POST", "PATCH"} {
		for _, field := range []string{"kind", "dtend", "Kind", "DTEND"} {
			t.Run(method+"/"+field, func(t *testing.T) {
				f := newSchedHTTP(t)
				body := map[string]any{"dtstart": "2099-01-01T09:00", "roles": map[string]any{}, field: nil}
				before := commitmentSnapshot(t, f)
				d := f.expectErr(commitmentCall(f, method, body), 400, "validation_error", "not_accepted")
				if d["field"] != field {
					t.Fatalf("field: %v", d)
				}
				if before != commitmentSnapshot(t, f) {
					t.Fatal("forbidden member stored")
				}
				f.nothingStored()
			})
		}
	}
	f := newSchedHTTP(t)
	d := f.expectErr(commitmentCall(f, "POST", map[string]any{"dtstart": "2099-01-01T09:00"}), 400, "validation_error", "required")
	if d["field"] != "roles" {
		t.Fatal(d)
	}
	for _, method := range []string{"POST", "GET", "PATCH"} {
		var body any
		if method == "POST" {
			body = map[string]any{"dtstart": "2099-01-01T09:00", "roles": map[string]any{}}
		}
		if method == "PATCH" {
			body = map[string]any{"audience": "human"}
		}
		rr := commitmentCall(f, method, body)
		status := 200
		if method == "POST" {
			status = 201
		}
		readCommitment(t, rr, status)
		m := schedMap(t, rr)
		for _, key := range []string{"dtstart", "tz", "floating", "rdates", "exdates", "anchor", "audience", "lead_minutes", "next_occurrence", "reminder_id", "created_at", "updated_at"} {
			if _, ok := m[key]; !ok {
				t.Errorf("%s missing %s", method, key)
			}
		}
		for _, key := range []string{"kind", "dtend"} {
			if _, ok := m[key]; ok {
				t.Errorf("%s exposes %s", method, key)
			}
		}
	}
}

func TestCommitmentOccupiedValidationAtEveryDoor(t *testing.T) {
	t.Parallel()
	for _, door := range []string{"create", "roles", "timing", "renegotiate", "renegotiate occurrence"} {
		for _, tc := range []struct {
			name, start string
			days, secs  int
		}{
			{"zero", "2099-01-01T09:00", 0, 0}, {"negative", "2099-01-01T09:00", 0, -1}, {"all day seconds", "2099-01-01", 1, 1},
		} {
			t.Run(door+"/"+tc.name, func(t *testing.T) {
				f := newSchedHTTP(t)
				roles := map[string]any{"occupied_days": tc.days, "occupied_secs": tc.secs}
				method, path, body := "POST", commitmentPath(f), map[string]any{"dtstart": tc.start, "roles": roles}
				if door != "create" {
					in := models.ScheduleCreate{Kind: "check", Dtstart: tc.start, Tz: "UTC", Rrule: "FREQ=DAILY"}
					r := models.CommitmentRolesInput{}
					if strings.HasPrefix(door, "renegotiate") {
						d, s, b := 1, 0, "stays_owed"
						r.DeadlineDays = &d
						r.DeadlineSecs = &s
						r.DeadlineBehavior = &b
					}
					sc, err := f.srv.store.CreateCommitmentSchedule(f.ws.ID, f.item.ID, in, r)
					if err != nil {
						t.Fatal(err)
					}
					method, body = "PATCH", map[string]any{"roles": roles}
					if door == "timing" || strings.HasPrefix(door, "renegotiate") {
						if _, err := f.srv.store.DB().Exec("UPDATE item_schedules SET occupied_days=?,occupied_secs=? WHERE id=?", tc.days, tc.secs, sc.ID); err != nil {
							t.Fatal(err)
						}
						body = map[string]any{"dtstart": strings.Replace(tc.start, "01-01", "01-02", 1)}
					}
					if strings.HasPrefix(door, "renegotiate") {
						method, path, body = "POST", path+"/renegotiate", map[string]any{"scope": "series", "reason": "agreed", "remove_deadline": true, "preview": true}
						if door == "renegotiate occurrence" {
							body = map[string]any{"reason": "agreed", "deadline": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), "preview": true}
						}
					}
				}
				before := commitmentSnapshot(t, f)
				d := f.expectErr(f.as(f.token(f.owner, ""), method, path, body), 400, "validation_error", "")
				if d["field"] != "occupied_until" {
					t.Fatalf("field: %v", d)
				}
				if before != commitmentSnapshot(t, f) {
					t.Fatal("invalid occupied offset wrote")
				}
			})
		}
	}
}

func TestCommitmentWindowTimingPreservesOffset(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"native", "legacy_backfill"} {
		for _, start := range []string{"2099-01-01T08:00", "2099-01-01T12:00"} {
			t.Run(origin+"/"+start, func(t *testing.T) {
				f := newSchedHTTP(t)
				in := models.ScheduleCreate{Kind: "window", Dtstart: "2099-01-01T09:00", Dtend: "2099-01-01T11:00", Tz: "UTC"}
				var err error
				if origin == "native" {
					d, s := 0, 7200
					_, err = f.srv.store.CreateCommitmentSchedule(f.ws.ID, f.item.ID, in, models.CommitmentRolesInput{OccupiedDays: &d, OccupiedSecs: &s})
				} else {
					_, err = f.srv.store.CreateSchedule(f.ws.ID, f.item.ID, in)
				}
				if err != nil {
					t.Fatal(err)
				}
				readCommitment(t, commitmentCall(f, "PATCH", map[string]any{"dtstart": start}), 200)
				sc, err := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := "2099-01-01T10:00:00"
				if strings.Contains(start, "12:00") {
					want = "2099-01-01T14:00:00"
				}
				if sc.Dtend == nil || *sc.Dtend != want {
					t.Fatalf("dtend=%v want %s", sc.Dtend, want)
				}
			})
		}
	}
}

func TestCommitmentResolveUsesDeadlineRole(t *testing.T) {
	t.Parallel()
	f := timingFixture(t, "native", true)
	if _, err := f.srv.store.DB().Exec("UPDATE item_schedules SET kind='check',dtend=NULL WHERE item_id=?", f.item.ID); err != nil {
		t.Fatal(err)
	}
	before := commitmentSnapshot(t, f)
	rr := f.as(f.token(f.owner, ""), "POST", commitmentPath(f)+"/resolve", map[string]any{"state": "move", "occurrence_at": "2099-01-01T09:00:00Z", "until": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)})
	f.expectErr(rr, 400, "validation_error", "combination")
	if !strings.Contains(rr.Body.String(), "POST .../commitment/renegotiate") || strings.Contains(rr.Body.String(), "change its kind") {
		t.Fatalf("message: %s", rr.Body.String())
	}
	if !reflect.DeepEqual(before, commitmentSnapshot(t, f)) {
		t.Fatal("refused move wrote")
	}
}

func TestCommitmentCombinedWindowAndStartUsesEditedRoles(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	d, s := 0, 7200
	if _, err := f.srv.store.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Dtstart: "2099-01-01T09:00", Tz: "UTC"}, models.CommitmentRolesInput{OccupiedDays: &d, OccupiedSecs: &s}); err != nil {
		t.Fatal(err)
	}
	readCommitment(t, commitmentCall(f, "PATCH", map[string]any{"dtstart": "2099-01-02", "roles": map[string]any{"occupied_days": 1, "occupied_secs": 0}}), 200)
	sc, err := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Kind != "window" || sc.Dtend == nil || *sc.Dtend != "2099-01-03" {
		t.Fatalf("combined window: %+v", sc)
	}
}
