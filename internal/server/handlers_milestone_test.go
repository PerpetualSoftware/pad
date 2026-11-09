package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4358: the milestone write doors and the reader-scoped read.

func (f *schedHTTP) ms(suffix string) string { return f.path(f.item) + suffix }

func (f *schedHTTP) kid(title string) *models.Item {
	return mustItem(f.t, f.srv, f.ws.ID, f.coll.ID, title)
}

func (f *schedHTTP) milestone() *schedHTTP {
	f.create(okSchedule)
	return f
}

func (f *schedHTTP) owned(method, path string, body any) *httptest.ResponseRecorder {
	return f.as(f.token(f.owner, ""), method, path, body)
}

func commitmentOf(t *testing.T, rr *httptest.ResponseRecorder) models.Commitment {
	t.Helper()
	return readCommitment(t, rr, http.StatusOK)
}

func TestMilestoneHTTPHappyPath(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t).milestone()
	kid := f.kid("Kid")
	c := commitmentOf(t, f.owned("PUT", f.ms("/forecast"), map[string]any{"value": "2026-12-01", "source": "plan"}))
	if c.Forecast == nil || c.Forecast.Value != "2026-12-01" || c.Forecast.Source != "plan" || c.Forecast.SetByKind != "user" || c.Forecast.SetBy != f.owner.ID {
		t.Fatalf("forecast: %+v", c.Forecast)
	}
	c = commitmentOf(t, f.owned("PUT", f.ms("/forecast"), map[string]any{"value": "2026-12-09T10:00:00Z"}))
	if len(c.Forecast.History) != 2 {
		t.Fatalf("history: %+v", c.Forecast.History)
	}
	c = commitmentOf(t, f.owned("PUT", f.ms("/children/"+kid.Slug), map[string]any{"kind": "child"}))
	if c.Milestone.Total != 1 || c.Milestone.Children[0].ItemRef == "" || c.Milestone.Result == nil || *c.Milestone.Result != "pending" {
		t.Fatalf("selected: %+v", c.Milestone)
	}
	c = commitmentOf(t, f.owned("PUT", f.ms("/children/"+kid.Slug), map[string]any{"kind": "waiver", "reason": "n/a"}))
	if c.Milestone.Waivers != 1 || *c.Milestone.Result != "completed" {
		t.Fatalf("waived: %+v", c.Milestone)
	}
	c = commitmentOf(t, f.owned("DELETE", f.ms("/children/"+kid.Slug), nil))
	if c.Milestone.Total != 0 || len(c.Milestone.History) != 1 {
		t.Fatalf("removed: %+v", c.Milestone)
	}
	c = commitmentOf(t, f.owned("DELETE", f.ms("/forecast"), nil))
	if c.Forecast != nil {
		t.Fatalf("forecast survives a DELETE: %+v", c.Forecast)
	}
	if n := f.count(`SELECT COUNT(*) FROM activities WHERE action = 'schedule_updated' AND metadata NOT LIKE '%Kid%'`); n == 0 {
		t.Error("no names-only activity recorded")
	}
}

