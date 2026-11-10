package store

import (
	"database/sql"
	"errors"
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
					if mode == "nil maps" {
						want = ErrScheduleLinkNotFound
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
