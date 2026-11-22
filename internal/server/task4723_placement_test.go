package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TASK-4723: a bare until on snooze and move, and a bare to on the linked
// move (role target or earliest), land at the CALLER's day start in the
// caller's account zone, as remind_at does since TASK-4699.

// task4723UntilRefusal is main's answer to a bare until, byte for byte. A
// bare date on done or skip, or on an agent-audience schedule, still answers
// exactly this.
const task4723UntilRefusal = "{\"error\":{\"code\":\"validation_error\",\"details\":{\"code\":\"format\",\"field\":\"until\"},\"message\":\"until must be an RFC3339 instant\"}}\n"

// task4723ToRefusal is main's answer to a bare to on the linked move, byte for
// byte. Role deadline and occupied_until, an agent-audience schedule and a
// caller who may not edit still answer exactly this.
const task4723ToRefusal = "{\"error\":{\"code\":\"validation_error\",\"details\":{\"code\":\"format\",\"field\":\"to\"},\"message\":\"schedule to: Invalid commitment to\"}}\n"

var task4723NY = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}()

// task4723Day is the New York calendar day n days from now, and the instant
// that day starts at for a caller whose day starts at minute.
func task4723Day(n, minute int) (string, string) {
	d := time.Now().In(task4723NY).AddDate(0, 0, n)
	at := time.Date(d.Year(), d.Month(), d.Day(), minute/60, minute%60, 0, 0, task4723NY)
	return d.Format("2006-01-02"), at.UTC().Format(time.RFC3339)
}

// task4723Commit creates a one-shot commitment tomorrow at 10:00 New York and
// returns the item slug and its outstanding occurrence. deadlineDays < 0
// leaves the deadline unset.
func task4723Commit(t *testing.T, w *task4699World, title string, deadlineDays int) (string, string) {
	t.Helper()
	start := time.Now().In(task4723NY).AddDate(0, 0, 1)
	roles := map[string]any{"target_days": 0, "target_secs": 0}
	if deadlineDays >= 0 {
		roles["deadline_days"], roles["deadline_secs"], roles["deadline_behavior"], roles["outcome"] = deadlineDays, 0, "stays_owed", "shipped"
	}
	code, m, raw := task4699Commit(t, w, title, map[string]any{"dtstart": start.Format("2006-01-02") + "T10:00", "tz": "America/New_York", "roles": roles})
	if code != http.StatusCreated {
		t.Fatalf("commit %s: %d %s", title, code, raw)
	}
	next, _ := m["next_occurrence"].(map[string]any)
	at, _ := next["at"].(string)
	if at == "" {
		t.Fatalf("commit %s: no next occurrence: %s", title, raw)
	}
	return w.items[title].Slug, at
}

func (w *task4699World) resolve(t *testing.T, slug string, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	return w.do(t, "POST", "/items/"+slug+"/commitment/resolve", body)
}

func TestTask4723_BareSnoozeUntilLandsAtTheCallersDayStart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		dayStart int
		minute   int
	}{
		{"unset reads 09:00", -1, 540},
		{"set", 450, 450},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newTask4699World(t, "America/New_York", c.dayStart)
			slug, occ := task4723Commit(t, w, "Snooze", -1)
			day, want := task4723Day(3, c.minute)
			code, m, raw := w.resolve(t, slug, map[string]any{"occurrence_at": occ, "state": "snooze", "until": day})
			res, _ := m["resolution"].(map[string]any)
			view, _ := m["commitment"].(map[string]any)
			if code != http.StatusOK || res["until"] != want || res["resolved_from"] != "day_start" {
				t.Fatalf("snooze %s: %d %s, want resolution.until %s resolved_from day_start", day, code, raw, want)
			}
			if view["snoozed_until"] != want {
				t.Errorf("snoozed_until = %v, want %s: %s", view["snoozed_until"], want, raw)
			}
		})
	}
}

// The hard-deadline check reads the placed instant: a bare date past the
// deadline is refused, then applied with confirm_past_deadline.
func TestTask4723_BareMoveUntilMeetsTheDeadlineCheck(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 450)
	slug, occ := task4723Commit(t, w, "Move", 1)
	day, want := task4723Day(5, 450)
	body := map[string]any{"occurrence_at": occ, "state": "move", "until": day}
	if code, m, raw := w.resolve(t, slug, body); code != http.StatusConflict {
		t.Fatalf("past the deadline: %d %s, want 409", code, raw)
	} else if e, _ := m["error"].(map[string]any); e["code"] != "target_after_deadline" {
		t.Fatalf("past the deadline: %s", raw)
	}
	body["confirm_past_deadline"] = true
	code, m, raw := w.resolve(t, slug, body)
	res, _ := m["resolution"].(map[string]any)
	if code != http.StatusOK || res["until"] != want || res["resolved_from"] != "day_start" || res["warning"] != "target_after_deadline" {
		t.Fatalf("confirmed: %d %s, want until %s resolved_from day_start", code, raw, want)
	}
}

