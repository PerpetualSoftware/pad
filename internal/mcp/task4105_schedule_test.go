package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-4105: schedules and the agenda on pad_item. Every action rides the
// `pad item remind` CLI on stdio and a route on /mcp.

// task4105FixtureFor adjusts the shared parity fixture for the schedule
// actions and routes that need an input of their own: name is a catalog label
// ("pad_item.agenda") or a routed command ("item remind edit"). The shared
// `to` is a number for chat read, and a schedule-edit with no member to change
// is refused before dispatch.
func task4105FixtureFor(name string, in map[string]any) {
	switch name {
	case "pad_item.agenda", "item remind agenda":
		in["from"], in["to"] = "2026-11-09T00:00:00Z", "2026-11-16T00:00:00Z"
	case "pad_item.schedule-edit", "item remind edit":
		in["deadline"] = "hard"
	}
}

func scheduleItemAction(t *testing.T, name string) ActionFn {
	t.Helper()
	for _, d := range Catalog {
		if d.Name == "pad_item" {
			fn, ok := d.Actions[name]
			if !ok {
				t.Fatalf("pad_item has no action %q", name)
			}
			return fn
		}
	}
	t.Fatal("pad_item is not in the catalog")
	return nil
}

// stdioCall runs a pad_item action against a recording dispatcher, as the
// stdio server does, and returns what reached it.
func stdioCall(t *testing.T, action string, in map[string]any) (*CallToolResult, *fakeDispatcher) {
	t.Helper()
	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	res, err := scheduleItemAction(t, action)(context.Background(), in, env)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return res, disp
}

type recordedRequest struct {
	Method, Path, Query, Body string
}

