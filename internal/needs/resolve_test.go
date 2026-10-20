package needs

import (
	"strings"
	"testing"
	"time"
)

type fakeElig map[string]bool

func (f fakeElig) Eligible(userID, _, _ string) bool { return f[userID] }

func TestResolve_Order(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	mention := func(u string, d int) Mention {
		return Mention{UserID: u, CommentID: "c-" + u, By: "Commenter-" + u, At: t0.Add(time.Duration(d) * day)}
	}
	flagged := t0.Add(5 * day)
	cases := []struct {
		name     string
		cand     Candidate
		owners   []string
		elig     fakeElig
		wantRule string
		want     []string
		waiting  time.Time
	}{
		{"asked beats mentioned", Candidate{AwaitsHuman: true, AssigneeUserID: "u-asg", CreatedAt: t0, Mentions: []Mention{mention("u-men", 1)}},
			[]string{"u-own"}, fakeElig{"u-asg": true, "u-men": true, "u-own": true}, RuleAsked, []string{"u-asg"}, t0},
		{"mentioned beats assigned", Candidate{AssigneeUserID: "u-asg", CreatedAt: t0, AssignedAt: t0, Mentions: []Mention{mention("u-men", 2)}},
			[]string{"u-own"}, fakeElig{"u-asg": true, "u-men": true, "u-own": true}, RuleMentioned, []string{"u-men"}, t0.Add(2 * day)},
		{"assigned beats owners", Candidate{AssigneeUserID: "u-asg", CreatedAt: t0, AssignedAt: t0.Add(3 * day)},
			[]string{"u-own"}, fakeElig{"u-asg": true, "u-own": true}, RuleAssigned, []string{"u-asg"}, t0.Add(3 * day)},
		{"owners are the last resort", Candidate{CreatedAt: t0}, []string{"u-own", "u-own2"},
			fakeElig{"u-own": true, "u-own2": true}, RuleAdminFallback, []string{"u-own", "u-own2"}, t0},
		{"ineligible asked falls through to mentioned", Candidate{AwaitsHuman: true, AssigneeUserID: "u-asg", CreatedAt: t0, Mentions: []Mention{mention("u-men", 1)}},
			[]string{"u-own"}, fakeElig{"u-men": true, "u-own": true}, RuleMentioned, []string{"u-men"}, t0.Add(day)},
		{"an awaits item with no assignee is not asked", Candidate{AwaitsHuman: true, CreatedAt: t0},
			[]string{"u-own"}, fakeElig{"u-own": true}, RuleAdminFallback, []string{"u-own"}, t0},
		{"a role-only assignee falls through", Candidate{AssigneeUserID: "", CreatedAt: t0},
			[]string{"u-own"}, fakeElig{"u-own": true}, RuleAdminFallback, []string{"u-own"}, t0},
		{"only the winning rule's people", Candidate{CreatedAt: t0, Mentions: []Mention{mention("a", 1), mention("b", 2), mention("x", 3)}, AssigneeUserID: "u-asg"},
			[]string{"u-own"}, fakeElig{"a": true, "b": true, "u-asg": true, "u-own": true}, RuleMentioned, []string{"a", "b"}, t0.Add(day)},
		{"needs_human moves WaitingSince to the earlier claim", Candidate{AssigneeUserID: "u-asg", CreatedAt: t0, AssignedAt: t0.Add(30 * day), NeedsHuman: &flagged},
			nil, fakeElig{"u-asg": true}, RuleAssigned, []string{"u-asg"}, flagged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Resolve(tc.cand, tc.owners, tc.elig)
			if !ok {
				t.Fatal("dropped")
			}
			if got.Rule != tc.wantRule || strings.Join(got.People, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("rule %q people %v, want %q %v", got.Rule, got.People, tc.wantRule, tc.want)
			}
			if !got.WaitingSince.Equal(tc.waiting) {
				t.Fatalf("WaitingSince %v, want %v", got.WaitingSince, tc.waiting)
			}
			if got.Why.Rule != tc.wantRule || got.Why.Text == "" || got.Why.At == nil {
				t.Fatalf("why = %+v", got.Why)
			}
			for _, skipped := range []string{"u-asg", "u-own", "u-men"} {
				if !tc.elig[skipped] && strings.Contains(got.Why.Text, skipped) {
					t.Fatalf("why.text names a skipped user %q: %q", skipped, got.Why.Text)
				}
			}
		})
	}

	t.Run("no eligible person drops the item", func(t *testing.T) {
		c := Candidate{AwaitsHuman: true, AssigneeUserID: "u-asg", Mentions: []Mention{mention("u-men", 1)}}
		if r, ok := Resolve(c, []string{"u-own"}, fakeElig{}); ok {
			t.Fatalf("an item nobody may see was routed to %v", r.People)
		}
	})
	t.Run("because lists the claims", func(t *testing.T) {
		got, _ := Resolve(Candidate{AwaitsHuman: true, AssigneeUserID: "u-asg", CreatedAt: t0, NeedsHuman: &flagged}, nil, fakeElig{"u-asg": true})
		if len(got.Because) != 2 || got.Because[0].Kind != BecauseHumanTask || got.Because[1].Kind != BecauseNeedsHuman {
			t.Fatalf("because = %+v", got.Because)
		}
		m, _ := Resolve(Candidate{Mentions: []Mention{mention("u-men", 1)}}, nil, fakeElig{"u-men": true})
		if m.Why.By != "Commenter-u-men" || m.Why.CommentID != "c-u-men" || m.Because[0].Kind != BecauseMention {
			t.Fatalf("mention why/because = %+v %+v", m.Why, m.Because)
		}
	})
}

func TestCursor(t *testing.T) {
	asOf := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	r := &Row{ItemID: "i1", WaitingSince: asOf.Add(-time.Hour), Item: ItemInfo{CreatedAt: asOf.Add(-2 * time.Hour)}}
	tok := NewCursor(SortAge, "active", "", asOf, r).Encode()
	c, err := DecodeCursor(tok, SortAge, "active", "")
	if err != nil || c.Key() != KeyOf(r) || !c.At().Equal(asOf) {
		t.Fatalf("round trip: %+v %v", c, err)
	}
	for name, args := range map[string][3]string{"view": {SortAge, "hidden", ""}, "ws": {SortAge, "active", "other"}, "sort": {"x", "active", ""}} {
		if _, err := DecodeCursor(tok, args[0], args[1], args[2]); err != ErrInvalidCursor {
			t.Fatalf("a cursor for another %s was accepted", name)
		}
	}
	for _, bad := range []string{"", "!!", "e30", "bm90LWpzb24"} {
		if _, err := DecodeCursor(bad, SortAge, "active", ""); err != ErrInvalidCursor {
			t.Fatalf("%q accepted", bad)
		}
	}
	a, b := Key{asOf, asOf, "a"}, Key{asOf, asOf, "b"}
	if !a.Less(b) || b.Less(a) || a.Less(a) {
		t.Fatal("ties must break by item id")
	}
}
