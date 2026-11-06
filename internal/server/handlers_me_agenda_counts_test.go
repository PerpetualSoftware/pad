package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// countsOf returns the counts series of an agenda response as compact JSON, or
// "" when there is none.
func countsOf(m map[string]any) string {
	c, ok := m["counts"]
	if !ok {
		return ""
	}
	b, _ := json.Marshal(c)
	return string(b)
}

// completedOn returns the completed count of one day.
func completedOn(t *testing.T, m map[string]any, day string) int {
	t.Helper()
	list, _ := m["counts"].([]any)
	for _, raw := range list {
		d := raw.(map[string]any)
		if d["day"] == day {
			return int(d["completed"].(float64))
		}
	}
	t.Fatalf("no counts for %s in %s", day, countsOf(m))
	return 0
}

func pinTo(t *testing.T, w *agendaWorld) func(context.Context) context.Context {
	return func(c context.Context) context.Context {
		ctx, err := w.srv.withTokenPin(WithTokenWorkspaceID(c, w.ws.ID), &models.APIToken{UserID: w.owner.ID, WorkspaceID: w.ws.ID})
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
}

func TestMeAgendaCountsSeries(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	meAgendaFacts(t, w, "tasks", "One")
	meAgendaFacts(t, w, "tasks", "Two")
	m := meAgendaOK(t, w, w.owner, agendaWindow)
	if got, want := countsOf(m), `[{"burndown":0,"completed":2,"day":"2031-03-10","open":0}]`; got != want {
		t.Fatalf("counts = %s, want %s", got, want)
	}
}

// A hidden item in a readable workspace moves no count, and a guest sees only
// what an item grant opens in a restricted collection.
func TestMeAgendaCountsHiddenAndGuest(t *testing.T) {
	w := newAgendaWorld(t, "tasks", "secret")
	member := mustUser(t, w.srv, "counts-member@example.com", "countsmember", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, member.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := w.srv.store.SetMemberCollectionAccess(w.ws.ID, member.ID, "specific", []string{w.colls["tasks"].ID}); err != nil {
		t.Fatal(err)
	}
	meAgendaFacts(t, w, "tasks", "Readable")
	before := countsOf(meAgendaOK(t, w, member, agendaWindow))
	meAgendaFacts(t, w, "secret", "Hidden")
	if after := countsOf(meAgendaOK(t, w, member, agendaWindow)); after != before || !strings.Contains(after, `"completed":1`) {
		t.Fatalf("hidden item moved the counts: %s -> %s", before, after)
	}

	granted := meAgendaFacts(t, w, "secret", "Granted")
	guest := mustUser(t, w.srv, "counts-guest@example.com", "countsguest", "")
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, granted.ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	if got := countsOf(meAgendaOK(t, w, guest, agendaWindow)); got != `[{"burndown":0,"completed":1,"day":"2031-03-10","open":0}]` {
		t.Fatalf("guest counts = %s", got)
	}
}

func TestMeAgendaCountsRevokedWorkspaceDrops(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	u := mustUser(t, w.srv, "counts-revoked@example.com", "countsrevoked", "")
	for _, ws := range []*models.Workspace{w.ws, other.ws} {
		if err := w.srv.store.AddWorkspaceMember(ws.ID, u.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
	}
	meAgendaFacts(t, w, "tasks", "Kept")
	meAgendaFacts(t, other, "tasks", "Revoked")
	if got := completedOn(t, meAgendaOK(t, w, u, agendaWindow), "2031-03-10"); got != 2 {
		t.Fatalf("before = %d", got)
	}
	if err := w.srv.store.RemoveWorkspaceMember(other.ws.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if got := completedOn(t, meAgendaOK(t, w, u, agendaWindow), "2031-03-10"); got != 1 {
		t.Fatalf("after revoke = %d", got)
	}
}

// 23:30 local on the 10th lands on the 10th, not the 11th UTC.
func TestMeAgendaCountsLocalDay(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	it := meAgendaFacts(t, w, "tasks", "Late")
	meAgendaExec(t, w, `UPDATE items SET created_at = ? WHERE id = ?`, "2031-03-01T00:00:00Z", it.ID)
	meAgendaExec(t, w, `UPDATE status_transitions SET created_at = ? WHERE item_id = ? AND to_status = 'done'`, "2031-03-11T06:30:00Z", it.ID)
	m := meAgendaOK(t, w, w.owner, "from=2031-03-10T07:00:00Z&to=2031-03-12T07:00:00Z&tz=America/Los_Angeles")
	if got, want := countsOf(m), `[{"burndown":0,"completed":1,"day":"2031-03-10","open":0},{"burndown":0,"completed":0,"day":"2031-03-11","open":0}]`; got != want {
		t.Fatalf("counts = %s, want %s", got, want)
	}
	utc := meAgendaOK(t, w, w.owner, "from=2031-03-10T07:00:00Z&to=2031-03-12T07:00:00Z&tz=UTC")
	if completedOn(t, utc, "2031-03-11") != 1 {
		t.Fatalf("utc counts = %s", countsOf(utc))
	}
}

func TestMeAgendaGroupParameter(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	stranger := mustUser(t, w.srv, "counts-stranger@example.com", "countsstranger", "")
	if err := w.srv.store.AddWorkspaceMember(other.ws.ID, stranger.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	meAgendaFacts(t, w, "tasks", "Home")
	meAgendaFacts(t, other, "tasks", "Away")
	// The stranger owns nothing here, so mine is empty for them.
	home := groupCreate(t, w.srv, w.owner, "Home only", w.ws.ID)
	theirs := groupCreate(t, w.srv, stranger, "Theirs", other.ws.ID)

	all := meAgendaOK(t, w, w.owner, agendaWindow)
	if completedOn(t, all, "2031-03-10") != 2 || countsOf(all) != countsOf(meAgendaOK(t, w, w.owner, agendaWindow+"&group=all")) {
		t.Fatalf("default is not all: %s", countsOf(all))
	}
	if got := completedOn(t, meAgendaOK(t, w, w.owner, agendaWindow+"&group=mine"), "2031-03-10"); got != 2 {
		t.Fatalf("mine = %d", got)
	}
	named := meAgendaOK(t, w, w.owner, agendaWindow+"&group="+home.ID)
	if completedOn(t, named, "2031-03-10") != 1 || seen(named) != "completed:Home,created:Home,date:Home,schedule:Home" {
		t.Fatalf("named group: %s / %s", seen(named), countsOf(named))
	}
	if got := completedOn(t, meAgendaOK(t, w, stranger, agendaWindow+"&group=mine"), "2031-03-10"); got != 0 {
		t.Fatalf("stranger mine = %d", got)
	}
	// Another user's group, and an id that does not exist, answer alike.
	a := meAgendaGet(t, w, w.owner, agendaWindow+"&group="+theirs.ID)
	b := meAgendaGet(t, w, w.owner, agendaWindow+"&group=00000000-0000-4000-8000-000000000000")
	if a.Code != http.StatusNotFound || b.Code != http.StatusNotFound || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatalf("foreign group: %d %s / unknown: %d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
	if rr := meAgendaGet(t, w, w.owner, agendaWindow+"&group="); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty group: %d %s", rr.Code, rr.Body.String())
	}
}

// A pinned token never names a group: the refusal is the same whether the id is
// its own, unknown or built in. With no group it stays inside its workspace in
// the entries and the counts.
func TestMeAgendaGroupPinnedToken(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	meAgendaFacts(t, w, "tasks", "Pinned")
	meAgendaFacts(t, other, "tasks", "Elsewhere")
	own := groupCreate(t, w.srv, w.owner, "Own", w.ws.ID)
	pin := pinTo(t, w)
	var first string
	for _, group := range []string{own.ID, "00000000-0000-4000-8000-000000000000", "all", "mine"} {
		rr := meAgendaGet(t, w, w.owner, agendaWindow+"&group="+group, pin)
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "token_workspace_pinned") {
			t.Fatalf("group=%s: %d %s", group, rr.Code, rr.Body.String())
		}
		// The id is never echoed, and the body is the same for every value.
		if strings.Contains(rr.Body.String(), group) && group != "all" && group != "mine" {
			t.Fatalf("group=%s echoed: %s", group, rr.Body.String())
		}
		if first == "" {
			first = rr.Body.String()
		} else if rr.Body.String() != first {
			t.Fatalf("group=%s body differs:\n%s\n%s", group, rr.Body.String(), first)
		}
	}
	m := meAgendaOK(t, w, w.owner, agendaWindow, pin)
	if seen(m) != "completed:Pinned,created:Pinned,date:Pinned,schedule:Pinned" || completedOn(t, m, "2031-03-10") != 1 {
		t.Fatalf("pinned default widened: %s / %s", seen(m), countsOf(m))
	}
}

func TestMeAgendaCountsCap(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	meAgendaFacts(t, w, "tasks", "One")
	meAgendaFacts(t, w, "tasks", "Two")
	w.srv.agendaCountsLimit = 1
	m := meAgendaOK(t, w, w.owner, agendaWindow)
	if m["counts_truncated"] != true || m["truncated"] != true {
		t.Fatalf("cap not reported: %v", m)
	}
	if _, has := m["counts"]; has {
		t.Fatalf("a partial series was sent: %s", countsOf(m))
	}
	if _, has := m["next_from"]; has {
		t.Fatalf("a count cut promised a resume point: %v", m["next_from"])
	}
}