// remoteCall runs a pad_item action through the /mcp dispatcher against a
// handler that records the request and answers 200 {}.
func remoteCall(t *testing.T, action string, in map[string]any) (*CallToolResult, []recordedRequest) {
	t.Helper()
	var seen []recordedRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b []byte
		if r.Body != nil {
			b, _ = io.ReadAll(r.Body)
		}
		seen = append(seen, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	disp := &HTTPHandlerDispatcher{Handler: h, UserResolver: func(context.Context) *models.User { return &models.User{ID: "u1"} }}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	res, err := scheduleItemAction(t, action)(context.Background(), in, env)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return res, seen
}

func copyInput(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func TestTASK4105_ScheduleReadOnlyClassification(t *testing.T) {
	for action, want := range map[string]bool{
		"schedule": true, "agenda": true,
		"schedule-edit": false, "schedule-delete": false, "schedule-resolve": false, "remind": false,
	} {
		if got := isReadOnlyAction("pad_item", action); got != want {
			t.Errorf("pad_item.%s read-only = %v, want %v", action, got, want)
		}
	}
	for _, d := range Catalog {
		if d.Name != "pad_item" {
			continue
		}
		a := annotationForDef(d)
		if *a.ReadOnlyHint || !*a.DestructiveHint {
			t.Errorf("pad_item annotation moved: readOnly=%v destructive=%v, want false/true", *a.ReadOnlyHint, *a.DestructiveHint)
		}
	}
}

// The one-shot form is unchanged on both transports: no schedule parameter,
// or an empty/false one, reaches the same command and the same route as before.
func TestTASK4105_RemindWithoutScheduleParamsIsTheOneShot(t *testing.T) {
	base := map[string]any{"ref": "TASK-1", "remind_at": "2026-08-01T09:00:00Z"}
	for name, extra := range map[string]map[string]any{
		"plain":          {},
		"empty strings":  {"every": "", "start": "", "tz": "", "deadline": "", "window": "", "anchor": "", "audience": ""},
		"floating false": {"floating": false},
	} {
		t.Run("stdio/"+name, func(t *testing.T) {
			in := copyInput(base)
			for k, v := range extra {
				in[k] = v
			}
			_, disp := stdioCall(t, "remind", in)
			if got, want := strings.Join(disp.gotPath, " "), "item remind"; got != want {
				t.Fatalf("cmdPath = %q, want %q", got, want)
			}
			want := []string{"--remind-at", "2026-08-01T09:00:00Z", "--workspace", "docapp", "--format", "json", "--", "TASK-1"}
			if !reflect.DeepEqual(disp.gotArgs, want) {
				t.Fatalf("args = %q, want %q", disp.gotArgs, want)
			}
		})
	}
	for name, extra := range map[string]map[string]any{
		"plain":          {},
		"empty strings":  {"every": "", "start": "", "tz": "", "deadline": ""},
		"floating false": {"floating": false},
	} {
		t.Run("remote/"+name, func(t *testing.T) {
			in := copyInput(base)
			for k, v := range extra {
				in[k] = v
			}
			res, seen := remoteCall(t, "remind", withWorkspace(in))
			if res.IsError {
				t.Fatalf("refused: %s", textOf(res))
			}
			want := []recordedRequest{{"POST", "/api/v1/workspaces/docapp/items/TASK-1/reminders", "", `{"remind_at":"2026-08-01T09:00:00Z"}`}}
			if !reflect.DeepEqual(seen, want) {
				t.Fatalf("requests = %+v, want %+v", seen, want)
			}
		})
	}
}

func withWorkspace(in map[string]any) map[string]any {
	out := copyInput(in)
	out["workspace"] = "docapp"
	return out
}

func TestTASK4105_RemindWithScheduleParamsCreatesASchedule(t *testing.T) {
	in := map[string]any{"ref": "TASK-1", "start": "2026-11-09T09:00", "every": "weekdays", "deadline": "soft", "anchor": "completion", "audience": "agent", "tz": "America/New_York"}
	t.Run("stdio", func(t *testing.T) {
		_, disp := stdioCall(t, "remind", in)
		if got := strings.Join(disp.gotPath, " "); got != "item remind" {
			t.Fatalf("cmdPath = %q", got)
		}
		for _, pair := range [][2]string{{"--start", "2026-11-09T09:00"}, {"--every", "weekdays"}, {"--deadline", "soft"}, {"--anchor", "completion"}, {"--audience", "agent"}, {"--tz", "America/New_York"}} {
			if !hasFlagPair(disp.gotArgs, pair[0], pair[1]) {
				t.Errorf("args %q lack %s %s", disp.gotArgs, pair[0], pair[1])
			}
		}
	})
	t.Run("remote", func(t *testing.T) {
		res, seen := remoteCall(t, "remind", withWorkspace(in))
		if res.IsError {
			t.Fatalf("refused: %s", textOf(res))
		}
		if len(seen) != 1 || seen[0].Method != "POST" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-1/schedule" {
			t.Fatalf("requests = %+v", seen)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(seen[0].Body), &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"kind": "deadline_soft", "dtstart": "2026-11-09T09:00:00", "rrule": "weekdays", "anchor": "completion", "audience": "agent", "tz": "America/New_York"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("body = %v, want %v", got, want)
		}
	})
}

func hasFlagPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func TestTASK4105_RemoteCreateBodies(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{"check from a date", map[string]any{"start": "2026-11-09"}, map[string]any{"kind": "check", "dtstart": "2026-11-09"}},
		{"floating", map[string]any{"start": "2026-11-09T09:00", "floating": true}, map[string]any{"kind": "check", "dtstart": "2026-11-09T09:00:00", "floating": true}},
		{"hard deadline", map[string]any{"start": "2026-11-14T17:00", "deadline": "hard"}, map[string]any{"kind": "deadline_hard", "dtstart": "2026-11-14T17:00:00"}},
		{"window by duration", map[string]any{"start": "2026-11-09T09:00", "window": "2h"}, map[string]any{"kind": "window", "dtstart": "2026-11-09T09:00:00", "dtend": "2026-11-09T11:00:00"}},
		{"window by days on a date", map[string]any{"start": "2026-11-09", "window": "3d"}, map[string]any{"kind": "window", "dtstart": "2026-11-09", "dtend": "2026-11-12"}},
		{"window by end wall", map[string]any{"start": "2026-11-09T09:00", "window": "2026-11-09T17:00"}, map[string]any{"kind": "window", "dtstart": "2026-11-09T09:00:00", "dtend": "2026-11-09T17:00:00"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := withWorkspace(tc.in)
			in["ref"] = "TASK-1"
			res, seen := remoteCall(t, "remind", in)
			if res.IsError || len(seen) != 1 {
				t.Fatalf("res=%s seen=%+v", textOf(res), seen)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(seen[0].Body), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("body = %v, want %v", got, tc.want)
			}
		})
	}
	refused := map[string]map[string]any{
		"start is required":            {"every": "daily"},
		"one-shot and schedule mixed":  {"start": "2026-11-09T09:00", "remind_at": "2026-08-01T09:00:00Z"},
		"tz and floating together":     {"start": "2026-11-09T09:00", "tz": "UTC", "floating": true},
		"deadline and window together": {"start": "2026-11-09T09:00", "deadline": "hard", "window": "2h"},
		"deadline none on a new one":   {"start": "2026-11-09T09:00", "deadline": "none"},
		"an unknown deadline":          {"start": "2026-11-09T09:00", "deadline": "firm"},
		"an hour window on a date":     {"start": "2026-11-09", "window": "2h"},
	}
	for name, in := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			in = withWorkspace(in)
			in["ref"] = "TASK-1"
			res, seen := remoteCall(t, "remind", in)
			if !res.IsError || len(seen) != 0 {
				t.Fatalf("want a refusal with no request; res=%s seen=%+v", textOf(res), seen)
			}
		})
	}
}

