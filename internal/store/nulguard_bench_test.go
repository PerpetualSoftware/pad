package store

import (
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The read-path cost measurement S1 named as owed.
//
// The guard touches EVERY statement, reads included, so its per-parameter cost
// is paid on the read flood rather than only on writes. BUG-2812 already
// measured the HTTP gate at ~1.5x alloc / ~2.0x CPU on large bodies and that
// was judged worth paying at ONE door; this is a cost at every door, so it
// needs its own number rather than an assumption that a byte scan is free.
//
// What is being measured is normalizeAndCheck against parameter shapes the read path
// actually binds — ids, slugs, short filter strings — plus the shapes that
// exercise each arm, so the answer says which arm costs what.
func benchArgs(vals ...string) []driver.NamedValue {
	out := make([]driver.NamedValue, len(vals))
	for i, v := range vals {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// The overwhelmingly common read shape: a workspace id and an item id, both
// UUIDs. Every list, every resolve, every permission check.
func BenchmarkNormalizeAndCheck_TypicalRead(b *testing.B) {
	args := benchArgs("5f13fa9a-78e7-4aff-92d5-a85b80b35eaf", "0dbe0458-a965-4fc2-85d7-ab6f5e140a56")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := normalizeAndCheck(args); err != nil {
			b.Fatal(err)
		}
	}
}

// A read carrying user text — a search term or a title filter. Still no JSON,
// so still the cheap arm.
func BenchmarkNormalizeAndCheck_ReadWithUserText(b *testing.B) {
	args := benchArgs("5f13fa9a-78e7-4aff-92d5-a85b80b35eaf", "a reasonably long search phrase someone might type")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := normalizeAndCheck(args); err != nil {
			b.Fatal(err)
		}
	}
}

// The []byte exemption's cost: a Yjs op-log append binds a blob, and the guard
// must skip it without touching the bytes.
func BenchmarkNormalizeAndCheck_BinaryParameterSkipped(b *testing.B) {
	blob := make([]byte, 64*1024)
	args := []driver.NamedValue{{Ordinal: 1, Value: "item-id"}, {Ordinal: 2, Value: blob}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := normalizeAndCheck(args); err != nil {
			b.Fatal(err)
		}
	}
}

// A JSON-classed WRITE: a realistic item fields blob carrying no escape, so it
// pays the IsJSONDocument parse but not the walk. This is the arm that must not
// run on reads.
func BenchmarkNormalizeAndCheck_JSONWriteNoEscape(b *testing.B) {
	fields := `{"status":"in-progress","priority":"high","effort":"m","due_date":"2026-09-30","assignee":"someone@example.com"}`
	args := benchArgs("item-id", fields)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := normalizeAndCheck(args); err != nil {
			b.Fatal(err)
		}
	}
}

// The same blob WITH an escape present, so the escape pre-filter fires and the
// full decode-and-walk runs. The worst case for an ACCEPTED value.
//
// The escape here must NOT decode to a NUL, or the benchmark measures the
// refusal path instead of the acceptance path — the first version of this used
// textguard.EscNUL and every iteration was refused, which is why it silently
// produced no result at all. It is a \u0041 (the letter A), built rather than
// typed for the same reason every other escape in this unit is.
func BenchmarkNormalizeAndCheck_JSONWriteWithEscape(b *testing.B) {
	harmlessEscape := string([]byte{'\\', 'u', '0', '0', '4', '1'})
	fields := `{"status":"open","note":"harmless ` + harmlessEscape + ` text","priority":"low"}`
	args := benchArgs("item-id", fields)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := normalizeAndCheck(args); err != nil {
			b.Fatal(err)
		}
	}
}

// A large content write — the shape that would hurt most if the JSON arm ran
// on everything. Content is user text, not JSON, so the escape pre-filter
// short-circuits before any parse.
func BenchmarkNormalizeAndCheck_LargeTextWrite(b *testing.B) {
	body := make([]byte, 0, 256*1024)
	for len(body) < 256*1024 {
		body = append(body, "Lorem ipsum dolor sit amet, consectetur adipiscing elit. "...)
	}
	args := benchArgs("item-id", string(body))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := normalizeAndCheck(args); err != nil {
			b.Fatal(err)
		}
	}
}

