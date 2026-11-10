package store

import "testing"

func TestTASK4423_R05Migration(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Graph migration")
	u := notifyUser(t, s, "graph-migration@example.com")
	for _, kind := range []string{"deadline_approval_requested", "commitment_moved", "commitment_moved_past_deadline", "commitment_gate_opened"} {
		if _, err := s.db.Exec(s.q(`INSERT INTO user_notifications (id,user_id,workspace_id,item_id,kind,dedupe_key,created_at) VALUES (?,?,?,?,?,?,?)`), newID(), u.ID, ws.ID, "subject", kind, kind, nowNano()); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
	if s.dialect.Driver() == DriverSQLite {
		rows, err := s.db.Query(`SELECT name,sql FROM sqlite_master WHERE type='trigger' AND tbl_name='user_notifications'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var name, sql string
			rows.Scan(&name, &sql)
			t.Log(name, sql)
		}
	}
}