func TestTASK4105_ScheduleReadEditDelete(t *testing.T) {
	t.Run("schedule", func(t *testing.T) {
		_, disp := stdioCall(t, "schedule", map[string]any{"ref": "TASK-1"})
		if got := strings.Join(disp.gotPath, " "); got != "item remind show" {
			t.Fatalf("cmdPath = %q", got)
		}
		_, seen := remoteCall(t, "schedule", map[string]any{"ref": "TASK-1", "workspace": "docapp"})
		if want := []recordedRequest{{"GET", "/api/v1/workspaces/docapp/items/TASK-1/schedule", "", ""}}; !reflect.DeepEqual(seen, want) {
			t.Fatalf("requests = %+v", seen)
		}
	})
	t.Run("edit", func(t *testing.T) {
		_, disp := stdioCall(t, "schedule-edit", map[string]any{"ref": "TASK-1", "deadline": "none", "every": "daily"})
		if got := strings.Join(disp.gotPath, " "); got != "item remind edit" || !hasFlagPair(disp.gotArgs, "--deadline", "none") {
			t.Fatalf("cmdPath = %q args = %q", got, disp.gotArgs)
		}
		cases := []struct {
			name string
			in   map[string]any
			want map[string]any
		}{
			{"deadline none returns to a check", map[string]any{"deadline": "none"}, map[string]any{"kind": "check", "dtend": ""}},
			{"deadline hard", map[string]any{"deadline": "hard"}, map[string]any{"kind": "deadline_hard", "dtend": ""}},
			{"start and rule", map[string]any{"start": "2026-11-16T10:00", "every": "weekly"}, map[string]any{"dtstart": "2026-11-16T10:00:00", "rrule": "weekly"}},
			{"a window by end wall", map[string]any{"window": "2026-11-09T17:00"}, map[string]any{"kind": "window", "dtend": "2026-11-09T17:00:00"}},
			{"a window by duration with the start", map[string]any{"start": "2026-11-09T09:00", "window": "90m"}, map[string]any{"kind": "window", "dtstart": "2026-11-09T09:00:00", "dtend": "2026-11-09T10:30:00"}},
			{"tz pins", map[string]any{"tz": "Europe/Paris"}, map[string]any{"tz": "Europe/Paris", "floating": false}},
			{"floating unpins", map[string]any{"floating": true}, map[string]any{"floating": true}},
			{"audience and anchor", map[string]any{"audience": "human", "anchor": "calendar"}, map[string]any{"audience": "human", "anchor": "calendar"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := withWorkspace(tc.in)
				in["ref"] = "TASK-1"
				res, seen := remoteCall(t, "schedule-edit", in)
				if res.IsError || len(seen) != 1 || seen[0].Method != "PATCH" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-1/schedule" {
					t.Fatalf("res=%s seen=%+v", textOf(res), seen)
				}
				var got map[string]any
				if err := json.Unmarshal([]byte(seen[0].Body), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("body = %v, want %v", got, tc.want)
				}
			})
		}
		for name, in := range map[string]map[string]any{
			"nothing to change":            {},
			"a duration needs the start":   {"window": "2h"},
			"tz and floating together":     {"tz": "UTC", "floating": true},
			"deadline and window together": {"deadline": "hard", "window": "2026-11-09T17:00"},
		} {
			t.Run("refuses "+name, func(t *testing.T) {
				in = withWorkspace(in)
				in["ref"] = "TASK-1"
				res, seen := remoteCall(t, "schedule-edit", in)
				if !res.IsError || len(seen) != 0 {
					t.Fatalf("want a refusal with no request; res=%s seen=%+v", textOf(res), seen)
				}
			})
		}
	})
	t.Run("delete", func(t *testing.T) {
		_, disp := stdioCall(t, "schedule-delete", map[string]any{"ref": "TASK-1"})
		if got := strings.Join(disp.gotPath, " "); got != "item remind rm" {
			t.Fatalf("cmdPath = %q", got)
		}
		if !contains(disp.gotArgs, "--yes") {
			t.Fatalf("args %q must carry --yes: the subprocess cannot answer the prompt", disp.gotArgs)
		}
		_, seen := remoteCall(t, "schedule-delete", map[string]any{"ref": "TASK-1", "workspace": "docapp"})
		if want := []recordedRequest{{"DELETE", "/api/v1/workspaces/docapp/items/TASK-1/schedule", "", ""}}; !reflect.DeepEqual(seen, want) {
			t.Fatalf("requests = %+v", seen)
		}
	})
}

