package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Hot-path benchmark for GetOAuthConnectionAccess (PLAN-1519 Phase A
// acceptance: "Hot-path benchmark: dual-read overhead measured and
// documented").
//
// The dual-read introspection gate added in TASK-1520 calls this
// method on every authenticated /mcp request. Two shapes matter to
// the overhead profile:
//
//   - Wildcard (all_current_workspaces=true) — single PK lookup, the
//     join is short-circuited. This is the dominant shape post-Phase-C
//     (IDEA-1517 §2a defaults the consent UI to wildcard).
//   - Explicit (all_current_workspaces=false) — PK lookup + indexed
//     scan + small join against workspaces. Rarer in practice but
//     worth measuring because the cost scales with allow-list size.
//
// Numbers from a local run on this branch (SQLite, modernc.org/sqlite
// driver, BEGIN IMMEDIATE + WAL, AMD Ryzen 3 5300U):
//
//   BenchmarkGetOAuthConnectionAccess_Wildcard-8    12.2µs/op     488 B/op     15 allocs/op
//   BenchmarkGetOAuthConnectionAccess_Explicit-8    36.9µs/op    1168 B/op     45 allocs/op
//   BenchmarkGetOAuthConnectionAccess_NoRow-8       11.9µs/op     488 B/op     15 allocs/op
//
// The dual-read overhead per request is bounded by these calls —
// well under a millisecond and comfortably below the per-request HTTP
// + Bearer-parsing budget. If a future change pushes either case into
// multi-millisecond territory, the place to look is the workspaces
// join (Explicit) or the PK lookup contention with concurrent writers
// (Wildcard / NoRow share the same PK-lookup shape — sql.ErrNoRows
// vs. a row is the only branch difference).
//
// Run with:
//
//	go test -bench BenchmarkGetOAuthConnectionAccess -benchmem ./internal/store/

func benchSeedConnection(b *testing.B, allCurrent bool, allowedSlugs int) (*Store, string) {
	b.Helper()
	s, err := New(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("New store: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	u, err := s.CreateUser(models.UserCreate{
		Email: "bench@example.com", Name: "B", Password: "pw-bench-12345",
	})
	if err != nil {
		b.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID:            "bench-chain",
		UserID:               u.ID,
		AllCurrentWorkspaces: allCurrent,
	}); err != nil {
		b.Fatalf("CreateOAuthConnection: %v", err)
	}
	for i := 0; i < allowedSlugs; i++ {
		// Each workspace needs its own owner row to satisfy uniqueness
		// constraints — quick throwaway users keep the bench focused
		// on the read path.
		owner, err := s.CreateUser(models.UserCreate{
			Email: fmt.Sprintf("bench-owner-%d@example.com", i), Name: "Owner",
			Password: "pw-bench-owner-12345",
		})
		if err != nil {
			b.Fatalf("CreateUser owner %d: %v", i, err)
		}
		ws, err := s.CreateWorkspace(models.WorkspaceCreate{
			Name:    fmt.Sprintf("bench-ws-%d", i),
			OwnerID: owner.ID,
		})
		if err != nil {
			b.Fatalf("CreateWorkspace %d: %v", i, err)
		}
		if err := s.AddConnectionWorkspace("bench-chain", ws.ID, AddedByUser); err != nil {
			b.Fatalf("AddConnectionWorkspace %d: %v", i, err)
		}
	}
	return s, "bench-chain"
}

func BenchmarkGetOAuthConnectionAccess_Wildcard(b *testing.B) {
	s, reqID := benchSeedConnection(b, true, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetOAuthConnectionAccess(reqID); err != nil {
			b.Fatalf("GetOAuthConnectionAccess: %v", err)
		}
	}
}

func BenchmarkGetOAuthConnectionAccess_Explicit(b *testing.B) {
	// 4 workspaces in the allow-list — representative of the IDEA-1517
	// §2a UX where a user picks "Only specific workspaces" and ticks
	// a handful. Scales linearly past that; covered as a smoke.
	s, reqID := benchSeedConnection(b, false, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetOAuthConnectionAccess(reqID); err != nil {
			b.Fatalf("GetOAuthConnectionAccess: %v", err)
		}
	}
}

func BenchmarkGetOAuthConnectionAccess_NoRow(b *testing.B) {
	// "No connection row" is the dominant Phase-A shape (the new
	// tables are empty until Phase C wires the write path), so the
	// PK-lookup-miss path deserves its own bench number too.
	s, err := New(filepath.Join(b.TempDir(), "no_row.db"))
	if err != nil {
		b.Fatalf("New store: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetOAuthConnectionAccess("never-existed"); err != nil {
			b.Fatalf("GetOAuthConnectionAccess: %v", err)
		}
	}
}
