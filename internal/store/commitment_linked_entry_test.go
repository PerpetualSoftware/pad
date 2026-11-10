package store

import (
	"encoding/json"
	"testing"
	"time"
)

// A dependent the caller cannot read must not change a renegotiation's answer.
func TestLinkedOccurrenceRenegotiationIsIdenticalWithAHiddenDependent(t *testing.T) {
	answer := func(linked bool) string {
		f := deadlineLinkFixture(t, 2)
		if linked {
			f.linkRaw(t, 0, 1, "deadline", "target", oneDay)
		}
		in := linkedRenegotiationInput()
		root := f.items[0].ID
		in.Access.Readable, in.Access.Editable = map[string]bool{root: true}, map[string]bool{root: true}
		p, err := f.renegotiateLinked(0, in)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Revision) != 43 {
			t.Fatalf("revision length %d", len(p.Revision))
		}
		p.Revision = ""
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if with, without := answer(true), answer(false); with != without {
		t.Fatalf("a hidden dependent changed the preview:\n%s\n%s", with, without)
	}
}

// The occurrence row is keyed by the recipient's instant, whichever zone the
// caller named the occurrence in.
func TestLinkedResolveFloatingCommentUsesTheRecipientsOccurrence(t *testing.T) {
	f := newLinkFix(t, 2)
	f.exec(t, `UPDATE item_schedules SET floating=?, tz=NULL WHERE id=?`, true, f.scheds[0].ID)
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	u := f.user(t, "linked-floating@example.com", "UTC")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	seen, ok, err := OutstandingInstant(f.reload(t, f.items[0].ID), ny)
	if err != nil || !ok {
		t.Fatalf("viewer reading: %v %v", ok, err)
	}
	res, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ResolveInput{State: "move", OccurrenceAt: seen, View: ny, Until: ptr(rt("2030-03-10T09:00:00Z")), Actor: "user", Comment: f.comment(f.items[0], u.ID, "Plans changed")}, rt("2030-03-03T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, f.scheds[0].ID); n != 1 {
		t.Fatalf("occurrence rows = %d, want 1", n)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND comment_id=?`, f.scheds[0].ID, res.Comment.ID) != 1 {
		t.Fatal("the moved occurrence lost its comment")
	}
}