// schedule-resolve: one refusal text on both transports, occurrence_at
// required on both, and each state reaches its own command and the one route.
func TestTASK4105_ScheduleResolveParity(t *testing.T) {
	good := map[string]any{"ref": "TASK-1", "state": "snooze", "occurrence_at": "2026-11-09T09:00:00Z", "until": "2099-01-01T09:00:00Z", "comment": "later"}
	refusals := map[string]map[string]any{
		"no occurrence_at":          {"ref": "TASK-1", "state": "done"},
		"blank occurrence_at":       {"ref": "TASK-1", "state": "done", "occurrence_at": "  "},
		"no state":                  {"ref": "TASK-1", "occurrence_at": "2026-11-09T09:00:00Z"},
		"an unknown state":          {"ref": "TASK-1", "state": "finish", "occurrence_at": "2026-11-09T09:00:00Z"},
		"not an instant":            {"ref": "TASK-1", "state": "done", "occurrence_at": "tomorrow"},
		"snooze with no until":      {"ref": "TASK-1", "state": "snooze", "occurrence_at": "2026-11-09T09:00:00Z"},
		"move until not an instant": {"ref": "TASK-1", "state": "move", "occurrence_at": "2026-11-09T09:00:00Z", "until": "2h"},
		"done with an until":        {"ref": "TASK-1", "state": "done", "occurrence_at": "2026-11-09T09:00:00Z", "until": "2099-01-01T09:00:00Z"},
	}
	for name, in := range refusals {
		t.Run(name, func(t *testing.T) {
			sres, disp := stdioCall(t, "schedule-resolve", in)
			rres, seen := remoteCall(t, "schedule-resolve", withWorkspace(in))
			if !sres.IsError || !rres.IsError {
				t.Fatalf("both transports must refuse; stdio=%v remote=%v", sres.IsError, rres.IsError)
			}
			if len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("nothing may be dispatched; stdio=%v remote=%+v", disp.gotPath, seen)
			}
			if textOf(sres) != textOf(rres) {
				t.Fatalf("refusal text differs:\nstdio:  %s\nremote: %s", textOf(sres), textOf(rres))
			}
			if !strings.Contains(textOf(sres), "occurrence_at") && !strings.Contains(textOf(sres), "state") && !strings.Contains(textOf(sres), "until") {
				t.Fatalf("refusal names none of occurrence_at, state, until: %s", textOf(sres))
			}
		})
	}
	for _, state := range []string{"done", "skip", "snooze", "move"} {
		t.Run("dispatches "+state, func(t *testing.T) {
			in := copyInput(good)
			in["state"] = state
			wantBody := map[string]any{"state": state, "occurrence_at": "2026-11-09T09:00:00Z", "comment": "later", "until": "2099-01-01T09:00:00Z"}
			if state == "done" || state == "skip" {
				delete(in, "until")
				delete(wantBody, "until")
			}
			_, disp := stdioCall(t, "schedule-resolve", in)
			if got, want := strings.Join(disp.gotPath, " "), "item remind "+state; got != want {
				t.Fatalf("cmdPath = %q, want %q", got, want)
			}
			if !hasFlagPair(disp.gotArgs, "--occurrence", "2026-11-09T09:00:00Z") || !hasFlagPair(disp.gotArgs, "--comment", "later") {
				t.Fatalf("args = %q", disp.gotArgs)
			}
			res, seen := remoteCall(t, "schedule-resolve", withWorkspace(in))
			if res.IsError || len(seen) != 1 || seen[0].Method != "POST" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-1/schedule/resolve" {
				t.Fatalf("res=%s seen=%+v", textOf(res), seen)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(seen[0].Body), &body); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(body, wantBody) {
				t.Fatalf("body = %v, want %v", body, wantBody)
			}
		})
	}
}

