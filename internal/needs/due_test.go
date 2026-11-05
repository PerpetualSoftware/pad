package needs

import (
	"testing"
	"time"
)

func TestResolveDueOrdering(t *testing.T) {
	due := time.Date(2031, 3, 10, 9, 0, 0, 0, time.UTC)
	later := due.Add(time.Hour)
	for _, tc := range []struct {
		name                                 string
		asked, mentioned, assigned, eligible bool
		rule                                 string
	}{
		{"asked", true, true, true, true, RuleAsked},
		{"mentioned", false, true, true, true, RuleMentioned},
		{"assigned", false, false, true, true, RuleAssigned},
		{"due", false, false, false, true, RuleDue},
		{"fallback", false, false, false, false, RuleAdminFallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Candidate{CreatedAt: later, AssignedAt: later, Due: &due, DueRecipientID: "recipient", AwaitsHuman: tc.asked}
			if tc.assigned {
				c.AssigneeUserID = "recipient"
			}
			if tc.mentioned {
				c.Mentions = []Mention{{UserID: "recipient", At: later}}
			}
			r, ok := Resolve(c, []string{"owner"}, fakeElig{"recipient": tc.eligible, "owner": true})
			if !ok || r.Rule != tc.rule {
				t.Fatalf("routing = %+v, ok=%v", r, ok)
			}
			person := "recipient"
			want := due
			if !tc.eligible {
				person = "owner"
				want = later
			}
			own := r.For(person)
			if !own.WaitingSince.Equal(want) {
				t.Fatalf("waiting = %s, want %s", own.WaitingSince, want)
			}
			n := 0
			for _, b := range own.Because {
				if b.Kind == BecauseDue {
					n++
					if !b.At.Equal(due) {
						t.Fatalf("due = %+v", b)
					}
				}
			}
			wantN := 1
			if !tc.eligible {
				wantN = 0
			}
			if n != wantN {
				t.Fatalf("due claims=%d want %d", n, wantN)
			}
			if tc.rule == RuleDue && (own.Why.Text != "A reminder of yours is due." || !own.Why.At.Equal(due)) {
				t.Fatalf("why = %+v", own.Why)
			}
		})
	}
}

func TestResolveDueMentionedPerPerson(t *testing.T) {
	due := time.Date(2031, 3, 10, 9, 0, 0, 0, time.UTC)
	mention := due.Add(time.Hour)
	c := Candidate{Due: &due, DueRecipientID: "recipient", Mentions: []Mention{{UserID: "other", At: mention}, {UserID: "recipient", At: mention}}}
	r, ok := Resolve(c, nil, fakeElig{"recipient": true, "other": true})
	if !ok || r.Rule != RuleMentioned {
		t.Fatalf("routing = %+v", r)
	}
	for _, uid := range []string{"recipient", "other"} {
		own := r.For(uid)
		want := mention
		n := 1
		if uid == "recipient" {
			want = due
			n = 2
		}
		if !own.WaitingSince.Equal(want) || len(own.Because) != n {
			t.Fatalf("%s = %+v", uid, own)
		}
		for _, b := range own.Because {
			if b.Kind == BecauseDue && uid != "recipient" {
				t.Fatalf("due leaked to %s", uid)
			}
		}
	}
}