func TestTask4723_BareUntilRefusals(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 450)
	slug, occ := task4723Commit(t, w, "Refusals", -1)
	day, _ := task4723Day(3, 450)

	// done and skip: main's refusal, byte for byte.
	for _, state := range []string{"done", "skip"} {
		code, _, raw := w.resolve(t, slug, map[string]any{"occurrence_at": occ, "state": state, "until": day})
		if code != http.StatusBadRequest || raw != task4723UntilRefusal {
			t.Errorf("%s: %d %q, want %q", state, code, raw, task4723UntilRefusal)
		}
	}
	// Malformed on snooze or move names both forms.
	for _, v := range []string{"next tuesday", "2026-13-40", "2026-11-10T09:00"} {
		code, m, raw := w.resolve(t, slug, map[string]any{"occurrence_at": occ, "state": "snooze", "until": v})
		if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "until" || detail != "format" || !strings.Contains(raw, "or a bare date (YYYY-MM-DD)") {
			t.Errorf("malformed %q: %d %s, want until/format naming both forms", v, code, raw)
		}
	}
	// No account zone.
	nz := newTask4699World(t, "", 450)
	nzSlug, nzOcc := task4723Commit(t, nz, "No zone", -1)
	code, m, raw := nz.resolve(t, nzSlug, map[string]any{"occurrence_at": nzOcc, "state": "snooze", "until": day})
	if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "until" || detail != "zone_unset" || !strings.Contains(raw, "pad auth timezone") {
		t.Errorf("no zone: %d %s, want until/zone_unset naming pad auth timezone", code, raw)
	}
	// Agent audience: main's refusal, byte for byte.
	if code, _, raw := w.do(t, "PATCH", "/items/"+slug+"/commitment", map[string]any{"audience": "agent"}); code != http.StatusOK {
		t.Fatalf("audience agent: %d %s", code, raw)
	}
	for _, state := range []string{"snooze", "move"} {
		code, _, raw := w.resolve(t, slug, map[string]any{"occurrence_at": occ, "state": state, "until": day})
		if code != http.StatusBadRequest || raw != task4723UntilRefusal {
			t.Errorf("agent %s: %d %q, want %q", state, code, raw, task4723UntilRefusal)
		}
	}
}

// An RFC3339 until answers exactly as before: no until or resolved_from.
func TestTask4723_InstantUntilIsUnmarked(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 450)
	slug, occ := task4723Commit(t, w, "Instant", -1)
	until := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	code, m, raw := w.resolve(t, slug, map[string]any{"occurrence_at": occ, "state": "snooze", "until": until})
	res, _ := m["resolution"].(map[string]any)
	if code != http.StatusOK {
		t.Fatalf("instant: %d %s", code, raw)
	}
	for _, k := range []string{"until", "resolved_from"} {
		if _, ok := res[k]; ok {
			t.Errorf("an instant until carries resolution.%s: %s", k, raw)
		}
	}
}

func TestTask4723_LinkedMoveBareToPlacesTargetAndEarliest(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 450)
	slug, _ := task4723Commit(t, w, "Linked", -1)
	day, want := task4723Day(4, 450)
	code, m, raw := w.do(t, "POST", "/items/"+slug+"/commitment/move-preview", map[string]any{"role": "target", "to": day})
	if code != http.StatusOK || m["resolved_from"] != "day_start" {
		t.Fatalf("preview: %d %s, want 200 resolved_from day_start", code, raw)
	}
	changes, _ := m["changes"].([]any)
	root := map[string]any{}
	for _, c := range changes {
		if cm, _ := c.(map[string]any); cm["root"] == true {
			root = cm
		}
	}
	if root["to"] != want {
		t.Fatalf("preview root to = %v, want %s: %s", root["to"], want, raw)
	}
	code, m, raw = w.do(t, "POST", "/items/"+slug+"/commitment/move", map[string]any{"role": "target", "to": day, "revision": m["revision"]})
	if code != http.StatusOK || m["resolved_from"] != "day_start" {
		t.Fatalf("apply: %d %s, want 200 resolved_from day_start", code, raw)
	}
	// earliest is placed too.
	start := time.Now().In(task4723NY).AddDate(0, 0, 1).Format("2006-01-02") + "T10:00"
	if code, _, raw := task4699Commit(t, w, "Earliest", map[string]any{"dtstart": start, "tz": "America/New_York", "roles": map[string]any{"target_days": 0, "target_secs": 0, "earliest_days": 0, "earliest_secs": 0}}); code != http.StatusCreated {
		t.Fatalf("commit earliest: %d %s", code, raw)
	}
	early, _ := task4723Day(2, 450)
	code, m, raw = w.do(t, "POST", "/items/"+w.items["Earliest"].Slug+"/commitment/move-preview", map[string]any{"role": "earliest", "to": early})
	if code != http.StatusOK || m["resolved_from"] != "day_start" {
		t.Fatalf("earliest preview: %d %s", code, raw)
	}
	// An RFC3339 to is unmarked.
	code, m, raw = w.do(t, "POST", "/items/"+slug+"/commitment/move-preview", map[string]any{"role": "target", "to": want})
	if _, ok := m["resolved_from"]; code != http.StatusOK || ok {
		t.Fatalf("instant preview: %d %s, want no resolved_from", code, raw)
	}
}

