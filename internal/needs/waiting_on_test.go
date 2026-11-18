package needs

import (
	"testing"
	"time"
)

// TASK-4643: WaitingOn picks what an item waits on; Deliver maps it to people.

// An ask owned by a seat with no eligible person waits on the role, and a
// role delivers to no human: the item sits in the seat's queue, not the
// owners'.
func TestWaitingOnRoleOnlyAsk(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []Candidate{
		{ItemID: "i", AwaitsHuman: true, AgentRoleID: "role-1", CreatedAt: t0},
		{ItemID: "i", WaitingStatus: true, AgentRoleID: "role-1", CreatedAt: t0},
	} {
		elig := fakeElig{"u-own": true}
		tg := WaitingOn(c, elig)
		if tg.Kind != TargetRole || tg.Rule != RuleRole {
			t.Fatalf("target = %+v, want role", tg)
		}
		if r, ok := Deliver(c, tg, []string{"u-own"}, elig); ok || len(r.People) != 0 {
			t.Fatalf("a role target was delivered to %v", r.People)
		}
		if r, ok := Resolve(c, []string{"u-own"}, elig); ok || len(r.People) != 0 {
			t.Fatalf("Resolve routed a role-only ask to %v", r.People)
		}
	}
}

// The owners fallback is for asks only: an item that is not an ask, with no
// eligible person and no role, waits on nobody and reaches no owner.
func TestWaitingOnFallbackRequiresAsk(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	elig := fakeElig{"u-own": true}
	c := Candidate{ItemID: "i", CreatedAt: t0, AssigneeUserID: "u-gone",
		Mentions: []Mention{{UserID: "u-out", CommentID: "c", At: t0}}}
	tg := WaitingOn(c, elig)
	if tg.Kind != TargetNone || tg.Rule != RuleNone || len(tg.IDs) != 0 {
		t.Fatalf("target = %+v, want none", tg)
	}
	if r, ok := Resolve(c, []string{"u-own"}, elig); ok || len(r.People) != 0 {
		t.Fatalf("a non-ask fell to the owners: %v", r.People)
	}
}

// An ask whose assignee is not eligible falls to its role when it has one,
// else to the eligible owners.
func TestWaitingOnIneligibleAssigneeAskFallsBack(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	elig := fakeElig{"u-own": true}
	withRole := Candidate{ItemID: "i", AwaitsHuman: true, AssigneeUserID: "u-gone", AgentRoleID: "role-1", CreatedAt: t0}
	if tg := WaitingOn(withRole, elig); tg.Kind != TargetRole || tg.Rule != RuleRole {
		t.Fatalf("with a role: target = %+v, want role", tg)
	}
	bare := Candidate{ItemID: "i", WaitingStatus: true, AssigneeUserID: "u-gone", CreatedAt: t0}
	tg := WaitingOn(bare, elig)
	if tg.Kind != TargetOwners || tg.Rule != RuleAdminFallback {
		t.Fatalf("without a role: target = %+v, want owners", tg)
	}
	r, ok := Deliver(bare, tg, []string{"u-own", "u-gone-owner"}, elig)
	if !ok || len(r.People) != 1 || r.People[0] != "u-own" || r.Rule != RuleAdminFallback {
		t.Fatalf("owners delivery = %+v ok=%v", r, ok)
	}
	if r.Target.Kind != TargetOwners {
		t.Fatalf("routing target = %+v", r.Target)
	}
}

// An eligible assigned person on an ask is asked, whatever else holds; the
// target names that person only.
func TestWaitingOnAskedPerson(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	c := Candidate{ItemID: "i", WaitingStatus: true, AssigneeUserID: "u-asg", AgentRoleID: "role-1", CreatedAt: t0,
		Mentions: []Mention{{UserID: "u-men", CommentID: "c", At: t0}}}
	tg := WaitingOn(c, fakeElig{"u-asg": true, "u-men": true})
	if tg.Kind != TargetPerson || tg.Rule != RuleAsked || len(tg.IDs) != 1 || tg.IDs[0] != "u-asg" {
		t.Fatalf("target = %+v", tg)
	}
}

// A due reminder names exactly one recipient, so it waits on a person, like
// asked and assigned.
func TestWaitingOnDueIsPerson(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	c := Candidate{ItemID: "i", CreatedAt: t0, Due: &t0, DueRecipientID: "u-due"}
	tg := WaitingOn(c, fakeElig{"u-due": true})
	if tg.Kind != TargetPerson || tg.Rule != RuleDue || len(tg.IDs) != 1 || tg.IDs[0] != "u-due" {
		t.Fatalf("target = %+v, want person/due", tg)
	}
}
