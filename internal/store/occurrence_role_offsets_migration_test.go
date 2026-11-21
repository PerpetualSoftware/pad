package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const occurrenceOriginalColumns = "id,schedule_id,occurrence_at,occurrence_wall,state,comment_id,actor,resolved_at,actual_source,actual_done_at"

var occurrenceRoleColumns = []string{"roles_target_days", "roles_target_secs", "roles_earliest_days", "roles_earliest_secs", "roles_deadline_days", "roles_deadline_secs", "roles_deadline_behavior", "roles_occupied_days", "roles_occupied_secs"}

// TestOccurrenceRoleOffsetsMigration upgrades a database holding history of
// every state across schedules with different offsets, on both engines.
func TestOccurrenceRoleOffsetsMigration(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(map[bool]string{false: "sqlite", true: "postgres"}[pg], func(t *testing.T) {
			var s *Store
			if pg {
				url := os.Getenv("PAD_TEST_POSTGRES_URL")
				if url == "" {
					t.Skip("PAD_TEST_POSTGRES_URL not set")
				}
				s = testStorePostgres(t, url)
				s.mustExec(t, `ALTER TABLE schedule_occurrences DROP COLUMN roles_target_days, DROP COLUMN roles_target_secs, DROP COLUMN roles_earliest_days, DROP COLUMN roles_earliest_secs, DROP COLUMN roles_deadline_days, DROP COLUMN roles_deadline_secs, DROP COLUMN roles_deadline_behavior, DROP COLUMN roles_occupied_days, DROP COLUMN roles_occupied_secs`)
			} else {
				db, rest := preMigrationDB(t, "189")
				if len(rest) == 0 || !strings.HasPrefix(rest[0], "189_") {
					t.Fatalf("migration 189 is not next: %v", rest)
				}
				s = &Store{db: db, dialect: &sqliteDialect{}}
			}
			ws := createTestWorkspace(t, s, "Occurrence roles migration")
			col := createTestCollection(t, s, ws.ID, "Tasks")
			states := []string{"done", "skipped", "missed", "lapsed", "moved"}
			var scheds []string
			for n, roles := range []*models.CommitmentRolesInput{
				{TargetDays: 1, TargetSecs: 3600, EarliestDays: i(-1), EarliestSecs: i(0), DeadlineDays: i(2), DeadlineSecs: i(0), DeadlineBehavior: ptr("lapses"), OccupiedDays: i(0), OccupiedSecs: i(5400)},
				{TargetSecs: -900},
				nil, // a legacy hard deadline: deadline 0/0 stays_owed from the 167 backfill
			} {
				it := createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Migrating %d", n), "")
				in := models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}
				var sc *models.ItemSchedule
				var err error
				if roles == nil {
					in.Kind = "deadline_hard"
					sc, err = s.CreateSchedule(ws.ID, it.ID, in)
				} else {
					sc, err = s.CreateCommitmentSchedule(ws.ID, it.ID, in, *roles)
				}
				if err != nil {
					t.Fatal(err)
				}
				scheds = append(scheds, sc.ID)
				for d, state := range states {
					day := time.Date(2030, 2, 1+d, 9, 0, 0, 0, time.UTC)
					s.mustExec(t, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at,actual_source) VALUES (?,?,?,?,?,'user',?,'legacy_unknown')`,
						fmt.Sprintf("%s-%d", sc.ID, d), sc.ID, day.Format(time.RFC3339), day.Format("2006-01-02T15:04:05"), state, day.Add(time.Hour).Format(time.RFC3339))
				}
			}
			var objects string
			if !pg {
				objects = renegotiateRows(t, s, "(SELECT name AS id,type,name,sql FROM sqlite_master WHERE tbl_name='schedule_occurrences' AND type IN ('index','trigger'))", "")
			}
			original := renegotiateRows(t, s, "(SELECT "+occurrenceOriginalColumns+" FROM schedule_occurrences)", "")
			schedulesBefore := renegotiateRows(t, s, "item_schedules", "")

			fs, path := migrationsFS, "migrations/189_occurrence_role_offsets.sql"
			if pg {
				fs, path = pgMigrationsFS, "pgmigrations/147_occurrence_role_offsets.sql"
			}
			body, err := fs.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if pg {
				err = applyPostgresMigration(s.db, "999_occurrence_roles_probe.sql", string(body))
			} else {
				err = applySQLiteMigration(s.db, "189_occurrence_role_offsets.sql", string(body))
			}
			if err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			stamped := s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE roles_target_days IS NOT NULL`)
			t.Logf("occurrence role backfill (%s): migration took %s, %d of %d rows stamped", map[bool]string{false: "sqlite", true: "postgres"}[pg], elapsed, stamped, s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences`))
			if want := len(scheds) * (len(states) - 1); stamped != want {
				t.Fatalf("stamped %d rows, want %d", stamped, want)
			}

			if got := renegotiateRows(t, s, "(SELECT "+occurrenceOriginalColumns+" FROM schedule_occurrences)", ""); got != original {
				t.Fatalf("existing occurrence columns changed\nbefore %s\nafter  %s", original, got)
			}
			if got := renegotiateRows(t, s, "item_schedules", ""); got != schedulesBefore {
				t.Fatal("item_schedules changed")
			}
			if !pg && objects != renegotiateRows(t, s, "(SELECT name AS id,type,name,sql FROM sqlite_master WHERE tbl_name='schedule_occurrences' AND type IN ('index','trigger'))", "") {
				t.Fatal("index or trigger definitions changed")
			}

			// Row for row: the stamp is the schedule's offsets, a moved row has none.
			rows, err := s.db.Query(s.q(`SELECT o.id, o.state, `+prefixed("o.", occurrenceRoleColumns)+`, `+prefixed("sc.", []string{"target_days", "target_secs", "earliest_days", "earliest_secs", "deadline_days", "deadline_secs", "deadline_behavior", "occupied_days", "occupied_secs"})+`
				FROM schedule_occurrences o JOIN item_schedules sc ON sc.id = o.schedule_id ORDER BY o.id`))
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			for rows.Next() {
				var id, state string
				vals := make([]any, 18)
				dest := []any{&id, &state}
				for k := range vals {
					dest = append(dest, &vals[k])
				}
				if err := rows.Scan(dest...); err != nil {
					t.Fatal(err)
				}
				for k := 0; k < 9; k++ {
					got, want := sqlValue(vals[k]), sqlValue(vals[9+k])
					if state == "moved" {
						want = "<nil>"
					}
					if got != want {
						t.Errorf("%s (%s) %s = %s, want %s", id, state, occurrenceRoleColumns[k], got, want)
					}
				}
				checked++
			}
			rows.Close()
			if checked != len(scheds)*len(states) {
				t.Fatalf("checked %d rows", checked)
			}
			// The fixture really carries distinct offsets, so equality above is not vacuous.
			if s.countRows(t, `SELECT COUNT(DISTINCT roles_target_secs) FROM schedule_occurrences`) != 3 || s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE roles_deadline_behavior='stays_owed'`) != len(states)-1 {
				t.Fatal("fixture offsets are not distinct")
			}

			// Each CHECK refuses a bad row.
			sc := scheds[0]
			ok := func(cols string, vals ...any) error {
				names := "id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at"
				_, err := s.db.Exec(s.q(`INSERT INTO schedule_occurrences (`+names+`,`+cols+`) VALUES (?,?,?,?,?,'',?,`+placeholders(len(vals)-1)+`)`),
					append([]any{newID(), sc, "2031-01-01T09:00:00Z", "2031-01-01T09:00:00", vals[0], "2031-01-01T10:00:00Z"}, vals[1:]...)...)
				return err
			}
			for name, tc := range map[string][]any{
				"target pair":                {"roles_target_days", "done", 0},
				"earliest pair":              {"roles_target_days,roles_target_secs,roles_earliest_days", "done", 0, 0, 1},
				"deadline pair":              {"roles_target_days,roles_target_secs,roles_deadline_secs,roles_deadline_behavior", "done", 0, 0, 1, "lapses"},
				"behavior without deadline":  {"roles_target_days,roles_target_secs,roles_deadline_behavior", "done", 0, 0, "lapses"},
				"deadline without behavior":  {"roles_target_days,roles_target_secs,roles_deadline_days,roles_deadline_secs", "done", 0, 0, 1, 0},
				"behavior enum":              {"roles_target_days,roles_target_secs,roles_deadline_days,roles_deadline_secs,roles_deadline_behavior", "done", 0, 0, 1, 0, "soft"},
				"occupied pair":              {"roles_target_days,roles_target_secs,roles_occupied_secs", "done", 0, 0, 60},
				"stamped moved row":          {"roles_target_days,roles_target_secs", "moved", 0, 0},
				"secs without days (target)": {"roles_target_secs", "skipped", 0},
			} {
				if err := ok(tc[0].(string), tc[1:]...); err == nil {
					t.Errorf("%s: a bad row was accepted", name)
				}
			}
			if err := ok("roles_target_days,roles_target_secs,roles_deadline_days,roles_deadline_secs,roles_deadline_behavior", "done", 0, 0, 1, 0, "stays_owed"); err != nil {
				t.Fatalf("a good stamped row was refused: %v", err)
			}

			if !pg {
				if s.countRows(t, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='uq_schedule_occurrences_allday_wall'`) != 1 {
					t.Fatal("lost the all-day wall index")
				}
				for _, column := range []string{"actor", "occurrence_wall"} {
					if _, err := s.db.Exec(`UPDATE schedule_occurrences SET `+column+`=? WHERE schedule_id=?`, "\x00", sc); err == nil {
						t.Fatal("lost NUL guard", column)
					}
				}
				fk, err := s.db.Query(`PRAGMA foreign_key_check`)
				if err != nil {
					t.Fatal(err)
				}
				violation := fk.Next()
				fk.Close()
				if violation {
					t.Fatal("foreign key violation")
				}
			}
			s.mustExec(t, `DELETE FROM item_schedules WHERE id=?`, sc)
			if s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, sc) != 0 {
				t.Fatal("lost the cascading foreign key")
			}
		})
	}
}

func prefixed(p string, cols []string) string {
	out := make([]string, len(cols))
	for k, c := range cols {
		out[k] = p + c
	}
	return strings.Join(out, ", ")
}

func sqlValue(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}
