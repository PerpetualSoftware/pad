package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Decode the wire-shaped partial so absent, null and zero remain distinct.
func patchRoles(t *testing.T, raw string) *models.CommitmentRolesPatch {
	t.Helper()
	var p models.CommitmentRolesPatch
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func partialRolesFixture(t *testing.T, deadline bool) (*schedFixture, *models.ItemSchedule) {
	t.Helper()
	f := newSchedFixture(t)
	r := models.CommitmentRolesInput{TargetDays: 1, TargetSecs: 3600, EarliestDays: i(-1), EarliestSecs: i(60), Outcome: "delivered"}
	if deadline {
		r.DeadlineDays, r.DeadlineSecs, r.DeadlineBehavior = i(2), i(0), ptr("lapses")
	}
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, r)
	if err != nil {
		t.Fatal(err)
	}
	return f, sc
}

func TestCommitmentPartialRolesMerge(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "window", true: "deadline"}[deadline], func(t *testing.T) {
			f, sc := partialRolesFixture(t, deadline)
			before, err := f.s.scheduleRolesOf(f.s.db, sc)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{}, patchRoles(t, `{"occupied_days":0,"occupied_secs":7200}`))
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.s.scheduleRolesOf(f.s.db, got)
			if err != nil {
				t.Fatal(err)
			}
			want := before
			want.OccupiedDays, want.OccupiedSecs = i(0), i(7200)
			if !reflect.DeepEqual(after, want) {
				t.Fatalf("roles=%+v want %+v", after, want)
			}
			if deadline {
				if got.Kind != "deadline_hard" || got.Dtend != nil {
					t.Fatalf("derived %+v", got)
				}
			} else if got.Kind != "window" || str(got.Dtend) != "2099-01-01T11:00:00" {
				t.Fatalf("derived %+v", got)
			}
			rows := f.journal(t, sc.ID)
			if rows[len(rows)-1].Reason != rolesWriteReason {
				t.Fatalf("journal %+v", rows)
			}
			// An equal partial must not write even updated_at or another journal row.
			f.exec(t, `UPDATE item_schedules SET updated_at=? WHERE id=?`, "2000-01-01T00:00:00Z", sc.ID)
			prior := f.reload(t, f.item.ID)
			again, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{}, patchRoles(t, `{"occupied_secs":7200}`))
			if err != nil || !reflect.DeepEqual(prior, again) || len(f.journal(t, sc.ID)) != len(rows) {
				t.Fatalf("no-op wrote: %+v %v", again, err)
			}
		})
	}
}

func TestCommitmentPartialRolesPairingAndGuards(t *testing.T) {
	for _, tc := range []struct {
		name, body, field, code string
		deadline, occupied      bool
		hard                    bool
	}{
		{name: "add deadline", body: `{"deadline_days":2,"deadline_secs":0,"deadline_behavior":"lapses"}`},
		{name: "clear earliest", body: `{"earliest_days":null,"earliest_secs":null}`},
		{name: "half earliest", body: `{"earliest_days":null}`, field: "earliest", code: "combination"},
		{name: "half occupied", body: `{"occupied_secs":null}`, occupied: true, field: "occupied_until", code: "combination"},
		{name: "clear occupied", body: `{"occupied_days":null,"occupied_secs":null}`, occupied: true},
		{name: "clear deadline", body: `{"deadline_days":null,"deadline_secs":null,"deadline_behavior":null}`, deadline: true, hard: true},
		{name: "behavior only", body: `{"deadline_behavior":"stays_owed"}`, deadline: true, hard: true},
		{name: "half deadline", body: `{"deadline_days":null}`, deadline: true, field: "deadline", code: "combination"},
		{name: "no behavior", body: `{"deadline_days":2,"deadline_secs":0}`, field: "deadline_behavior", code: "combination"},
		{name: "null target days", body: `{"target_days":null}`, field: "target_days", code: "required"},
		{name: "null target secs", body: `{"target_secs":null}`, field: "target_secs", code: "required"},
		{name: "null outcome", body: `{"outcome":null}`, field: "outcome", code: "required"},
		{name: "empty", body: `{}`, field: "roles", code: "required"},
		{name: "range", body: `{"target_days":2147483648}`, field: "roles", code: "range"},
		{name: "behavior enum", body: `{"deadline_behavior":"bogus"}`, field: "deadline_behavior", code: "enum"},
		{name: "outcome nul", body: `{"outcome":"a\u0000b"}`, field: "outcome", code: "enum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sc := partialRolesFixture(t, tc.deadline)
			if tc.occupied {
				f.exec(t, `UPDATE item_schedules SET occupied_days=0,occupied_secs=7200,kind='window',dtend='2099-01-01T11:00:00' WHERE id=?`, sc.ID)
			}
			before := f.reload(t, f.item.ID)
			journal := f.journal(t, sc.ID)
			got, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{}, patchRoles(t, tc.body))
			if tc.hard {
				if !errors.Is(err, ErrHardDeadlineRenegotiate) {
					t.Fatalf("want hard deadline: %v", err)
				}
			} else if tc.field != "" {
				var fe *ScheduleFieldError
				if !errors.As(err, &fe) || fe.Field != tc.field || fe.Code != tc.code {
					t.Fatalf("want %s/%s: %v", tc.field, tc.code, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				r, _ := f.s.scheduleRolesOf(f.s.db, got)
				if r.TargetDays != 1 || r.TargetSecs != 3600 || r.Outcome != "delivered" {
					t.Fatalf("lost roles %+v", r)
				}
				switch tc.name {
				case "add deadline":
					if got.Kind != "deadline_hard" || r.EarliestDays == nil {
						t.Fatalf("derived %+v %+v", got, r)
					}
				case "clear earliest":
					if r.EarliestDays != nil {
						t.Fatal("not cleared")
					}
				case "clear occupied":
					if got.Kind != "check" || got.Dtend != nil || r.OccupiedDays != nil {
						t.Fatalf("not cleared %+v", got)
					}
				}
				return
			}
			if !reflect.DeepEqual(before, f.reload(t, f.item.ID)) || !reflect.DeepEqual(journal, f.journal(t, sc.ID)) {
				t.Fatal("refusal wrote")
			}
		})
	}
}

