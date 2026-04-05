package store

import "testing"

func TestRebindQuery(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no params", "SELECT 1", "SELECT 1"},
		{"single param", "SELECT * FROM t WHERE id = ?", "SELECT * FROM t WHERE id = $1"},
		{"multiple params", "INSERT INTO t (a, b, c) VALUES (?, ?, ?)", "INSERT INTO t (a, b, c) VALUES ($1, $2, $3)"},
		{"string literal preserved", "SELECT * FROM t WHERE name = 'what?' AND id = ?", "SELECT * FROM t WHERE name = 'what?' AND id = $1"},
		{"mixed", "SELECT * FROM t WHERE a = ? AND b = 'foo?' AND c = ?", "SELECT * FROM t WHERE a = $1 AND b = 'foo?' AND c = $2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rebindQuery(tt.input)
			if got != tt.want {
				t.Errorf("rebindQuery(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSQLiteDialect(t *testing.T) {
	d := &sqliteDialect{}

	if d.Driver() != DriverSQLite {
		t.Errorf("expected DriverSQLite, got %v", d.Driver())
	}
	if got := d.JSONExtractText("i.fields", "status"); got != "json_extract(i.fields, '$.status')" {
		t.Errorf("JSONExtractText = %q", got)
	}
	if got := d.Now(); got != "datetime('now')" {
		t.Errorf("Now = %q", got)
	}
	if got := d.FTSMatch("items_fts", "search_vector"); got != "items_fts MATCH ?" {
		t.Errorf("FTSMatch = %q", got)
	}
	if got := d.GroupConcat("u.name", true); got != "GROUP_CONCAT(DISTINCT u.name)" {
		t.Errorf("GroupConcat = %q", got)
	}
}

func TestPostgresDialect(t *testing.T) {
	d := &postgresDialect{}

	if d.Driver() != DriverPostgres {
		t.Errorf("expected DriverPostgres, got %v", d.Driver())
	}
	if got := d.Placeholder(3); got != "$3" {
		t.Errorf("Placeholder(3) = %q", got)
	}
	if got := d.JSONExtractText("i.fields", "status"); got != "i.fields->>'status'" {
		t.Errorf("JSONExtractText = %q", got)
	}
	if got := d.JSONRemove("fields", "phase"); got != "(fields::jsonb - 'phase')" {
		t.Errorf("JSONRemove = %q", got)
	}
	if got := d.GroupConcat("u.name", true); got != "STRING_AGG(DISTINCT u.name, ',')" {
		t.Errorf("GroupConcat = %q", got)
	}
	if got := d.ILike(); got != "ILIKE" {
		t.Errorf("ILike = %q", got)
	}
}