func TestTASK4105_AgendaRequiresItsWindowOnBothTransports(t *testing.T) {
	window := map[string]any{"from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z"}
	for name, in := range map[string]map[string]any{
		"no from": {"to": window["to"]},
		"no to":   {"from": window["from"]},
		"neither": {},
	} {
		t.Run(name, func(t *testing.T) {
			sres, disp := stdioCall(t, "agenda", in)
			rres, seen := remoteCall(t, "agenda", withWorkspace(in))
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("both must refuse before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
			}
			if textOf(sres) != textOf(rres) {
				t.Fatalf("refusal text differs:\nstdio:  %s\nremote: %s", textOf(sres), textOf(rres))
			}
		})
	}
	in := copyInput(window)
	in["tz"] = "Europe/Paris"
	_, disp := stdioCall(t, "agenda", in)
	if got := strings.Join(disp.gotPath, " "); got != "item remind agenda" || !hasFlagPair(disp.gotArgs, "--from", "2026-11-09T00:00:00Z") || !hasFlagPair(disp.gotArgs, "--tz", "Europe/Paris") {
		t.Fatalf("cmdPath = %q args = %q", got, disp.gotArgs)
	}
	_, seen := remoteCall(t, "agenda", withWorkspace(in))
	if len(seen) != 1 || seen[0].Method != "GET" || seen[0].Path != "/api/v1/workspaces/docapp/agenda" {
		t.Fatalf("requests = %+v", seen)
	}
	q, _ := url.ParseQuery(seen[0].Query)
	if q.Get("from") != "2026-11-09T00:00:00Z" || q.Get("to") != "2026-11-16T00:00:00Z" || q.Get("tz") != "Europe/Paris" {
		t.Fatalf("query = %v", q)
	}
	_, seen = remoteCall(t, "agenda", withWorkspace(window))
	if q, _ := url.ParseQuery(seen[0].Query); q.Has("tz") {
		t.Fatalf("tz must be left out when not given: %v", q)
	}
}

// The remote bodies are accepted by the real server: create, read, edit,
// resolve, the agenda, and delete, as the owner; a viewer may read only.
func TestTASK4105_ScheduleRemoteAgainstTheServer(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	mkUser := func(email string) *models.User {
		u, err := s.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, viewer := mkUser("sch-owner@example.com"), mkUser("sch-viewer@example.com")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Sch WS", Slug: "sch-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		u    *models.User
		role string
	}{{owner, "owner"}, {viewer, "viewer"}} {
		if err := s.AddWorkspaceMember(ws.ID, m.u.ID, m.role); err != nil {
			t.Fatal(err)
		}
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Recurring", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	call := func(as *models.User, action string, in map[string]any) (map[string]any, string, bool) {
		t.Helper()
		disp := &HTTPHandlerDispatcher{Handler: http.HandlerFunc(srv.ServeHTTP), UserResolver: func(context.Context) *models.User { return as }}
		env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: disp}
		in = copyInput(in)
		in["workspace"] = ws.Slug
		res, err := scheduleItemAction(t, action)(context.Background(), in, env)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		text := resultText(res)
		var out map[string]any
		_ = json.Unmarshal([]byte(text), &out)
		return out, text, res.IsError
	}
	ref := item.Ref
	start := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02") + "T09:00"

	if _, text, isErr := call(owner, "remind", map[string]any{"ref": ref, "start": start, "every": "daily", "tz": "UTC"}); isErr {
		t.Fatalf("create: %s", text)
	}
	if _, text, isErr := call(owner, "remind", map[string]any{"ref": ref, "start": start}); !isErr || !strings.Contains(text, "already has a schedule") {
		t.Fatalf("a second create must be refused: the item already has a schedule: %s", text)
	}
	view, text, isErr := call(viewer, "schedule", map[string]any{"ref": ref})
	if isErr || view["kind"] != "check" || view["rrule"] == nil {
		t.Fatalf("viewer read: err=%v %s", isErr, text)
	}
	if _, text, isErr := call(viewer, "schedule-delete", map[string]any{"ref": ref}); !isErr {
		t.Fatalf("a viewer must not delete a schedule: %s", text)
	}
	if view, text, isErr := call(owner, "schedule-edit", map[string]any{"ref": ref, "deadline": "soft", "audience": "agent"}); isErr || view["kind"] != "deadline_soft" || view["audience"] != "agent" {
		t.Fatalf("edit: %s", text)
	}
	occ, _ := view["next_occurrence"].(map[string]any)
	at, _ := occ["at"].(string)
	if at == "" {
		t.Fatalf("schedule has no next occurrence: %v", view)
	}
	if _, text, isErr := call(owner, "schedule-resolve", map[string]any{"ref": ref, "state": "move", "occurrence_at": at, "until": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)}); isErr {
		t.Fatalf("moving a soft deadline needs no comment: %s", text)
	}
	if _, text, isErr := call(owner, "schedule-resolve", map[string]any{"ref": ref, "state": "skip", "occurrence_at": "2000-01-01T00:00:00Z"}); !isErr || !strings.Contains(text, "not the schedule's outstanding occurrence") {
		t.Fatalf("a wrong occurrence must be refused as stale: %s", text)
	}
	if _, text, isErr := call(owner, "schedule-resolve", map[string]any{"ref": ref, "state": "skip", "occurrence_at": at, "comment": "not this week"}); isErr {
		t.Fatalf("resolve: %s", text)
	}
	from := time.Now().UTC().Format(time.RFC3339)
	to := time.Now().UTC().AddDate(0, 0, 10).Format(time.RFC3339)
	agenda, text, isErr := call(owner, "agenda", map[string]any{"from": from, "to": to, "tz": "UTC"})
	if isErr || agenda["entries"] == nil {
		t.Fatalf("agenda: %s", text)
	}
	if out, text, isErr := call(owner, "schedule-delete", map[string]any{"ref": ref}); isErr || out["deleted"] != true {
		t.Fatalf("delete: %s", text)
	}
	if _, text, isErr := call(owner, "schedule", map[string]any{"ref": ref}); !isErr || !strings.Contains(text, "not_found") {
		t.Fatalf("after delete the schedule must be gone: %s", text)
	}
}

