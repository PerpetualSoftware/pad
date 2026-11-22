package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4699 PR2: placement. A bare remind_at lands at the CALLER's day start
// in the caller's account zone; commitment day_start:true rewrites a bare
// start date to that wall time. Always the caller's setting, never the
// recipient's.

// task4699AgentRefusal is main's bare-date refusal, byte for byte. An
// agent-audience schedule still answers exactly this.
const task4699AgentRefusal = "{\"error\":{\"code\":\"invalid_remind_at\",\"message\":\"remind_at must be an RFC3339 instant (e.g. 2026-08-01T09:00:00Z). A bare date has no time of day, so it is refused rather than assumed to mean midnight.\"}}\n"

type task4699World struct {
	*agendaWorld
	token string
}

// newTask4699World signs in the agenda world's owner with the given account
// zone and day start; "" and -1 leave either unset.
func newTask4699World(t *testing.T, zone string, dayStart int) *task4699World {
	t.Helper()
	w := newAgendaWorld(t, "tasks")
	upd := models.UserUpdate{}
	if zone != "" {
		upd.Timezone = &zone
	}
	if dayStart >= 0 {
		upd.DayStartMinute = &dayStart
	}
	if upd.Timezone != nil || upd.DayStartMinute != nil {
		if _, err := w.srv.store.UpdateUser(w.owner.ID, upd); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := w.srv.store.CreateSession(w.owner.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	return &task4699World{agendaWorld: w, token: tok}
}

func (w *task4699World) do(t *testing.T, method, path string, body any) (int, map[string]any, string) {
	t.Helper()
	rr := doRequestWithCookie(w.srv, method, "/api/v1/workspaces/"+w.ws.Slug+path, body, w.token)
	m := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr.Code, m, rr.Body.String()
}

func (w *task4699World) arm(t *testing.T, title, at string) (int, map[string]any, string) {
	t.Helper()
	it := w.item(t, "tasks", title, `{"status":"open"}`, nil)
	return w.do(t, "POST", "/items/"+it.Slug+"/reminders", map[string]any{"remind_at": at})
}

func task4699Detail(m map[string]any) (code, field, detail string) {
	e, _ := m["error"].(map[string]any)
	code, _ = e["code"].(string)
	d, _ := e["details"].(map[string]any)
	field, _ = d["field"].(string)
	detail, _ = d["code"].(string)
	return
}

func TestTask4699_BareRemindAtLandsAtTheCallersDayStart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, date, want string
		dayStart         int
	}{
		{"set", "2026-11-09", "2026-11-09T12:30:00Z", 450},
		{"unset reads 09:00", "2026-11-09", "2026-11-09T14:00:00Z", -1},
		// 02:30 does not exist in New York that day: the gap shifts it forward
		// to 03:30 EDT, which is 07:30Z.
		{"dst gap", "2026-03-08", "2026-03-08T07:30:00Z", 150},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newTask4699World(t, "America/New_York", c.dayStart)
			code, m, raw := w.arm(t, "Bare "+c.name, c.date)
			if code != http.StatusCreated || m["remind_at"] != c.want || m["resolved_from"] != "day_start" {
				t.Fatalf("bare %s: %d %s, want 201 remind_at %s resolved_from day_start", c.date, code, raw, c.want)
			}
		})
	}
}

// An instant is not a bare date: it is stored as given and says nothing of
// where it came from.
func TestTask4699_InstantRemindAtIsUnmarked(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 450)
	code, m, raw := w.arm(t, "Instant", "2026-11-09T10:00:00Z")
	if code != http.StatusCreated || m["remind_at"] != "2026-11-09T10:00:00Z" {
		t.Fatalf("instant: %d %s", code, raw)
	}
	if _, ok := m["resolved_from"]; ok {
		t.Errorf("an instant carries resolved_from: %s", raw)
	}
}

func TestTask4699_BareRemindAtNeedsAnAccountZone(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "", 450)
	code, m, raw := w.arm(t, "No zone", "2026-11-09")
	if c, _, _ := task4699Detail(m); code != http.StatusBadRequest || c != "invalid_remind_at" || !strings.Contains(raw, "pad auth timezone") {
		t.Fatalf("no zone: %d %s, want 400 invalid_remind_at naming pad auth timezone", code, raw)
	}
}