// The number that actually decides whether the guard is affordable: a REAL
// read through the store, guarded, against the same read on the raw driver.
//
// normalizeAndCheck' 33ns is meaningless on its own — it is only a cost if it is a
// cost RELATIVE to the statement it rides on. These two benchmarks are the
// same query on the same data, differing only in which driver name the pool
// was opened with, so the ratio between them is the guard's real overhead on
// the read flood.
func benchReadStore(b *testing.B, guarded bool) {
	b.Helper()
	s := benchStore(b, guarded)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Bench"})
	if err != nil {
		b.Fatalf("workspace: %v", err)
	}
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true}]}`,
	})
	if err != nil {
		b.Fatalf("collection: %v", err)
	}
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "Bench subject",
		Content: "a body of the sort a real item carries",
		Fields:  `{"status":"open"}`,
	})
	if err != nil {
		b.Fatalf("item: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetItem(item.ID); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreRead_Guarded(b *testing.B)   { benchReadStore(b, true) }
func BenchmarkStoreRead_Unguarded(b *testing.B) { benchReadStore(b, false) }

// THE RESULT, recorded here so the next reader does not have to re-derive it —
// and stated at the width the measurement supports rather than as a headline
// number, because the end-to-end pair does NOT resolve the difference.
//
// Per-call, normalizeAndCheck against a typical read's parameters (two UUIDs):
//
//	BenchmarkNormalizeAndCheck_TypicalRead      33.5 ns/op    0 B/op   0 allocs/op
//	BenchmarkNormalizeAndCheck_ReadWithUserText 33.9 ns/op    0 B/op   0 allocs/op
//	BenchmarkNormalizeAndCheck_BinarySkipped    20.2 ns/op    0 B/op   0 allocs/op
//	BenchmarkNormalizeAndCheck_JSONWriteNoEscape 32.5 ns/op   0 B/op   0 allocs/op
//	BenchmarkNormalizeAndCheck_JSONWriteWithEscape 2167 ns/op 697 B/op 16 allocs/op
//	BenchmarkNormalizeAndCheck_LargeTextWrite (256 KiB) 7365 ns/op 0 B/op 0 allocs/op
//
// End-to-end, the same GetItem guarded and unguarded, 3000x x 3:
//
//	Guarded    118695 / 143724 / 127118 ns/op   7034 B/op   158 allocs/op
//	Unguarded  113577 / 125275 / 137065 ns/op   7034 B/op   158 allocs/op
//
// TWO THINGS THAT NUMBER SET ACTUALLY SAYS:
//
//  1. The ALLOCATION profile is identical — 7034 B and 158 allocs on both
//     sides. That is a real, resolvable invariant: the guard adds no
//     allocation to a read, because the raw-NUL check is a byte scan and the
//     escape pre-filter short-circuits before any parse.
//  2. The TIME difference is NOT resolvable here and I am not going to quote
//     one. Run-to-run spread within each group is ~25 microseconds; the
//     expected difference is ~34 nanoseconds, three orders of magnitude
//     smaller. Reporting "3.6% slower" from these six numbers would be reading
//     noise as signal.
//
// The affordability argument therefore rests on the per-call measurement
// against the statement it rides on: ~34 ns of guard against ~125 microseconds
// of read is ~0.03%. The JSON arm — the only expensive one — does not run on
// reads, and not because reads are special: it is gated behind the escape
// pre-filter, so it runs only for a value that actually contains the escape.
// BenchmarkNormalizeAndCheck_JSONWriteNoEscape is the evidence for that, at 32.5 ns
// and zero allocations on a realistic fields blob.

// benchStore opens a store on the guarded or the raw driver. It duplicates a
// little of New()'s setup deliberately: the point is to vary ONLY the driver
// name, and calling New() would always give the guarded one.
func benchStore(b *testing.B, guarded bool) *Store {
	b.Helper()
	dir := b.TempDir()
	dsn := filepath.Join(dir, "bench.db") + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(on)&_txlock=immediate"
	if err := registerGuardedDrivers(); err != nil {
		b.Fatalf("register: %v", err)
	}
	name := "sqlite"
	if guarded {
		name = guardedSQLiteDriver
	}
	db, err := sql.Open(name, dsn)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		b.Fatalf("wal: %v", err)
	}
	db.SetMaxOpenConns(sqliteMaxOpenConns)
	s := &Store{db: db, dialect: &sqliteDialect{}}
	if err := s.migrate(); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return s
}