// The new params are declared with the descriptions an agent acts on.
func TestTASK4105_NewParamsAreDeclared(t *testing.T) {
	declared := map[string]ParamDef{}
	for _, p := range padItemSchemaParams {
		declared[p.Name] = p
	}
	for _, name := range []string{"every", "start", "deadline", "window", "anchor", "audience", "tz", "floating", "state", "until", "occurrence_at", "from", "to"} {
		p, ok := declared[name]
		if !ok {
			t.Errorf("param %q is not declared", name)
			continue
		}
		if (name == "floating") != (p.Type == "bool") {
			t.Errorf("param %q has type %q", name, p.Type)
		}
	}
	if d := declared["deadline"].Description; !strings.Contains(d, "none") || !strings.Contains(strings.ToLower(d), "schedule-edit") {
		t.Errorf("deadline must say none is a schedule-edit value: %q", d)
	}
	if d := declared["remind_at"].Description; !strings.Contains(d, "one-shot") {
		t.Errorf("remind_at must say it is required only for the one-shot form: %q", d)
	}
	if d := declared["comment"].Description; !strings.Contains(d, "soft deadline") {
		t.Errorf("comment must mention moving a soft deadline: %q", d)
	}
	for _, d := range Catalog {
		if d.Name == "pad_item" && !strings.Contains(d.Description, "schedule-delete") {
			t.Error("the pad_item description does not describe schedule-delete")
		}
	}
}

// An empty or false schedule member is "not given" on both transports: it is
// not an edit, and it cannot clear a rule or unpin a zone by being padded in.
func TestTASK4105_EditReadsEmptyMembersAsNotGiven(t *testing.T) {
	pad := map[string]any{"every": "", "start": " ", "tz": "", "window": "", "anchor": "", "audience": "", "floating": false}
	t.Run("nothing else given is refused on both", func(t *testing.T) {
		in := copyInput(pad)
		in["ref"] = "TASK-1"
		sres, disp := stdioCall(t, "schedule-edit", in)
		rres, seen := remoteCall(t, "schedule-edit", withWorkspace(in))
		if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
			t.Fatalf("want both refused before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
		}
		if textOf(sres) != textOf(rres) || !strings.Contains(textOf(sres), "nothing to change") {
			t.Fatalf("stdio: %s\nremote: %s", textOf(sres), textOf(rres))
		}
	})
	t.Run("a real member is sent alone", func(t *testing.T) {
		in := copyInput(pad)
		in["ref"], in["deadline"] = "TASK-1", "hard"
		_, disp := stdioCall(t, "schedule-edit", in)
		for _, flag := range []string{"--every", "--start", "--tz", "--window", "--floating"} {
			if contains(disp.gotArgs, flag) {
				t.Errorf("stdio sent %s: %q", flag, disp.gotArgs)
			}
		}
		if !hasFlagPair(disp.gotArgs, "--deadline", "hard") {
			t.Errorf("stdio args = %q", disp.gotArgs)
		}
		_, seen := remoteCall(t, "schedule-edit", withWorkspace(in))
		if len(seen) != 1 || seen[0].Body != `{"dtend":"","kind":"deadline_hard"}` {
			t.Fatalf("remote requests = %+v", seen)
		}
	})
}