func TestTask4699_RearmResolvesABareDate(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 450)
	code, m, raw := w.arm(t, "Rearm", "2031-01-01T00:00:00Z")
	if code != http.StatusCreated {
		t.Fatalf("arm: %d %s", code, raw)
	}
	code, m, raw = w.do(t, "PATCH", "/reminders/"+m["id"].(string), map[string]any{"remind_at": "2026-11-09"})
	if code != http.StatusOK || m["remind_at"] != "2026-11-09T12:30:00Z" || m["resolved_from"] != "day_start" {
		t.Fatalf("rearm bare: %d %s", code, raw)
	}
}

// Agent-audience schedules keep main's refusal, status, code and message, on
// both doors, even for a caller with a zone and a day start.
func TestTask4699_AgentAudienceKeepsTheBareDateRefusal(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 450)
	it := w.item(t, "tasks", "Agent", `{"status":"open"}`, nil)
	code, m, raw := w.do(t, "POST", "/items/"+it.Slug+"/reminders", map[string]any{"remind_at": "2031-01-01T00:00:00Z"})
	if code != http.StatusCreated {
		t.Fatalf("arm: %d %s", code, raw)
	}
	id := m["id"].(string)
	if code, _, raw := w.do(t, "PATCH", "/items/"+it.Slug+"/commitment", map[string]any{"audience": "agent"}); code != http.StatusOK {
		t.Fatalf("audience agent: %d %s", code, raw)
	}
	for name, req := range map[string][2]string{
		"rearm":  {"PATCH", "/reminders/" + id},
		"create": {"POST", "/items/" + it.Slug + "/reminders"},
	} {
		rr := doRequestWithCookie(w.srv, req[0], "/api/v1/workspaces/"+w.ws.Slug+req[1], map[string]any{"remind_at": "2026-11-09"}, w.token)
		if rr.Code != http.StatusBadRequest || rr.Body.String() != task4699AgentRefusal {
			t.Errorf("%s: %d %q, want 400 %q", name, rr.Code, rr.Body.String(), task4699AgentRefusal)
		}
	}
}

func task4699Commit(t *testing.T, w *task4699World, title string, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	it := w.item(t, "tasks", title, `{"status":"open"}`, nil)
	if _, ok := body["roles"]; !ok {
		body["roles"] = map[string]int{"target_days": 0, "target_secs": 0}
	}
	return w.do(t, "POST", "/items/"+it.Slug+"/commitment", body)
}

func TestTask4699_CommitmentDayStartRewritesABareStart(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 495)
	code, m, raw := task4699Commit(t, w, "Day start", map[string]any{"dtstart": "2026-11-09", "rrule": "FREQ=DAILY", "day_start": true})
	if code != http.StatusCreated || m["dtstart"] != "2026-11-09T08:15:00" || m["tz"] != "America/New_York" {
		t.Fatalf("day_start: %d %s, want dtstart 2026-11-09T08:15:00 in America/New_York", code, raw)
	}
	code, m, raw = task4699Commit(t, w, "All day", map[string]any{"dtstart": "2026-11-09", "rrule": "FREQ=DAILY"})
	if code != http.StatusCreated || m["dtstart"] != "2026-11-09" {
		t.Fatalf("without day_start: %d %s, want all-day 2026-11-09", code, raw)
	}
	// false is the same as absent.
	code, m, raw = task4699Commit(t, w, "False", map[string]any{"dtstart": "2026-11-09", "rrule": "FREQ=DAILY", "day_start": false})
	if code != http.StatusCreated || m["dtstart"] != "2026-11-09" {
		t.Fatalf("day_start false: %d %s, want all-day", code, raw)
	}
	// PATCH rewrites the start it is given, fixed at write.
	slug := w.items["All day"].Slug
	code, m, raw = w.do(t, "PATCH", "/items/"+slug+"/commitment", map[string]any{"dtstart": "2026-11-10", "day_start": true})
	if code != http.StatusOK || m["dtstart"] != "2026-11-10T08:15:00" {
		t.Fatalf("PATCH day_start: %d %s", code, raw)
	}
}

