package store

import "testing"

func TestRebindQuery(t *testing.T) {
	t.Parallel()
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
		// BUG-3430: an apostrophe in a comment used to start "string mode",
		// leaving every later ? unbound on Postgres only.
		{"apostrophe in line comment", "SELECT * FROM t\n-- the collection's items\nWHERE a = ? AND b = ?", "SELECT * FROM t\n-- the collection's items\nWHERE a = $1 AND b = $2"},
		{"line comment ends at newline", "SELECT ? -- what? isn't bound\n, ?", "SELECT $1 -- what? isn't bound\n, $2"},
		{"line comment at end of query", "SELECT ? -- trailing, no newline?", "SELECT $1 -- trailing, no newline?"},
		{"apostrophe in block comment", "SELECT * FROM t /* don't */ WHERE a = ?", "SELECT * FROM t /* don't */ WHERE a = $1"},
		{"? in block comment", "SELECT ? /* is ? bound? no */ , ?", "SELECT $1 /* is ? bound? no */ , $2"},
		{"multi-line block comment", "SELECT ?\n/* it's\n a ? */\nWHERE b = ?", "SELECT $1\n/* it's\n a ? */\nWHERE b = $2"},
		{"comment markers inside a string", "SELECT 'a--b?' , '/* c? */' , ?", "SELECT 'a--b?' , '/* c? */' , $1"},
		{"escaped quote in a string", "SELECT 'it''s ?' , ?", "SELECT 'it''s ?' , $1"},
		{"apostrophe and ? in a quoted identifier", `SELECT "it's?" FROM t WHERE a = ?`, `SELECT "it's?" FROM t WHERE a = $1`},
		// Postgres block comments nest (codex r1).
		{"nested block comment", "SELECT /* outer /* inner */ ? */ ?", "SELECT /* outer /* inner */ ? */ $1"},
		{"block comment at end of query", "SELECT ? /* done? */", "SELECT $1 /* done? */"},
		{"comment markers inside a quoted identifier", `SELECT "a--b?", "c/*d?" FROM t WHERE x = ?`, `SELECT "a--b?", "c/*d?" FROM t WHERE x = $1`},
		{"escaped quote in a quoted identifier", `SELECT "say ""hi?""" FROM t WHERE x = ?`, `SELECT "say ""hi?""" FROM t WHERE x = $1`},
		{"double quote in a comment", "SELECT ? -- a \"quoted\" word\n, ?", "SELECT $1 -- a \"quoted\" word\n, $2"},
		// Unterminated: the rest is copied as-is (Postgres then reports the
		// syntax error itself).
		{"unterminated string", "SELECT ?, 'oops ?", "SELECT $1, 'oops ?"},
		{"unterminated block comment", "SELECT ? /* never ? closed", "SELECT $1 /* never ? closed"},
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

func TestDateBucket(t *testing.T) {
	t.Parallel()
	// Day/hour bucketing is fixed-width substring on the UTC RFC3339 TEXT, so
	// both dialects emit the same expression. Verify each granularity.
	for _, d := range []Dialect{&sqliteDialect{}, &postgresDialect{}} {
		if got := d.DateBucket("created_at", "day"); got != "SUBSTR(created_at, 1, 10)" {
			t.Errorf("%T day bucket = %q", d, got)
		}
		if got := d.DateBucket("created_at", "hour"); got != "SUBSTR(created_at, 1, 13)" {
			t.Errorf("%T hour bucket = %q", d, got)
		}
		// Unknown granularity falls back to day.
		if got := d.DateBucket("created_at", "week"); got != "SUBSTR(created_at, 1, 10)" {
			t.Errorf("%T fallback bucket = %q", d, got)
		}
	}
}

func TestSQLiteDialect(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
