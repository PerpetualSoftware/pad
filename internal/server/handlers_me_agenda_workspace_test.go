package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/google/uuid"
)

const agendaWorkspaceUnavailable = "{\"error\":{\"code\":\"not_found\",\"message\":\"Workspace not available\"}}\n"

func TestMeAgendaWorkspaceCountScanIsolation(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Aardvark")
	if other.ws.Slug >= w.ws.Slug {
		t.Fatal("overflow fixture must sort first")
	}
	only := mustUser(t, w.srv, "only@example.com", "only", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, only.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	meAgendaFacts(t, w, "tasks", "Chosen")
	for i := 0; i < 5; i++ {
		meAgendaFacts(t, other, "tasks", fmt.Sprintf("Overflow %d", i))
	}
	w.srv.agendaCountsLimit, w.srv.agendaScanLimit = 3, 3
	query := agendaWindow + "&include=created,completed"
	want := meAgendaOK(t, w, only, query)
	got := meAgendaOK(t, w, w.owner, query+"&workspace="+w.ws.ID)
	if countsOf(got) == "" || countsOf(got) != countsOf(want) || completedOn(t, got, "2031-03-10") != 1 {
		t.Errorf("unrelated workspace consumed count budget: got %s, want %s", countsOf(got), countsOf(want))
	}
	if got["window_complete"] != true || got["truncated"] == true || !reflect.DeepEqual(got["entries"], want["entries"]) {
		t.Errorf("unrelated workspace consumed scan budget: got %s, want %s; complete=%v", seen(got), seen(want), got["window_complete"])
	}
	overflow := meAgendaOK(t, w, w.owner, query+"&workspace="+other.ws.ID)
	if overflow["counts_truncated"] != true || overflow["truncated"] != true || overflow["next_from"] != nil {
		t.Fatalf("overflow caps = %v", overflow)
	}
	// Without a cap, any leaked item must also change the actual count.
	w.srv.agendaCountsLimit, w.srv.agendaScanLimit = 0, 0
	got = meAgendaOK(t, w, w.owner, query+"&workspace="+w.ws.ID)
	if completedOn(t, got, "2031-03-10") != 1 {
		t.Errorf("unselected items leaked into counts: %s", countsOf(got))
	}
}

func TestMeAgendaWorkspaceUnavailable(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	deleted := meAgendaWorkspace(t, w, "Deleted")
	meAgendaExec(t, w, `UPDATE workspaces SET deleted_at = ? WHERE id = ?`, "2031-03-01T00:00:00Z", deleted.ws.ID)
	never := mustUser(t, w.srv, "never@example.com", "never", "")
	revoked := mustUser(t, w.srv, "revoked@example.com", "revoked", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, revoked.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := w.srv.store.RemoveWorkspaceMember(w.ws.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	guest := mustUser(t, w.srv, "stale@example.com", "stale", "")
	it := w.item(t, "tasks", "Deleted grant target", `{}`, nil)
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, it.ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	meAgendaExec(t, w, `UPDATE items SET deleted_at = ? WHERE id = ?`, "2031-03-01T00:00:00Z", it.ID)
	for _, tc := range []struct {
		name, id string
		user     *models.User
		mods     []func(context.Context) context.Context
	}{
		{"unknown", uuid.NewString(), w.owner, nil},
		{"deleted", deleted.ws.ID, w.owner, nil},
		{"never-member", w.ws.ID, never, nil},
		{"revoked", w.ws.ID, revoked, nil},
		{"consent-excluded", w.ws.ID, w.owner, []func(context.Context) context.Context{func(c context.Context) context.Context { return WithTokenAllowedWorkspaces(c, []string{"elsewhere"}) }}},
		{"stale-guest", w.ws.ID, guest, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := meAgendaGet(t, w, tc.user, agendaWindow+"&workspace="+tc.id, tc.mods...)
			if rr.Code != 404 || !bytes.Equal(rr.Body.Bytes(), []byte(agendaWorkspaceUnavailable)) {
				t.Errorf("got %d %q, want identical unavailable bytes", rr.Code, rr.Body.Bytes())
			}
		})
	}
	empty := meAgendaWorkspace(t, w, "Empty")
	if rr := meAgendaGet(t, w, w.owner, agendaWindow+"&workspace="+empty.ws.ID); rr.Code != 200 {
		t.Fatalf("readable empty: %d %s", rr.Code, rr.Body.String())
	}
}

func TestMeAgendaWorkspacePinned(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	meAgendaFacts(t, w, "tasks", "Pinned")
	meAgendaFacts(t, other, "tasks", "Foreign")
	for _, vanished := range []bool{false, true} {
		t.Run(fmt.Sprint("vanished=", vanished), func(t *testing.T) {
			pin := pinTo(t, w)
			if vanished {
				meAgendaExec(t, w, `UPDATE workspaces SET deleted_at = ? WHERE id = ?`, "2031-03-01T00:00:00Z", w.ws.ID)
			}
			expected := meAgendaGet(t, w, w.owner, agendaWindow+"&group=all", pin).Body.Bytes()
			queries := []string{"workspace=" + w.ws.ID, "workspace=" + other.ws.ID, "workspace=" + uuid.NewString(), "workspace=", "workspace=bad", "workspace=&group=", "workspace=" + w.ws.ID + "&group=all", "group="}
			var existing, missing []byte
			for i, query := range queries {
				for _, window := range []string{agendaWindow, "from=invalid&to=invalid"} {
					rr := meAgendaGet(t, w, w.owner, window+"&"+query, pin)
					if rr.Code != 403 || !bytes.Equal(rr.Body.Bytes(), expected) {
						t.Errorf("%s: got %d %q, want %q", query, rr.Code, rr.Body.Bytes(), expected)
					}
					if window == agendaWindow && i == 1 {
						existing = rr.Body.Bytes()
					}
					if window == agendaWindow && i == 2 {
						missing = rr.Body.Bytes()
					}
				}
			}
			if !bytes.Equal(existing, missing) {
				t.Error("existing and missing ids produced different pinned bodies")
			}
			if !vanished {
				m := meAgendaOK(t, w, w.owner, agendaWindow, pin)
				if completedOn(t, m, "2031-03-10") != 1 || strings.Contains(seen(m), "Foreign") {
					t.Fatalf("omitted selector widened: %v", m)
				}
			}
		})
	}
}

func TestMeAgendaWorkspaceGuestParity(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks", "secret")
	other := meAgendaWorkspace(t, w, "Other")
	guest := mustUser(t, w.srv, "guest@example.com", "guest", "")
	visible := meAgendaFacts(t, w, "secret", "Granted")
	meAgendaFacts(t, w, "secret", "Hidden")
	away := meAgendaFacts(t, other, "tasks", "Away")
	for _, pair := range []struct {
		world *agendaWorld
		item  *models.Item
	}{{w, visible}, {other, away}} {
		if _, err := w.srv.store.CreateItemGrant(pair.world.ws.ID, pair.item.ID, guest.ID, "view", w.owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	all := meAgendaOK(t, w, guest, agendaWindow)
	got := meAgendaOK(t, w, guest, agendaWindow+"&workspace="+w.ws.ID)
	var contributed []any
	for _, raw := range all["entries"].([]any) {
		if raw.(map[string]any)["workspace"] == w.ws.Slug {
			contributed = append(contributed, raw)
		}
	}
	if !reflect.DeepEqual(got["entries"], contributed) {
		t.Errorf("guest entries differ: %s", seen(got))
	}
	baseline := meAgendaOK(t, w, guest, agendaWindow, func(c context.Context) context.Context { return WithTokenAllowedWorkspaces(c, []string{w.ws.Slug}) })
	if countsOf(got) != countsOf(baseline) || completedOn(t, got, "2031-03-10") != 1 {
		t.Errorf("guest counts differ: %s / %s", countsOf(got), countsOf(baseline))
	}
}

func TestMeAgendaWorkspaceParsing(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	meAgendaFacts(t, w, "tasks", "Chosen")
	meAgendaFacts(t, other, "tasks", "Other")
	for _, tc := range []struct{ name, query, code, message string }{
		{"bare", "workspace", "format", "`workspace` must be a workspace UUID"},
		{"empty", "workspace=", "format", "`workspace` must be a workspace UUID"},
		{"whitespace", "workspace=++", "format", "`workspace` must be a workspace UUID"},
		{"slug", "workspace=agenda", "format", "`workspace` must be a workspace UUID"},
		{"comma", "workspace=" + w.ws.ID + "," + other.ws.ID, "format", "`workspace` must be a workspace UUID"},
		{"raw", "workspace=" + strings.ReplaceAll(w.ws.ID, "-", ""), "format", "`workspace` must be a workspace UUID"},
		{"urn", "workspace=urn:uuid:" + w.ws.ID, "format", "`workspace` must be a workspace UUID"},
		{"braces", "workspace=" + url.QueryEscape("{"+w.ws.ID+"}"), "format", "`workspace` must be a workspace UUID"},
		{"repeated", "workspace=" + w.ws.ID + "&workspace=" + other.ws.ID, "format", "`workspace` must be a single workspace UUID"},
		{"identical", "workspace=" + w.ws.ID + "&workspace=" + w.ws.ID, "format", "`workspace` must be a single workspace UUID"},
		{"both", "workspace=" + w.ws.ID + "&group=all", "conflict", "`workspace` cannot be combined with `group`"},
		{"both-empty", "workspace=&group=", "conflict", "`workspace` cannot be combined with `group`"},
		{"conflict-first", "workspace=bad&workspace=bad&group=", "conflict", "`workspace` cannot be combined with `group`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := meAgendaGet(t, w, w.owner, agendaWindow+"&"+tc.query)
			var got struct {
				Error struct {
					Code, Message string
					Details       map[string]any
				}
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if rr.Code != 400 || got.Error.Code != "validation_error" || got.Error.Message != tc.message || !reflect.DeepEqual(got.Error.Details, map[string]any{"field": "workspace", "code": tc.code}) {
				t.Errorf("got %d %s", rr.Code, rr.Body.String())
			}
		})
	}
	for _, id := range []string{strings.ToUpper(w.ws.ID), "  " + w.ws.ID + "  "} {
		got := meAgendaOK(t, w, w.owner, agendaWindow+"&workspace="+url.QueryEscape(id))
		if completedOn(t, got, "2031-03-10") != 1 {
			t.Errorf("canonical selection leaked: %s", countsOf(got))
		}
	}
	rr := meAgendaGet(t, w, w.owner, agendaWindow+"&workspace="+uuid.Nil.String())
	if rr.Code != 404 || !bytes.Equal(rr.Body.Bytes(), []byte(agendaWorkspaceUnavailable)) {
		t.Errorf("nil UUID: %d %q", rr.Code, rr.Body.Bytes())
	}
	app := func(c context.Context) context.Context { return context.WithValue(c, appCtxKey{}, &appContext{}) }
	if rr := meAgendaGet(t, w, w.owner, "workspace=bad", app); rr.Code != 404 {
		t.Errorf("app precedence: %d", rr.Code)
	}
	if rr := meAgendaGet(t, w, nil, "workspace=bad"); rr.Code != 401 {
		t.Errorf("auth precedence: %d", rr.Code)
	}
}

func TestMeAgendaWorkspaceContinuation(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Aardvark")
	for i := 0; i < 6; i++ {
		w.item(t, "tasks", fmt.Sprintf("Chosen %d", i), `{}`, hourly())
	}
	other.item(t, "tasks", "Unrelated", `{}`, hourly())
	group := groupCreate(t, w.srv, w.owner, "Only chosen", w.ws.ID)
	selected := "&workspace=" + w.ws.ID
	grouped := "&group=" + group.ID
	got := meAgendaOK(t, w, w.owner, hourlyWindow+selected)
	want := meAgendaOK(t, w, w.owner, hourlyWindow+grouped)
	if want["next_from"] == nil {
		t.Fatal("fixture did not overflow")
	}
	if got["next_from"] != want["next_from"] || !reflect.DeepEqual(got["entries"], want["entries"]) {
		t.Errorf("unrelated rows changed continuation: got %v, want %v", got["next_from"], want["next_from"])
	}
	query, _ := url.ParseQuery(hourlyWindow)
	query.Set("from", want["next_from"].(string))
	rest := meAgendaOK(t, w, w.owner, query.Encode()+selected)
	baseline := meAgendaOK(t, w, w.owner, query.Encode()+grouped)
	if !reflect.DeepEqual(rest["entries"], baseline["entries"]) {
		t.Error("continuation entries differ from one-member group")
	}
}

func TestMeAgendaWorkspaceSameNameScope(t *testing.T) {
	t.Parallel()
	w, _ := forecastWorld(t)
	otherOwner := mustUser(t, w.srv, "other-owner@example.com", "otherowner", "")
	other := meAgendaWorkspace(t, &agendaWorld{srv: w.srv, owner: otherOwner}, "Agenda")
	meAgendaExec(t, w, `UPDATE workspaces SET name = ? WHERE id = ?`, w.ws.Name, other.ws.ID)
	other.ws.Name = w.ws.Name
	if err := w.srv.store.AddWorkspaceMember(other.ws.ID, w.owner.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	for _, world := range []*agendaWorld{w, other} {
		it, _ := forecastOnlyItem(t, world, "Forecast "+world.owner.Username, "2031-04-02")
		meAgendaExec(t, w, `UPDATE items SET created_by_user_id = ? WHERE id = ?`, w.owner.ID, it.ID)
		broken := world.item(t, "tasks", "Broken "+world.owner.Username, `{}`, oneShot())
		meAgendaExec(t, w, `UPDATE item_schedules SET error_code = 'invalid_rule' WHERE item_id = ?`, broken.ID)
	}
	for _, world := range []*agendaWorld{w, other} {
		group := groupCreate(t, w.srv, w.owner, "Only "+world.owner.Username, world.ws.ID)
		for _, window := range []string{agendaWindow, forecastApril} {
			got := meAgendaOK(t, w, w.owner, window+"&workspace="+world.ws.ID)
			want := meAgendaOK(t, w, w.owner, window+"&group="+group.ID)
			for _, field := range []string{"entries", "counts", "forecasts", "forecasts_complete", "priority_list", "waiting_on", "no_date", "unavailable", "unexpanded", "window_complete"} {
				if !reflect.DeepEqual(got[field], want[field]) {
					t.Errorf("%s %s differs from single-workspace group", world.owner.Username, field)
				}
			}
		}
	}
}