// Floating reads the wall in the recipient's zone, so it needs no caller zone;
// a pinned tz needs none either.
func TestTask4699_CommitmentDayStartFloatingAndPinned(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "", 495)
	code, m, raw := task4699Commit(t, w, "Floating", map[string]any{"dtstart": "2026-11-09", "rrule": "FREQ=DAILY", "floating": true, "day_start": true})
	if code != http.StatusCreated || m["dtstart"] != "2026-11-09T08:15:00" || m["floating"] != true {
		t.Fatalf("floating: %d %s", code, raw)
	}
	code, m, raw = task4699Commit(t, w, "Pinned", map[string]any{"dtstart": "2026-11-09", "rrule": "FREQ=DAILY", "tz": "Asia/Tokyo", "day_start": true})
	if code != http.StatusCreated || m["dtstart"] != "2026-11-09T08:15:00" || m["tz"] != "Asia/Tokyo" {
		t.Fatalf("pinned: %d %s", code, raw)
	}
}

func TestTask4699_CommitmentDayStartRefusals(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 495)
	noZone := newTask4699World(t, "", 495)
	cases := []struct {
		name string
		w    *task4699World
		body map[string]any
		want string
	}{
		{"timed start", w, map[string]any{"dtstart": "2026-11-09T10:00", "rrule": "FREQ=DAILY", "day_start": true}, "requires_date"},
		{"no start", w, map[string]any{"rrule": "FREQ=DAILY", "day_start": true}, "requires_date"},
		{"agent", w, map[string]any{"dtstart": "2026-11-09", "rrule": "FREQ=DAILY", "audience": "agent", "day_start": true}, "audience_agent"},
		{"no zone", noZone, map[string]any{"dtstart": "2026-11-09", "rrule": "FREQ=DAILY", "day_start": true}, "zone_unset"},
	}
	for _, c := range cases {
		code, m, raw := task4699Commit(t, c.w, "Refuse "+c.name, c.body)
		if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "day_start" || detail != c.want {
			t.Errorf("%s: %d %s, want 400 validation_error day_start/%s", c.name, code, raw, c.want)
		}
	}
	// PATCH: no start, a timed start, and an agent schedule (stored or patched).
	for _, title := range []string{"Patch target", "Patch agent"} {
		body := map[string]any{"dtstart": "2026-11-09", "rrule": "FREQ=DAILY"}
		if title == "Patch agent" {
			body["audience"] = "agent"
		}
		if code, _, raw := task4699Commit(t, w, title, body); code != http.StatusCreated {
			t.Fatalf("setup %s: %d %s", title, code, raw)
		}
	}
	slug := w.items["Patch target"].Slug
	agentSlug := w.items["Patch agent"].Slug
	for _, c := range []struct {
		name, slug string
		body       map[string]any
		want       string
	}{
		{"only day_start", slug, map[string]any{"day_start": true}, "requires_date"},
		{"timed", slug, map[string]any{"dtstart": "2026-11-10T07:00", "day_start": true}, "requires_date"},
		{"patched agent", slug, map[string]any{"dtstart": "2026-11-10", "audience": "agent", "day_start": true}, "audience_agent"},
		{"stored agent", agentSlug, map[string]any{"dtstart": "2026-11-10", "day_start": true}, "audience_agent"},
	} {
		code, m, got := w.do(t, "PATCH", "/items/"+c.slug+"/commitment", c.body)
		if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "day_start" || detail != c.want {
			t.Errorf("PATCH %s: %d %s, want 400 validation_error day_start/%s", c.name, code, got, c.want)
		}
	}
	// A stored agent schedule patched to human may take day_start.
	if code, m, got := w.do(t, "PATCH", "/items/"+agentSlug+"/commitment", map[string]any{"dtstart": "2026-11-10", "audience": "human", "day_start": true}); code != http.StatusOK || m["dtstart"] != "2026-11-10T08:15:00" {
		t.Errorf("agent patched to human: %d %s", code, got)
	}
}
