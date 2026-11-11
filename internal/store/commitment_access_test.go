package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestCommitmentAccessLoadersUsePinnedTransaction(t *testing.T) {
	for _, door := range []string{"resolve", "renegotiate"} {
		t.Run(door, func(t *testing.T) {
			f := newLinkFix(t, 2)
			f.deadline(t, 1, "stays_owed")
			role := "target"
			if door == "renegotiate" {
				role = "deadline"
				f.deadline(t, 0, "stays_owed")
			}
			f.linkRaw(t, 0, 1, role, "deadline", oneDay)
			for _, mode := range []string{"revoked edit", "nil maps", "query failure"} {
				t.Run(mode, func(t *testing.T) {
					called := false
					sentinel := errors.New("access query failed")
					loader := func(q Queryer) (models.CommitmentAccess, error) {
						called = true
						if _, ok := q.(*sql.Tx); !ok {
							t.Fatal("access loader received the pool")
						}
						var id string
						if err := q.QueryRow(f.s.q(`SELECT id FROM items WHERE id=?`), f.item.ID).Scan(&id); err != nil {
							t.Fatal(err)
						}
						if mode == "query failure" {
							return models.CommitmentAccess{}, sentinel
						}
						if mode == "nil maps" {
							return models.CommitmentAccess{}, nil
						}
						return f.access(0), nil
					}
					before := renegotiateState(t, f.s)
					var err error
					if door == "resolve" {
						to := rt("2030-03-10T09:00:00Z")
						_, err = f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, ResolveInput{Actor: "user", State: "move", OccurrenceAt: rt(*f.scheds[0].NextAt), Until: &to, LinkAccess: f.allEdit(), CommitmentAccessLoader: loader}, rt("2029-12-01T00:00:00Z"))
					} else {
						to := "2030-03-10T09:00:00Z"
						_, err = f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", models.CommitmentRenegotiateInput{Preview: true, Reason: "Replan", Deadline: &to, Access: f.allEdit(), CommitmentAccessLoader: loader}, rt("2029-12-01T00:00:00Z"), nil)
					}
					want := ErrDeadlineApprovalRequired
					if mode == "query failure" {
						want = sentinel
					}
					if !called || !errors.Is(err, want) {
						t.Fatalf("loader called=%v err=%v want=%v", called, err, want)
					}
					if after := renegotiateState(t, f.s); !reflect.DeepEqual(after, before) {
						t.Fatal("access refusal wrote state")
					}
				})
			}
		})
	}
}

func TestTASK4423_CommitmentAccessItems(t *testing.T) {
	f := newLinkFix(t, 8)
	// The request has no graph edges. Disconnected edges still belong to the set.
	f.linkRaw(t, 1, 2, "target", "target", oneDay)
	if _, err := f.s.CreateScheduleGate(f.ws.ID, f.items[3].ID, f.items[4].ID, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,change_type,root_change_id,recorded_at)
 SELECT 'change-' || i.id,i.workspace_id,i.id,sc.id,'create','change-' || i.id,'2026-01-01T00:00:00Z'
 FROM items i JOIN item_schedules sc ON sc.item_id=i.id WHERE i.workspace_id=?`), f.ws.ID); err != nil {
		t.Fatal(err)
	}
	var root, parent string
	for i, dst := range map[int]*string{5: &root, 6: &parent} {
		if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_changes WHERE item_id=?`), f.items[i].ID).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE schedule_changes SET root_change_id=?,parent_change_id=? WHERE item_id=?`), root, parent, f.item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE items SET deleted_at='2026-01-01T00:00:00Z' WHERE id=?`), f.items[2].ID); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.CommitmentAccessItemsQ(f.s.Q(), f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.ID] = true
		if row.ID == f.items[2].ID && row.DeletedAt == nil {
			t.Error("deleted link end lost its deletion marker")
		}
		if row.CollectionID != f.item.CollectionID {
			t.Errorf("collection=%q, want %q", row.CollectionID, f.item.CollectionID)
		}
	}
	want := map[string]bool{}
	for _, it := range f.items[:7] {
		want[it.ID] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("access candidates: got %d items, want exactly root, link ends, gate ends and lineage owners (7); unrelated included=%v", len(got), got[f.items[7].ID])
	}
}

func TestTASK4423_UnlinkedAccessLoaderRoot(t *testing.T) {
	for _, door := range []string{"resolve", "renegotiate"} {
		for _, readable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/readable=%v", door, readable), func(t *testing.T) {
				f := newLinkFix(t, 1)
				if door == "renegotiate" {
					f.deadline(t, 0, "stays_owed")
				}
				calls := 0
				loader := func(q Queryer) (models.CommitmentAccess, error) {
					calls++
					if _, ok := q.(*sql.Tx); !ok {
						t.Fatal("loader received pool instead of pinned transaction")
					}
					return models.CommitmentAccess{Readable: map[string]bool{f.item.ID: readable}}, nil
				}
				to := "2030-03-10T09:00:00Z"
				nowT := rt("2029-12-01T00:00:00Z")
				if door == "resolve" {
					until := rt(to)
					_, err := f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, ResolveInput{Actor: "user", State: "move", OccurrenceAt: rt(*f.scheds[0].NextAt), Until: &until, CommitmentAccessLoader: loader}, nowT)
					if err != nil {
						t.Fatalf("authorized unlinked resolve refused root absent from access maps: %v", err)
					}
					if got := f.role(t, 0, "target"); got != to {
						t.Fatalf("target=%s want %s", got, to)
					}
				} else {
					in := models.CommitmentRenegotiateInput{Preview: true, Reason: "Replan", Deadline: &to, CommitmentAccessLoader: loader}
					p, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, nowT, nil)
					if err != nil {
						t.Fatalf("authorized unlinked renegotiate refused root absent from access maps: %v", err)
					}
					in.Preview, in.Revision = false, p.Revision
					if _, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, nowT, nil); err != nil {
						t.Fatal(err)
					}
					if got := f.role(t, 0, "deadline"); got != to {
						t.Fatalf("deadline=%s want %s", got, to)
					}
				}
				wantCalls := 1
				if door == "renegotiate" {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Fatalf("loader calls=%d want %d", calls, wantCalls)
				}
			})
		}
	}
}