// A wrongly typed schedule member is refused naming it, before any request.
func TestTASK4105_RemoteRefusesWronglyTypedMembers(t *testing.T) {
	for name, in := range map[string]map[string]any{
		"every is a number":     {"start": "2026-11-09T09:00", "every": 5.0},
		"start is an object":    {"start": map[string]any{"a": 1.0}},
		"floating is a string":  {"start": "2026-11-09T09:00", "floating": "yes"},
		"remind_at with a rule": {"start": "2026-11-09T09:00", "remind_at": "2026-08-01T09:00:00Z"},
	} {
		t.Run(name, func(t *testing.T) {
			in = withWorkspace(in)
			in["ref"] = "TASK-1"
			res, seen := remoteCall(t, "remind", in)
			if !res.IsError || len(seen) != 0 {
				t.Fatalf("want a refusal with no request; res=%s seen=%+v", textOf(res), seen)
			}
		})
	}
}

// Stdio and /mcp read the same schedule input the same way: a wrong type is
// refused naming the first offender in a fixed order, a one-shot time never
// rides with schedule members whatever its type, and padded values are trimmed
// before they reach the CLI.
func TestTASK4105_SchedulePrechecksAgreeAcrossTransports(t *testing.T) {
	refused := map[string]struct {
		in   map[string]any
		want string
	}{
		"two wrong members name the first":    {map[string]any{"start": "2026-11-09T09:00", "every": 5.0, "tz": 7.0}, "every must be a string"},
		"false in a string member":            {map[string]any{"remind_at": "2026-11-09T09:00:00Z", "every": false}, "every must be a string"},
		"floating is a string":                {map[string]any{"start": "2026-11-09T09:00", "floating": "yes"}, "floating must be a boolean"},
		"remind_at of another type with rule": {map[string]any{"start": "2026-11-09T09:00", "remind_at": 123.0}, "cannot be combined"},
	}
	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			in := copyInput(c.in)
			in["ref"] = "TASK-1"
			sres, disp := stdioCall(t, "remind", in)
			rres, seen := remoteCall(t, "remind", withWorkspace(in))
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("want both refused before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
			}
			if !strings.Contains(textOf(sres), c.want) || !strings.Contains(textOf(rres), c.want) {
				t.Fatalf("want %q\nstdio: %s\nremote: %s", c.want, textOf(sres), textOf(rres))
			}
		})
	}
	t.Run("padded values are trimmed before the CLI", func(t *testing.T) {
		in := map[string]any{"ref": "TASK-1", "start": " 2026-11-09T09:00 ", "deadline": " hard"}
		_, disp := stdioCall(t, "remind", in)
		if !hasFlagPair(disp.gotArgs, "--deadline", "hard") || !hasFlagPair(disp.gotArgs, "--start", "2026-11-09T09:00") {
			t.Fatalf("stdio args = %q", disp.gotArgs)
		}
	})
}

// A non-string until is refused for every state, not read as absent.
func TestTASK4105_ResolveRefusesANonStringUntil(t *testing.T) {
	for _, state := range []string{"done", "skip", "snooze", "move"} {
		t.Run(state, func(t *testing.T) {
			in := map[string]any{"ref": "TASK-1", "state": state, "occurrence_at": "2026-11-09T09:00:00Z", "until": 123.0}
			sres, disp := stdioCall(t, "schedule-resolve", in)
			rres, seen := remoteCall(t, "schedule-resolve", withWorkspace(in))
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("want both refused before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
			}
			if !strings.Contains(textOf(sres), "until must be a string") || textOf(sres) != textOf(rres) {
				t.Fatalf("stdio: %s\nremote: %s", textOf(sres), textOf(rres))
			}
		})
	}
}

