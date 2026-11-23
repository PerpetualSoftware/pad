package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ChangedItemIDs names every item whose date the cascade changed, readable or
// not, once each, and never serializes: Changes alone stays filtered.
func TestLinkedResultChangedItemIDsIncludeUnreadable(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	acc := f.allEdit()
	delete(acc.Readable, f.items[2].ID)

	assertIDs := func(name string, res *models.LinkedMoveResult, want []string) {
		t.Helper()
		if !reflect.DeepEqual(res.ChangedItemIDs, want) {
			t.Fatalf("%s: ChangedItemIDs=%v, want %v", name, res.ChangedItemIDs, want)
		}
		for _, ch := range res.Changes {
			if ch.ItemID == f.items[2].ID {
				t.Fatalf("%s: Changes names the unreadable item: %+v", name, res.Changes)
			}
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		bare := *res
		bare.ChangedItemIDs = nil
		if b2, _ := json.Marshal(&bare); string(b2) != string(b) {
			t.Fatalf("%s: the changed set moved the bytes:\n%s\n%s", name, b, b2)
		}
		if strings.Contains(string(b), "changed_item") || strings.Contains(string(b), "ChangedItemIDs") || strings.Contains(string(b), f.items[2].ID) {
			t.Fatalf("%s: the changed set serialized: %s", name, b)
		}
	}

	res, err := f.move(0, "target", "2030-03-10T09:00:00Z", func(in *models.LinkedMoveInput) { in.Access = acc })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("changes=%+v, want the root and the readable child", res.Changes)
	}
	assertIDs("move", res, []string{f.items[0].ID, f.items[1].ID, f.items[2].ID})
	if got := f.role(t, 2, "target"); got != "2030-03-12T09:00:00Z" {
		t.Fatalf("hidden grandchild target=%s", got)
	}

	// A link write's changed set starts at the child: the parent stays.
	links, err := f.s.ListScheduleLinks(f.ws.ID, f.items[0].ID, f.allEdit())
	if err != nil || len(links) != 1 {
		t.Fatalf("links=%+v %v", links, err)
	}
	two := models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 2 * 86400}
	upd, err := f.s.UpdateScheduleLink(f.ws.ID, links[0].ID, models.ScheduleLinkUpdate{Offset: &two, Move: models.LinkWriteOptions{AcknowledgeViolations: true}}, acc)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("link update", &upd.Moved, []string{f.items[1].ID, f.items[2].ID})

	// A preview carries the same set; nothing moved, so the caller ignores it.
	pvw, err := f.move(0, "target", "2030-03-20T09:00:00Z", func(in *models.LinkedMoveInput) { in.Access = acc; in.Preview = true })
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("preview", pvw, []string{f.items[0].ID, f.items[1].ID, f.items[2].ID})
}