// RED if a child ref's answer depends on whether it exists or may be read, or
// if validation runs before the visibility gate.
func TestMilestoneChildRefsAnswerByteIdentical404(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t).milestone()
	guest := mustUser(t, f.srv, "ms-guest@example.com", "msguest", "")
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.item.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	hidden := f.kid("Hidden")
	otherWS := mustWorkspace(t, f.srv, "Elsewhere", f.owner.ID)
	otherColl := mustCollection(t, f.srv, otherWS.ID, "Tasks")
	foreign := mustItem(t, f.srv, otherWS.ID, otherColl.ID, "Foreign")
	gt := f.token(guest, "")
	var bodies []string
	for _, ref := range []string{"nope-999", hidden.Slug, foreign.Slug} {
		for _, body := range []any{map[string]any{"kind": "child"}, map[string]any{"kind": "bogus"}, map[string]any{"kind": "waiver"}} {
			rr := f.as(gt, "PUT", f.ms("/children/"+ref), body)
			if rr.Code != 404 {
				t.Fatalf("%s %v: %d %s", ref, body, rr.Code, rr.Body.String())
			}
			bodies = append(bodies, rr.Body.String())
		}
		rr := f.as(gt, "DELETE", f.ms("/children/"+ref), nil)
		if rr.Code != 404 {
			t.Fatalf("DELETE %s: %d", ref, rr.Code)
		}
		bodies = append(bodies, rr.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("404 bodies differ:\n%s\n%s", bodies[0], b)
		}
	}
	// The owner sees the hidden one as a normal child.
	f.owned("PUT", f.ms("/children/"+hidden.Slug), map[string]any{"kind": "child"})
	if f.count(`SELECT COUNT(*) FROM milestone_children`) != 1 {
		t.Fatal("owner could not select the child")
	}
	// The unknown milestone item and an invisible one answer alike.
	a := f.as(gt, "PUT", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+hidden.Slug+"/commitment/forecast", map[string]any{"value": "2026-12-01"})
	b := f.as(gt, "PUT", "/api/v1/workspaces/"+f.ws.Slug+"/items/nope-999/commitment/forecast", map[string]any{"value": "2026-12-01"})
	if a.Code != 404 || a.Body.String() != b.Body.String() {
		t.Fatalf("milestone item 404s differ: %d %s | %d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
}

// The reader never learns a hidden child's ref, kind or state; the answer is
// the same whether it is open or done.
func TestMilestoneGETWithHiddenChildIsUnknown(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t).milestone()
	guest := mustUser(t, f.srv, "ms-reader@example.com", "msreader", "")
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.item.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	hidden := f.kid("Hidden")
	f.owned("PUT", f.ms("/children/"+hidden.Slug), map[string]any{"kind": "child"})
	read := func() string {
		rr := f.as(f.token(guest, ""), "GET", f.ms(""), nil)
		if rr.Code != 200 {
			t.Fatalf("GET %d", rr.Code)
		}
		return rr.Body.String()
	}
	before := read()
	c := readCommitment(t, f.as(f.token(guest, ""), "GET", f.ms(""), nil), 200)
	if c.Milestone.Result == nil || *c.Milestone.Result != "unknown" || c.Milestone.Total != 0 || c.Milestone.EvidenceChanged != nil {
		t.Fatalf("guest milestone: %+v", c.Milestone)
	}
	ref := commitmentOf(t, f.owned("GET", f.ms(""), nil)).Milestone.Children[0].ItemRef
	if ref == "" || strings.Contains(before, ref) || strings.Contains(before, hidden.Slug) || strings.Contains(before, hidden.ID) {
		t.Fatalf("hidden ref %q leaked: %s", ref, before)
	}
	if _, err := f.srv.store.UpdateItem(hidden.ID, models.ItemUpdate{Fields: msStr(`{"status":"done"}`)}); err != nil {
		t.Fatal(err)
	}
	if after := read(); after != before {
		t.Fatalf("the reader's response moved with a hidden child:\n%s\n%s", before, after)
	}
	own := commitmentOf(t, f.owned("GET", f.ms(""), nil))
	if own.Milestone.Result == nil || *own.Milestone.Result == "unknown" || own.Milestone.Total != 1 {
		t.Fatalf("owner: %+v", own.Milestone)
	}
}