// A non-string comment is refused too, not dropped on /mcp and stringified on
// stdio.
func TestTASK4105_ResolveRefusesANonStringComment(t *testing.T) {
	in := map[string]any{"ref": "TASK-1", "state": "done", "occurrence_at": "2026-11-09T09:00:00Z", "comment": 17.0}
	sres, disp := stdioCall(t, "schedule-resolve", in)
	rres, seen := remoteCall(t, "schedule-resolve", withWorkspace(in))
	if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
		t.Fatalf("want both refused before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
	}
	if !strings.Contains(textOf(sres), "comment must be a string") || textOf(sres) != textOf(rres) {
		t.Fatalf("stdio: %s\nremote: %s", textOf(sres), textOf(rres))
	}
}

// An instant with a fraction of a second is refused on both transports: the CLI
// formats to the second and the route keeps the fraction, so the same value
// would land on different seconds.
func TestTASK4105_FractionalInstantsAreRefused(t *testing.T) {
	cases := map[string]struct {
		action string
		in     map[string]any
		want   string
	}{
		"snooze until":  {"schedule-resolve", map[string]any{"state": "snooze", "occurrence_at": "2026-11-09T09:00:00Z", "until": "2026-11-09T09:00:00.5Z"}, "until"},
		"occurrence_at": {"schedule-resolve", map[string]any{"state": "done", "occurrence_at": "2026-11-09T09:00:00.25Z"}, "occurrence_at"},
		"agenda from":   {"agenda", map[string]any{"from": "2026-11-09T09:00:00.5Z", "to": "2026-11-16T00:00:00Z"}, "from"},
		"agenda to":     {"agenda", map[string]any{"from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00.1Z"}, "to"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := copyInput(c.in)
			in["ref"] = "TASK-1"
			sres, disp := stdioCall(t, c.action, in)
			rres, seen := remoteCall(t, c.action, withWorkspace(in))
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("want both refused before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
			}
			if !strings.Contains(textOf(sres), "fraction of a second") || !strings.Contains(textOf(sres), c.want) || textOf(sres) != textOf(rres) {
				t.Fatalf("stdio: %s\nremote: %s", textOf(sres), textOf(rres))
			}
		})
	}
	t.Run("a whole-second instant still goes through", func(t *testing.T) {
		in := map[string]any{"ref": "TASK-1", "from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z"}
		if res, disp := stdioCall(t, "agenda", in); res.IsError || len(disp.gotArgs) == 0 {
			t.Fatalf("stdio refused or did not dispatch: %s", textOf(res))
		}
	})
}

// A non-string tz on agenda is refused, not dropped on /mcp and passed as text
// on stdio.
func TestTASK4105_AgendaRefusesANonStringTz(t *testing.T) {
	in := map[string]any{"ref": "TASK-1", "from": "2026-11-09T00:00:00Z", "to": "2026-11-16T00:00:00Z", "tz": false}
	sres, disp := stdioCall(t, "agenda", in)
	rres, seen := remoteCall(t, "agenda", withWorkspace(in))
	if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
		t.Fatalf("want both refused before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
	}
	if !strings.Contains(textOf(sres), "tz must be a string") || textOf(sres) != textOf(rres) {
		t.Fatalf("stdio: %s\nremote: %s", textOf(sres), textOf(rres))
	}
}

// A blank remind_at beside a schedule is not given on either transport: the
// schedule is armed, and stdio does not forward an empty --remind-at.
func TestTASK4105_BlankRemindAtBesideAScheduleIsNotGiven(t *testing.T) {
	for _, blank := range []any{" ", "", nil} {
		in := map[string]any{"ref": "TASK-1", "start": "2026-11-09T09:00", "remind_at": blank}
		sres, disp := stdioCall(t, "remind", copyInput(in))
		rres, seen := remoteCall(t, "remind", withWorkspace(in))
		if sres.IsError || rres.IsError {
			t.Fatalf("remind_at=%#v refused: stdio=%s remote=%s", blank, textOf(sres), textOf(rres))
		}
		if contains(disp.gotArgs, "--remind-at") || !hasFlagPair(disp.gotArgs, "--start", "2026-11-09T09:00") {
			t.Errorf("remind_at=%#v: stdio args = %q", blank, disp.gotArgs)
		}
		if len(seen) != 1 || !strings.HasSuffix(seen[0].Path, "/schedule") {
			t.Errorf("remind_at=%#v: remote requests = %+v", blank, seen)
		}
	}
}
