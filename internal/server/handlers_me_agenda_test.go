package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

var meAgendaNow = time.Date(2031, 3, 10, 12, 0, 0, 0, time.UTC)

func meAgendaGet(t *testing.T, w *agendaWorld, u *models.User, query string, mods ...func(context.Context) context.Context) *httptest.ResponseRecorder {
	t.Helper()
	if w.srv.agendaClock == nil {
		w.srv.agendaClock = func() time.Time { return meAgendaNow }
	}
	req := httptest.NewRequest("GET", "/api/v1/me/agenda?"+query, nil)
	ctx := req.Context()
	if u != nil {
		ctx = WithCurrentUser(ctx, u)
	}
	for _, mod := range mods {
		ctx = mod(ctx)
	}
	rr := httptest.NewRecorder()
	w.srv.ensureRouter()
	w.srv.router.ServeHTTP(rr, req.WithContext(ctx))
	return rr
}

func meAgendaOK(t *testing.T, w *agendaWorld, u *models.User, query string, mods ...func(context.Context) context.Context) map[string]any {
	t.Helper()
	rr := meAgendaGet(t, w, u, query, mods...)
	if rr.Code != http.StatusOK {
		t.Fatalf("agenda: %d %s", rr.Code, rr.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func meAgendaExec(t *testing.T, w *agendaWorld, q string, args ...any) {
	t.Helper()
	if _, err := w.srv.store.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func meAgendaTransition(t *testing.T, w *agendaWorld, it *models.Item, at, status string) {
	t.Helper()
	meAgendaExec(t, w, `INSERT INTO status_transitions (id,item_id,workspace_id,collection_id,field_key,from_status,to_status,created_at) VALUES (?,?,?,?, 'status','open',?,?)`, it.ID+at, it.ID, w.ws.ID, it.CollectionID, status, at)
}

func meAgendaFacts(t *testing.T, w *agendaWorld, coll, title string) *models.Item {
	t.Helper()
	it := w.item(t, coll, title, `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	meAgendaExec(t, w, `UPDATE items SET created_at = ? WHERE id = ?`, "2031-03-10T01:00:00Z", it.ID)
	meAgendaTransition(t, w, it, "2031-03-10T02:00:00Z", "done")
	return it
}

func TestMeAgendaRefusesApp(t *testing.T) {
	w := newAgendaWorld(t)
	app := func(ctx context.Context) context.Context { return context.WithValue(ctx, appCtxKey{}, &appContext{}) }
	rr := meAgendaGet(t, w, w.owner, "", app)
	want := newNfx(t, w.srv).get(w.owner, "", app)
	if rr.Code != 404 || !bytes.Equal(rr.Body.Bytes(), want.Body.Bytes()) {
		t.Fatalf("app refusal = %d %q; needs = %q", rr.Code, rr.Body.String(), want.Body.String())
	}
}

func TestMeAgendaRequiresAuth(t *testing.T) {
	w := newAgendaWorld(t)
	rr := meAgendaGet(t, w, nil, agendaWindow)
	if rr.Code != 401 {
		t.Fatalf("no user = %d %s", rr.Code, rr.Body.String())
	}
	meAgendaOK(t, w, w.owner, agendaWindow)
}

func meAgendaWorkspace(t *testing.T, w *agendaWorld, name string) *agendaWorld {
	t.Helper()
	ws := mustWorkspace(t, w.srv, name, w.owner.ID)
	c, err := w.srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: agendaSchema})
	if err != nil {
		t.Fatal(err)
	}
	return &agendaWorld{srv: w.srv, ws: ws, owner: w.owner, colls: map[string]*models.Collection{"tasks": c}, items: map[string]*models.Item{}}
}

func TestMeAgendaTokenAllowlist(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	meAgendaFacts(t, w, "tasks", "Allowed")
	meAgendaFacts(t, other, "tasks", "Excluded")
	for _, tc := range []struct {
		name string
		mod  func(context.Context) context.Context
	}{
		{"allowlist", func(c context.Context) context.Context { return WithTokenAllowedWorkspaces(c, []string{w.ws.Slug}) }},
		{"pin", func(c context.Context) context.Context {
			ctx, err := w.srv.withTokenPin(WithTokenWorkspaceID(c, w.ws.ID), &models.APIToken{UserID: w.owner.ID, WorkspaceID: w.ws.ID})
			if err != nil {
				t.Fatal(err)
			}
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := meAgendaOK(t, w, w.owner, agendaWindow, tc.mod)
			if got := seen(m); got != "completed:Allowed,created:Allowed,date:Allowed,schedule:Allowed" {
				t.Fatalf("entries = %s", got)
			}
			for _, e := range entriesOf(m) {
				if e["workspace"] != w.ws.Slug || e["workspace_name"] != w.ws.Name {
					t.Fatalf("workspace = %v", e)
				}
			}
		})
	}
}

func TestMeAgendaGuestRestrictedCollection(t *testing.T) {
	w := newAgendaWorld(t, "tasks", "secret")
	meAgendaFacts(t, w, "secret", "Hidden")
	granted := meAgendaFacts(t, w, "secret", "Granted")
	guest := mustUser(t, w.srv, "guest-me-agenda@example.com", "guestmeagenda", "")
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, granted.ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	if got := seen(meAgendaOK(t, w, guest, agendaWindow)); got != "completed:Granted,created:Granted,date:Granted,schedule:Granted" {
		t.Fatalf("guest entries = %s", got)
	}
}

func TestMeAgendaHiddenItemNotCounted(t *testing.T) {
	w := newAgendaWorld(t, "tasks", "secret")
	u := mustUser(t, w.srv, "member-me-agenda@example.com", "membermeagenda", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, u.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := w.srv.store.SetMemberCollectionAccess(w.ws.ID, u.ID, "specific", []string{w.colls["tasks"].ID}); err != nil {
		t.Fatal(err)
	}
	before := meAgendaGet(t, w, u, agendaWindow)
	meAgendaFacts(t, w, "secret", "Hidden")
	after := meAgendaGet(t, w, u, agendaWindow)
	if before.Code != 200 || !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
		t.Fatalf("hidden changes response: %d %s -> %d %s", before.Code, before.Body.String(), after.Code, after.Body.String())
	}
}

func TestMeAgendaLostAccessDropsSilently(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	meAgendaFacts(t, w, "tasks", "Lost")
	u := mustUser(t, w.srv, "lost-me-agenda@example.com", "lostmeagenda", "")
	never := mustUser(t, w.srv, "never-me-agenda@example.com", "nevermeagenda", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, u.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if got := len(entriesOf(meAgendaOK(t, w, u, agendaWindow))); got != 4 {
		t.Fatalf("before = %d", got)
	}
	if err := w.srv.store.RemoveWorkspaceMember(w.ws.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	lost, absent := meAgendaGet(t, w, u, agendaWindow), meAgendaGet(t, w, never, agendaWindow)
	if lost.Code != 200 || !bytes.Equal(lost.Body.Bytes(), absent.Body.Bytes()) || strings.Contains(lost.Body.String(), w.ws.Slug) || strings.Contains(lost.Body.String(), w.ws.Name) {
		t.Fatalf("lost = %s; never = %s", lost.Body.String(), absent.Body.String())
	}
}

func TestMeAgendaMergesWorkspacesDeterministically(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Zebra")
	for _, world := range []*agendaWorld{other, w} {
		it := meAgendaFacts(t, world, "tasks", world.ws.Slug)
		meAgendaExec(t, w, `UPDATE items SET created_at = ? WHERE id = ?`, "2031-03-10T09:00:00Z", it.ID)
		meAgendaExec(t, w, `UPDATE status_transitions SET created_at = ? WHERE item_id = ?`, "2031-03-10T09:00:00Z", it.ID)
		meAgendaExec(t, w, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"open","due_date":"2031-03-10T09:00:00Z"}`, it.ID)
	}
	a, b := meAgendaGet(t, w, w.owner, agendaWindow), meAgendaGet(t, w, w.owner, agendaWindow)
	if a.Code != 200 || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatalf("unstable: %s / %s", a.Body.String(), b.Body.String())
	}
	es := entriesOf(meAgendaOK(t, w, w.owner, agendaWindow))
	var order []string
	for _, e := range es {
		order = append(order, fmt.Sprintf("%s:%s", e["type"], e["workspace"]))
	}
	if got, want := strings.Join(order, ","), "schedule:agenda,schedule:zebra,date:agenda,date:zebra,created:agenda,created:zebra,completed:agenda,completed:zebra"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestMeAgendaBoundsAcrossTenWorkspaces(t *testing.T) {
	for _, kind := range []string{"schedule", "date", "created", "completed"} {
		t.Run(kind, func(t *testing.T) {
			w := newAgendaWorld(t, "tasks")
			w.srv.agendaClock = func() time.Time { return meAgendaNow }
			start := time.Date(2031, 3, 10, 0, 0, 0, 0, time.UTC)
			for i := 0; i < 10; i++ {
				world := w
				if i > 0 {
					world = meAgendaWorkspace(t, w, fmt.Sprintf("Workspace %02d", i))
				}
				for j := 0; j < 60; j++ {
					at := start.Add(time.Duration(j*10+i) * time.Minute)
					var sc *models.ScheduleCreate
					fields := `{"status":"open"}`
					if kind == "schedule" {
						sc = &models.ScheduleCreate{Kind: "check", Dtstart: at.Format("2006-01-02T15:04"), Tz: "UTC"}
					}
					if kind == "date" {
						fields = fmt.Sprintf(`{"status":"open","due_date":%q}`, at.Format(time.RFC3339))
					}
					it := world.item(t, "tasks", fmt.Sprintf("Item %d", j), fields, sc)
					if kind == "created" {
						meAgendaExec(t, w, `UPDATE items SET created_at = ? WHERE id = ?`, at.Format(time.RFC3339), it.ID)
					}
					if kind == "completed" {
						meAgendaTransition(t, world, it, at.Format(time.RFC3339), "done")
					}
				}
			}
			m := meAgendaOK(t, w, w.owner, agendaWindow)
			if len(entriesOf(m)) != 500 || m["truncated"] != true {
				t.Fatalf("entries=%d truncated=%v", len(entriesOf(m)), m["truncated"])
			}
			if kind == "date" {
				if _, ok := m["next_from"]; ok {
					t.Fatalf("date cursor = %v", m["next_from"])
				}
				return
			}
			next := start.Add(500 * time.Minute).Format(time.RFC3339)
			if m["next_from"] != next {
				t.Fatalf("cursor = %v want %s", m["next_from"], next)
			}
			rest := entriesOf(meAgendaOK(t, w, w.owner, "from="+next+"&to=2031-03-11T00:00:00Z"))
			if len(rest) != 100 {
				t.Fatalf("remaining = %d", len(rest))
			}
			all := append(entriesOf(m), rest...)
			for i, e := range all {
				if e["at"] != start.Add(time.Duration(i)*time.Minute).Format(time.RFC3339) {
					t.Fatalf("gap/duplicate at %d: %v", i, e)
				}
			}
		})
	}
}

func TestMeAgendaCreatedCompleted(t *testing.T) {
	w := newAgendaWorld(t, "tasks", "secret")
	u := mustUser(t, w.srv, "complete-me-agenda@example.com", "completemeagenda", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, u.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := w.srv.store.SetMemberCollectionAccess(w.ws.ID, u.ID, "specific", []string{w.colls["tasks"].ID}); err != nil {
		t.Fatal(err)
	}
	it := w.item(t, "tasks", "Delivered", `{"status":"done"}`, nil)
	meAgendaExec(t, w, `UPDATE items SET created_at = ? WHERE id = ?`, "2031-03-10T01:00:00Z", it.ID)
	meAgendaTransition(t, w, it, "2031-03-10T02:00:00Z", "done")
	meAgendaTransition(t, w, it, "2031-03-10T03:00:00Z", "done")
	for _, title := range []string{"Abandoned", "Moved", "Deleted", "End boundary"} {
		x := w.item(t, "tasks", title, `{"status":"done"}`, nil)
		status := "done"
		if title == "Abandoned" {
			status = "cancelled"
		}
		at := "2031-03-10T02:00:00Z"
		if title == "End boundary" {
			at = "2031-03-11T00:00:00Z"
		}
		meAgendaTransition(t, w, x, at, status)
		if title == "Moved" {
			meAgendaExec(t, w, `UPDATE items SET collection_id = ? WHERE id = ?`, w.colls["secret"].ID, x.ID)
		}
		if title == "Deleted" {
			meAgendaExec(t, w, `UPDATE items SET deleted_at = ? WHERE id = ?`, at, x.ID)
		}
	}
	m := meAgendaOK(t, w, u, agendaWindow)
	if got := seen(m); got != "completed:Delivered,created:Delivered" {
		t.Fatalf("entries = %s", got)
	}
	es := entriesOf(m)
	if es[0]["at"] != "2031-03-10T01:00:00Z" || es[1]["at"] != "2031-03-10T03:00:00Z" {
		t.Fatalf("instants = %v", es)
	}
}

func TestMeAgendaTzDefaults(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	tz := "America/Los_Angeles"
	w.owner.Timezone = &tz
	w.item(t, "tasks", "All day", `{}`, &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10", Tz: "Pacific/Auckland"})
	m := meAgendaOK(t, w, w.owner, "from=2031-03-09T00:00:00Z&to=2031-03-11T00:00:00Z")
	if m["tz"] != *w.owner.Timezone || entriesOf(m)[0]["day"] != "2031-03-10" {
		t.Fatalf("timezone = %v", m)
	}
	w.owner.Timezone = nil
	m = meAgendaOK(t, w, w.owner, agendaWindow)
	if m["tz"] != "UTC" || m["timezone_note"] == nil {
		t.Fatalf("UTC default = %v", m)
	}
	rr := meAgendaGet(t, w, w.owner, agendaWindow+"&tz=bogus")
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), `"invalid_tz"`) {
		t.Fatalf("invalid tz = %d %s", rr.Code, rr.Body.String())
	}
}

func TestMeAgendaWindowValidation(t *testing.T) {
	w := newAgendaWorld(t)
	for _, tc := range []struct{ name, query, code string }{
		{"missing from", "to=2031-03-11T00:00:00Z", "required"},
		{"missing to", "from=2031-03-10T00:00:00Z", "required"},
		{"bad from", "from=bad&to=2031-03-11T00:00:00Z", "format"},
		{"bad to", "from=2031-03-10T00:00:00Z&to=bad", "format"},
		{"range", "from=2031-03-10T00:00:00Z&to=2031-03-10T00:00:00Z", "range"},
		{"window", "from=2031-03-01T00:00:00Z&to=2031-06-01T00:00:01Z", "cap_window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := meAgendaGet(t, w, w.owner, tc.query)
			if rr.Code != 400 || !strings.Contains(rr.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("%d %s", rr.Code, rr.Body.String())
			}
		})
	}
	meAgendaOK(t, w, w.owner, "from=2031-03-01T00:00:00Z&to=2031-06-01T00:00:00Z")
}

func TestMeAgendaNoAdmissionIsEmpty(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	meAgendaFacts(t, w, "tasks", "Excluded")
	never := mustUser(t, w.srv, "empty-me-agenda@example.com", "emptymeagenda", "")
	expected := meAgendaGet(t, w, never, agendaWindow)
	if expected.Code != 200 {
		t.Fatalf("empty = %d %s", expected.Code, expected.Body.String())
	}
	for _, tc := range []struct {
		name string
		mod  func(context.Context) context.Context
	}{
		{"empty allowlist", func(c context.Context) context.Context { return WithTokenAllowedWorkspaces(c, []string{}) }},
		{"unknown allowlist", func(c context.Context) context.Context { return WithTokenAllowedWorkspaces(c, []string{"missing"}) }},
		{"unknown pin", func(c context.Context) context.Context { return WithTokenWorkspaceID(c, "missing") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := meAgendaGet(t, w, w.owner, agendaWindow, tc.mod)
			if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), expected.Body.Bytes()) {
				t.Fatalf("excluded = %d %s; want %s", rr.Code, rr.Body.String(), expected.Body.String())
			}
		})
	}
}