func TestMilestoneAuthArchivedAndMissingSchedule(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	kid := f.kid("Kid")
	forecast := map[string]any{"value": "2026-12-01"}
	// No schedule: 404 Schedule not found.
	for _, call := range []struct{ method, suffix string }{{"PUT", "/forecast"}, {"DELETE", "/forecast"}, {"PUT", "/children/" + kid.Slug}, {"DELETE", "/children/" + kid.Slug}} {
		rr := f.owned(call.method, f.ms(call.suffix), map[string]any{"value": "2026-12-01", "kind": "child"})
		f.expectErr(rr, 404, "not_found", "")
		if !strings.Contains(rr.Body.String(), "Schedule not found") {
			t.Errorf("%s %s: %s", call.method, call.suffix, rr.Body.String())
		}
	}
	f.milestone()
	viewer := f.member("ms-viewer@example.com", "viewer")
	for _, call := range []struct{ method, suffix string }{{"PUT", "/forecast"}, {"DELETE", "/forecast"}, {"PUT", "/children/" + kid.Slug}, {"DELETE", "/children/" + kid.Slug}} {
		f.expectErr(f.as(f.token(viewer, ""), call.method, f.ms(call.suffix), forecast), 403, "forbidden", "")
		f.expectErr(f.as(f.token(f.owner, `["read"]`), call.method, f.ms(call.suffix), forecast), 403, "forbidden", "")
	}
	if n := f.count(`SELECT COUNT(*) FROM schedule_forecast_events`); n != 0 {
		t.Fatalf("refused writes recorded %d events", n)
	}
	if err := f.srv.store.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct{ method, suffix string }{{"PUT", "/forecast"}, {"DELETE", "/forecast"}, {"PUT", "/children/" + kid.Slug}, {"DELETE", "/children/" + kid.Slug}} {
		f.expectErr(f.owned(call.method, f.ms(call.suffix), map[string]any{"value": "2026-12-01", "kind": "child"}), 409, "archived", "")
	}
}

func TestMilestoneHTTPValidation(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t).milestone()
	kid := f.kid("Kid")
	cases := []struct {
		name, method, suffix string
		body                 any
		status               int
		code, detail         string
	}{
		{"forecast value required", "PUT", "/forecast", map[string]any{"source": "x"}, 400, "validation_error", "required"},
		{"forecast value null", "PUT", "/forecast", `{"value":null}`, 400, "validation_error", "required"},
		{"forecast bad format", "PUT", "/forecast", map[string]any{"value": "soon"}, 400, "validation_error", "format"},
		{"forecast source cap", "PUT", "/forecast", map[string]any{"value": "2026-12-01", "source": strings.Repeat("x", 281)}, 400, "validation_error", "range"},
		{"forecast unknown member", "PUT", "/forecast", map[string]any{"value": "2026-12-01", "bogus": 1}, 400, "invalid_body", ""},
		{"child unknown member", "PUT", "/children/" + kid.Slug, map[string]any{"kind": "child", "bogus": 1}, 400, "invalid_body", ""},
		{"child kind required", "PUT", "/children/" + kid.Slug, map[string]any{}, 400, "validation_error", "required"},
		{"child kind enum", "PUT", "/children/" + kid.Slug, map[string]any{"kind": "other"}, 400, "validation_error", "enum"},
		{"waiver without reason", "PUT", "/children/" + kid.Slug, map[string]any{"kind": "waiver"}, 400, "validation_error", "combination"},
		{"self child", "PUT", "/children/" + f.item.Slug, map[string]any{"kind": "child"}, 400, "validation_error", "combination"},
	}
	for _, tc := range cases {
		rr := f.owned(tc.method, f.ms(tc.suffix), tc.body)
		f.expectErr(rr, tc.status, tc.code, tc.detail)
	}
	if f.count(`SELECT COUNT(*) FROM schedule_forecasts`)+f.count(`SELECT COUNT(*) FROM milestone_children`) != 0 {
		t.Fatal("a refused write stored a row")
	}
}

// Over the cap is refused whole as a range, naming the cap and no hidden count.
func TestMilestoneHTTPChildCap(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t).milestone()
	var last *models.Item
	for i := 0; i < 201; i++ {
		last = f.kid("c")
		if i < 200 {
			if err := f.srv.store.SetMilestoneChild(f.ws.ID, f.item.ID, last.ID, "child", "", f.owner.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	rr := f.owned("PUT", f.ms("/children/"+last.Slug), map[string]any{"kind": "child"})
	f.expectErr(rr, 400, "validation_error", "range")
	if !strings.Contains(rr.Body.String(), "200") {
		t.Errorf("message should name the cap: %s", rr.Body.String())
	}
	if n := f.count(`SELECT COUNT(*) FROM milestone_children`); n != 200 {
		t.Fatalf("selection = %d", n)
	}
}

func msStr(s string) *string { return &s }