func TestTask4723_LinkedMoveBareToRefusals(t *testing.T) {
	t.Parallel()
	w := newTask4699World(t, "America/New_York", 450)
	slug, _ := task4723Commit(t, w, "Linked refusals", 2)
	day, _ := task4723Day(4, 450)
	move := func(w *task4699World, slug string, body map[string]any) (int, map[string]any, string) {
		return w.do(t, "POST", "/items/"+slug+"/commitment/move-preview", body)
	}
	// deadline and occupied_until keep main's refusal.
	for _, role := range []string{"deadline", "occupied_until"} {
		if code, _, raw := move(w, slug, map[string]any{"role": role, "to": day}); code != http.StatusBadRequest || raw != task4723ToRefusal {
			t.Errorf("%s: %d %q, want %q", role, code, raw, task4723ToRefusal)
		}
	}
	// Malformed names both forms.
	code, m, raw := move(w, slug, map[string]any{"role": "target", "to": "next tuesday"})
	if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "to" || detail != "format" || !strings.Contains(raw, "a bare date (YYYY-MM-DD)") || raw == task4723ToRefusal {
		t.Errorf("malformed: %d %s", code, raw)
	}
	// Request-only checks answer for themselves, not as a to format refusal.
	code, m, raw = move(w, slug, map[string]any{"role": "earliest", "to": day, "scope": "series"})
	if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "scope" || detail != "combination" {
		t.Errorf("bare earliest series: %d %s, want scope/combination", code, raw)
	}
	code, m, raw = move(w, slug, map[string]any{"role": "target", "to": day, "scope": "everything"})
	if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "scope" || detail != "enum" {
		t.Errorf("bare bad scope: %d %s, want scope/enum", code, raw)
	}
	code, m, raw = w.do(t, "POST", "/items/"+slug+"/commitment/move", map[string]any{"role": "target", "to": day, "revision": "not-a-token"})
	if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "revision" || detail != "format" {
		t.Errorf("bare bad revision: %d %s, want revision/format", code, raw)
	}
	// No account zone.
	nz := newTask4699World(t, "", 450)
	nzSlug, _ := task4723Commit(t, nz, "No zone", -1)
	code, m, raw = move(nz, nzSlug, map[string]any{"role": "target", "to": day})
	if ec, field, detail := task4699Detail(m); code != http.StatusBadRequest || ec != "validation_error" || field != "to" || detail != "zone_unset" || !strings.Contains(raw, "pad auth timezone") {
		t.Errorf("no zone: %d %s, want to/zone_unset naming pad auth timezone", code, raw)
	}
	// A caller who may not edit gets main's answer to a bare date.
	viewer := mustUser(t, w.srv, "task4723-viewer@example.com", "task4723viewer", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	vtok, err := w.srv.store.CreateSession(viewer.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(w.srv, "POST", "/api/v1/workspaces/"+w.ws.Slug+"/items/"+slug+"/commitment/move-preview", map[string]any{"role": "target", "to": day}, vtok)
	if rr.Code != http.StatusBadRequest || rr.Body.String() != task4723ToRefusal {
		t.Errorf("viewer: %d %q, want %q", rr.Code, rr.Body.String(), task4723ToRefusal)
	}
	// Agent audience: main's refusal.
	if code, _, raw := w.do(t, "PATCH", "/items/"+slug+"/commitment", map[string]any{"audience": "agent"}); code != http.StatusOK {
		t.Fatalf("audience agent: %d %s", code, raw)
	}
	for _, door := range []string{"move-preview", "move"} {
		code, _, raw := w.do(t, "POST", "/items/"+slug+"/commitment/"+door, map[string]any{"role": "target", "to": day})
		if code != http.StatusBadRequest || raw != task4723ToRefusal {
			t.Errorf("agent %s: %d %q, want %q", door, code, raw, task4723ToRefusal)
		}
	}
}
