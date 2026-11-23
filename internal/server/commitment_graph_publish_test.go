package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// graphPublishFix is a three-item graph on an event bus. items[2] sits in a
// collection the caller (an editor limited to Tasks) cannot read.
type graphPublishFix struct {
	*graphHTTP
	caller *models.User
	tok    string
	ch     chan events.Event
}

func newGraphPublishFix(t *testing.T) *graphPublishFix {
	t.Helper()
	g := newGraphHTTPOn(t, testServerWithEvents(t), models.CommitmentRolesInput{}, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
	hidden := mustCollection(t, g.srv, g.ws.ID, "Hidden")
	if _, err := g.srv.store.DB().Exec(`UPDATE items SET collection_id=? WHERE id=?`, hidden.ID, g.items[2].ID); err != nil {
		t.Fatal(err)
	}
	u := g.member("graph-publish@example.com", "editor")
	if err := g.srv.store.SetMemberCollectionAccess(g.ws.ID, u.ID, "specific", []string{g.coll.ID}); err != nil {
		t.Fatal(err)
	}
	ch, _, _ := g.srv.events.Subscribe(context.Background(), g.ws.ID)
	t.Cleanup(func() { g.srv.events.Unsubscribe(ch) })
	return &graphPublishFix{graphHTTP: g, caller: u, tok: g.token(u, ""), ch: ch}
}

// drain returns the item_updated events published since the last drain.
func (f *graphPublishFix) drain() []events.Event {
	var out []events.Event
	for {
		select {
		case ev := <-f.ch:
			if ev.Type == events.ItemUpdated {
				out = append(out, ev)
			}
		case <-time.After(150 * time.Millisecond):
			return out
		}
	}
}

// expect drains and checks each named item got exactly one item_updated and
// no other item got any. want holds item indexes.
func (f *graphPublishFix) expect(what string, want ...int) []events.Event {
	f.t.Helper()
	got := f.drain()
	counts := map[string]int{}
	for _, ev := range got {
		counts[ev.ItemID]++
	}
	wantSet := map[string]bool{}
	for _, i := range want {
		wantSet[f.items[i].ID] = true
		if counts[f.items[i].ID] != 1 {
			f.t.Errorf("%s: items[%d] got %d item_updated, want 1", what, i, counts[f.items[i].ID])
		}
	}
	for id, n := range counts {
		if !wantSet[id] {
			f.t.Errorf("%s: unexpected %d item_updated for %s", what, n, id)
		}
	}
	return got
}

// noChangedSetOnWire checks the unfiltered changed set never reaches the body.
func (f *graphPublishFix) noChangedSetOnWire(what string, rr *httptest.ResponseRecorder) {
	f.t.Helper()
	body := rr.Body.String()
	if strings.Contains(body, "changed_item") || strings.Contains(body, "ChangedItemIDs") || strings.Contains(body, f.items[2].ID) {
		f.t.Errorf("%s: the response names the changed set or the hidden item: %s", what, body)
	}
}

// hiddenEventStaysHidden checks the hidden item's event is published on the bus
// and the SSE filter withholds it from the caller, who still gets a readable one.
func (f *graphPublishFix) hiddenEventStaysHidden(got []events.Event) {
	f.t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/events", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, f.caller))
	vis := f.srv.computeSSEVisibility(req, f.ws.ID)
	sawHidden := false
	for _, ev := range got {
		visible := sseEventVisibleFor(vis, f.caller.ID, ev)
		if ev.ItemID == f.items[2].ID {
			sawHidden = true
			if visible {
				f.t.Errorf("the hidden item's event is delivered to a caller who cannot read it")
			}
		} else if !visible {
			f.t.Errorf("the event for %s is withheld from a caller who can read it", ev.ItemID)
		}
	}
	if !sawHidden {
		f.t.Errorf("no event published for the hidden cascaded item")
	}
}

func (f *graphPublishFix) storeLinkID(p, c int) string {
	f.t.Helper()
	f.link(p, c, "target", "target", "")
	var id string
	if err := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(`SELECT l.id FROM schedule_links l JOIN item_schedules p ON p.id=l.parent_schedule_id JOIN item_schedules c ON c.id=l.child_schedule_id WHERE p.item_id=? AND c.item_id=?`), f.items[p].ID, f.items[c].ID).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

const wrongRevision = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestCommitmentGraphWritePublishesItemUpdated(t *testing.T) {
	t.Parallel()

	t.Run("link create", func(t *testing.T) {
		t.Parallel()
		f := newGraphPublishFix(t)
		f.storeLinkID(1, 2)
		f.drain()
		body := map[string]any{"child": f.items[1].ID, "parent_role": "target", "child_role": "target", "offset": map[string]any{"mode": "elapsed", "elapsed_secs": 0}, "preview": true}
		path := f.path(f.items[0]) + "/links"
		if rr := f.as(f.tok, "POST", path, body); rr.Code != 200 {
			t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
		}
		f.expect("link create preview")
		body["preview"] = false
		body["revision"] = wrongRevision
		f.expectErr(f.as(f.tok, "POST", path, body), 409, "commitment_changed", "")
		f.expect("link create refusal")
		delete(body, "revision")
		rr := f.as(f.tok, "POST", path, body)
		if rr.Code != 201 {
			t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
		}
		f.noChangedSetOnWire("link create", rr)
		// items[1] is an end and cascaded: still one event.
		f.hiddenEventStaysHidden(f.expect("link create", 0, 1, 2))
	})

	t.Run("link update", func(t *testing.T) {
		t.Parallel()
		f := newGraphPublishFix(t)
		id := f.storeLinkID(0, 1)
		f.storeLinkID(1, 2)
		f.drain()
		path := f.resourcePath("links", id)
		body := map[string]any{"offset": map[string]any{"mode": "elapsed", "elapsed_secs": 3600}, "preview": true}
		if rr := f.as(f.tok, "PATCH", path, body); rr.Code != 200 {
			t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
		}
		f.expect("link update preview")
		body["preview"] = false
		body["revision"] = wrongRevision
		f.expectErr(f.as(f.tok, "PATCH", path, body), 409, "commitment_changed", "")
		f.expect("link update refusal")
		delete(body, "revision")
		rr := f.as(f.tok, "PATCH", path, body)
		if rr.Code != 200 {
			t.Fatalf("update: %d %s", rr.Code, rr.Body.String())
		}
		f.noChangedSetOnWire("link update", rr)
		f.hiddenEventStaysHidden(f.expect("link update", 0, 1, 2))
	})

	t.Run("link delete", func(t *testing.T) {
		t.Parallel()
		f := newGraphPublishFix(t)
		id := f.storeLinkID(0, 1)
		f.drain()
		f.expectErr(f.as(f.tok, "DELETE", f.resourcePath("links", "00000000-0000-0000-0000-000000000000"), nil), 404, "schedule_link_not_found", "")
		f.expect("link delete refusal")
		if rr := f.as(f.tok, "DELETE", f.resourcePath("links", id), nil); rr.Code != 204 || rr.Body.Len() != 0 {
			t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
		}
		f.expect("link delete", 0, 1)
	})

	t.Run("link delete with a soft-deleted end", func(t *testing.T) {
		t.Parallel()
		f := newGraphPublishFix(t)
		id := f.storeLinkID(0, 1)
		if _, err := f.srv.store.DB().Exec(`UPDATE items SET deleted_at='2026-01-01T00:00:00Z' WHERE id=?`, f.items[1].ID); err != nil {
			t.Fatal(err)
		}
		f.drain()
		if rr := f.as(f.token(f.owner, ""), "DELETE", f.resourcePath("links", id), nil); rr.Code != 204 || rr.Body.Len() != 0 {
			t.Fatalf("owner delete: %d %s", rr.Code, rr.Body.String())
		}
		f.expect("link delete, deleted child", 0)
	})

	t.Run("gate create", func(t *testing.T) {
		t.Parallel()
		f := newGraphPublishFix(t)
		path := f.path(f.items[0]) + "/gates"
		f.expectErr(f.as(f.tok, "POST", path, map[string]any{"blocker": f.items[2].ID}), 404, "schedule_link_not_found", "")
		f.expect("gate create refusal")
		rr := f.as(f.tok, "POST", path, map[string]any{"blocker": f.items[1].ID})
		if rr.Code != 201 {
			t.Fatalf("gate: %d %s", rr.Code, rr.Body.String())
		}
		f.expect("gate create", 0, 1)
	})

	t.Run("gate delete", func(t *testing.T) {
		t.Parallel()
		f := newGraphPublishFix(t)
		if _, err := f.srv.store.CreateScheduleGate(f.ws.ID, f.items[0].ID, f.items[1].ID, f.access); err != nil {
			t.Fatal(err)
		}
		id := f.resourceID("gates")
		f.drain()
		if rr := f.as(f.tok, "DELETE", f.resourcePath("gates", id), nil); rr.Code != 204 || rr.Body.Len() != 0 {
			t.Fatalf("delete gate: %d %s", rr.Code, rr.Body.String())
		}
		f.expect("gate delete", 0, 1)
	})

	t.Run("move", func(t *testing.T) {
		t.Parallel()
		f := newGraphPublishFix(t)
		f.storeLinkID(0, 1)
		f.storeLinkID(1, 2)
		f.drain()
		base, _ := time.Parse(time.RFC3339, f.dates[0])
		move := map[string]any{"role": "target", "to": base.Add(3 * time.Hour).Format(time.RFC3339), "reason": "Move"}
		path := f.path(f.items[0])
		if rr := f.as(f.tok, "POST", path+"/move-preview", move); rr.Code != 200 {
			t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
		}
		f.expect("move-preview")
		move["revision"] = wrongRevision
		f.expectErr(f.as(f.tok, "POST", path+"/move", move), 409, "commitment_changed", "")
		f.expect("move refusal")
		delete(move, "revision")
		rr := f.as(f.tok, "POST", path+"/move", move)
		if rr.Code != 200 {
			t.Fatalf("move: %d %s", rr.Code, rr.Body.String())
		}
		f.noChangedSetOnWire("move", rr)
		f.hiddenEventStaysHidden(f.expect("move", 0, 1, 2))
	})
}
