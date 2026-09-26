package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// BUG-3222: invalid UTF-8 rides the NUL census. PostgreSQL refuses it
// (SQLSTATE 22021), SQLite stores it, and a migration meeting one fails
// partway through the copy.

// invalidByte is one byte that can never start or continue valid UTF-8 on its
// own. Built from its value so no escape text passes through an editor.
var invalidByte = string([]byte{0x80})

func TestScanAndRepairInvalidUTF8(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}
	ws := createTestWorkspace(t, s, "UTF8WS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	textRow := createTestItem(t, s, ws.ID, col.ID, "text", "body")
	jsonRow := createTestItem(t, s, ws.ID, col.ID, "json", "body")
	bothRow := createTestItem(t, s, ws.ID, col.ID, "both", "body")
	control := createTestItem(t, s, ws.ID, col.ID, "control", "body")

	multibyte := "é" + string(rune(0x4E2D)) + string(rune(0x1F600))
	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw, `UPDATE items SET title = ? WHERE id = ?`, "ok"+invalidByte+"x", textRow.ID)
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`, `{"note":"x`+invalidByte+`y"}`, jsonRow.ID)
		mustExec(t, raw, `UPDATE items SET title = ? WHERE id = ?`, "a"+textguard.NUL+"b"+invalidByte, bothRow.ID)
		mustExec(t, raw, `UPDATE items SET title = ?, fields = ? WHERE id = ?`,
			multibyte, `{"note":"`+multibyte+`"}`, control.ID)
	})

	report, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := map[string]NULViolation{}
	for _, v := range report.Violations {
		got[v.Key["id"]+"/"+v.Column] = v
	}
	for _, c := range []struct {
		key                 string
		wantInvalid, rawNUL bool
	}{
		{textRow.ID + "/title", true, false},
		{jsonRow.ID + "/fields", true, false},
		{bothRow.ID + "/title", true, true},
	} {
		v, ok := got[c.key]
		if !ok {
			t.Errorf("%s: not reported", c.key)
			continue
		}
		if v.InvalidUTF8 != c.wantInvalid || v.RawNUL != c.rawNUL {
			t.Errorf("%s: InvalidUTF8=%v RawNUL=%v, want %v/%v", c.key, v.InvalidUTF8, v.RawNUL, c.wantInvalid, c.rawNUL)
		}
		if v.WorkspaceID != ws.ID {
			t.Errorf("%s: workspace %q, want %q", c.key, v.WorkspaceID, ws.ID)
		}
	}
	for k := range got {
		if strings.HasPrefix(k, control.ID) {
			t.Errorf("valid multibyte text reported as a violation: %s", k)
		}
	}
	if report.Total() != 3 {
		t.Errorf("violations = %d, want 3 (a row with both defects counts once): %v", report.Total(), report.Violations)
	}
	if !strings.Contains(got[bothRow.ID+"/title"].String(), "raw NUL + invalid UTF-8") {
		t.Errorf("both kinds not named: %s", got[bothRow.ID+"/title"])
	}

	rep, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(rep.Failed) != 0 || len(rep.Repaired) != 3 {
		t.Fatalf("repaired %d, failed %v; want 3 and none", len(rep.Repaired), rep.Failed)
	}
	read := func(col, id string) string {
		var v string
		if err := s.db.QueryRow(`SELECT `+col+` FROM items WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := read("title", textRow.ID); v != "ok"+textguard.Replacement+"x" {
		t.Errorf("text row repaired to %q", v)
	}
	if v := read("title", bothRow.ID); v != "a"+textguard.Replacement+"b"+textguard.Replacement {
		t.Errorf("both row repaired to %q", v)
	}
	if v := read("title", control.ID); v != multibyte {
		t.Errorf("the control was changed: %q", v)
	}
	after, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if after.Total() != 0 {
		t.Errorf("violations after repair: %v", after.Violations)
	}
}

// In a VALID JSON document an invalid byte can only sit inside a string, so
// replacing it leaves a valid document with the same shape.
func TestRepairInvalidUTF8KeepsJSONValid(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}
	ws := createTestWorkspace(t, s, "UTF8JSON")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "json", "body")
	doc := `{"note":"x` + invalidByte + `y","n":1e3,"arr":["a` + invalidByte + `"]}`
	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`, doc, it.ID)
	})
	if _, err := s.RepairNUL(); err != nil {
		t.Fatalf("repair: %v", err)
	}
	var got string
	if err := s.db.QueryRow(`SELECT fields FROM items WHERE id = ?`, it.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := `{"note":"x` + textguard.Replacement + `y","n":1e3,"arr":["a` + textguard.Replacement + `"]}`
	if got != want || !json.Valid([]byte(got)) || !utf8.ValidString(got) {
		t.Errorf("repaired to %q (valid JSON %v), want %q", got, json.Valid([]byte(got)), want)
	}
}

// A value whose row has an INCOMPLETE key (a NULL in its primary key, which
// SQLite permits) and carries both defects is still ONE violation (codex r1).
// Two such rows stay two: the merge is by rowid, which both scans read, not by
// the partial key they share.
func TestInvalidUTF8MergesOnKeyIncompleteRows(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}
	value := `{"note":"a` + textguard.NUL + `b` + invalidByte + `"}`
	plantLegacyRows(t, s, func(raw *sql.DB) {
		for i := 0; i < 2; i++ {
			mustExec(t, raw, `INSERT INTO activities (id, action, actor, source, metadata, created_at)
				VALUES (NULL, 'created', 'agent', 'cli', ?, datetime('now'))`, value)
		}
	})
	report, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var got []NULViolation
	for _, v := range report.Violations {
		if v.Table == "activities" && v.Column == "metadata" {
			got = append(got, v)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want 2 violations (two rows, each counted once), got %d: %v", len(got), got)
	}
	for _, v := range got {
		if !v.KeyIncomplete || !v.RawNUL || !v.InvalidUTF8 {
			t.Errorf("want one incomplete-key violation carrying both kinds, got %+v", v)
		}
	}
}