func TestCommitmentPartialRolesNewStartAndLegacy(t *testing.T) {
	t.Run("new start", func(t *testing.T) {
		f, _ := partialRolesFixture(t, false)
		got, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{Dtstart: ptr("2099-01-02T13:00")}, patchRoles(t, `{"occupied_days":0,"occupied_secs":7200}`))
		if err != nil || str(got.Dtend) != "2099-01-02T15:00:00" {
			t.Fatalf("new start %+v %v", got, err)
		}
	})
	for _, body := range []string{`{}`, `{"target_secs":60}`, `timing`} {
		t.Run(body, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.commitFixture(t, "deadline_hard")
			before := f.reload(t, f.item.ID)
			n := len(f.journal(t, sc.ID))
			var p *models.CommitmentRolesPatch
			upd := models.ScheduleUpdate{}
			if body == "timing" {
				upd.Dtstart = ptr("2099-01-02T09:00")
			} else {
				p = patchRoles(t, body)
			}
			got, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, upd, p)
			if body == `{}` {
				var fe *ScheduleFieldError
				if !errors.As(err, &fe) || fe.Field != "roles" || fe.Code != "required" {
					t.Fatalf("empty %v", err)
				}
				if !reflect.DeepEqual(before, f.reload(t, f.item.ID)) || len(f.journal(t, sc.ID)) != n {
					t.Fatal("empty converted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r, _ := f.s.scheduleRolesQ(f.s.db, got.ID)
			if body == "timing" {
				if r.RolesOrigin != "legacy_backfill" {
					t.Fatal("timing converted")
				}
			} else {
				if r.RolesOrigin != "native" {
					t.Fatal("not converted")
				}
				rows := f.journal(t, sc.ID)
				last := rows[len(rows)-1]
				if last.BeforeJSON == nil || last.AfterJSON == nil {
					t.Fatalf("missing conversion %+v", last)
				}
			}
		})
	}
}

func TestCommitmentPartialRolesConcurrentWriters(t *testing.T) {
	f, sc := partialRolesFixture(t, false)
	if f.s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite two-connection write serialization")
	}
	other, err := New(f.s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	a, b := patchRoles(t, `{"target_secs":7200}`), patchRoles(t, `{"outcome":"accepted"}`)
	gate := make(chan struct{})
	errs := make(chan error, 2)
	for _, write := range []struct {
		s *Store
		p *models.CommitmentRolesPatch
	}{{f.s, a}, {other, b}} {
		go func(s *Store, p *models.CommitmentRolesPatch) {
			<-gate
			_, err := s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{}, p)
			errs <- err
		}(write.s, write.p)
	}
	close(gate)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	r, err := f.s.scheduleRolesQ(f.s.db, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.TargetSecs != 7200 || r.TargetDays != 1 || r.Outcome != "accepted" || r.EarliestDays == nil {
		t.Fatalf("lost concurrent member: %+v", r)
	}
}

func TestCommitmentPartialRolesClearNeedsEveryMember(t *testing.T) {
	for _, tc := range []struct{ body, field string }{
		{`{"earliest_days":null}`, "earliest"},
		{`{"earliest_secs":null}`, "earliest"},
		{`{"occupied_days":null}`, "occupied_until"},
		{`{"occupied_secs":null}`, "occupied_until"},
		{`{"deadline_days":null}`, "deadline"},
		{`{"deadline_secs":null}`, "deadline"},
		{`{"deadline_days":null,"deadline_secs":null}`, "deadline_behavior"},
		{`{"deadline_behavior":null}`, "deadline_behavior"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			f, sc := partialRolesFixture(t, false)
			f.exec(t, `UPDATE item_schedules SET earliest_days=NULL,earliest_secs=NULL WHERE id=?`, sc.ID)
			_, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{}, patchRoles(t, tc.body))
			var fe *ScheduleFieldError
			if !errors.As(err, &fe) || fe.Field != tc.field || fe.Code != "combination" {
				t.Fatalf("want %s/combination: %v", tc.field, err)
			}
		})
	}
}

func TestCommitmentPartialRolesSingleMembersAndSoftLabel(t *testing.T) {
	f, sc := partialRolesFixture(t, true)
	got, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{}, patchRoles(t, `{"earliest_secs":120,"target_days":0,"outcome":"accepted"}`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.scheduleRolesOf(f.s.db, got)
	if err != nil {
		t.Fatal(err)
	}
	if r.EarliestDays == nil || *r.EarliestDays != -1 || *r.EarliestSecs != 120 || r.TargetDays != 0 || r.TargetSecs != 3600 || r.Outcome != "accepted" || *r.DeadlineBehavior != "lapses" {
		t.Fatalf("merged %+v", r)
	}
	// An imported native soft label is retained when neither role remains.
	f.exec(t, `UPDATE item_schedules SET deadline_days=NULL,deadline_secs=NULL,deadline_behavior=NULL,occupied_days=0,occupied_secs=7200,kind='deadline_soft',dtend=NULL WHERE id=?`, sc.ID)
	got, err = f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{}, patchRoles(t, `{"occupied_days":null,"occupied_secs":null}`))
	if err != nil || got.Kind != "deadline_soft" || got.Dtend != nil {
		t.Fatalf("soft label %+v %v", got, err)
	}
}
