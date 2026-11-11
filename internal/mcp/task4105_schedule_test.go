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
	case "pad_item.schedule-link", "item remind link add":
		in["child"], in["parent_role"], in["child_role"] = "TASK-2", "target", "target"
		in["link_offset"] = map[string]any{"mode": "elapsed", "elapsed_secs": 0}
	case "item remind link edit":
		in["link_id"], in["cancel_policy"] = "link-1", "waiting_on"
	case "pad_item.schedule-unlink", "item remind link rm":
		in["link_id"] = "link-1"
	case "pad_item.schedule-gate", "item remind gate add":
		in["blocker"] = "TASK-2"
	case "item remind gate rm":
		in["gate_id"] = "gate-1"
	case "pad_item.schedule-move":
		in["role"], in["to"] = "target", "2099-01-01T09:00:00Z"
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
		_, _ = w.Write([]byte(`{"dtstart":"2026-11-09T09:00:00"}`))
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
		"schedule-edit": false, "schedule-delete": false, "schedule-resolve": false, "schedule-renegotiate": false, "remind": false,
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
		if len(seen) != 1 || seen[0].Method != "POST" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-1/commitment" {
			t.Fatalf("requests = %+v", seen)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(seen[0].Body), &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"roles": map[string]any{"target_days": float64(0), "target_secs": float64(0)}, "dtstart": "2026-11-09T09:00:00", "rrule": "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", "anchor": "completion", "audience": "agent", "tz": "America/New_York"}
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

func TestTASK4105_RemoteCreateRefusals(t *testing.T) {
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
		if want := []recordedRequest{{"GET", "/api/v1/workspaces/docapp/items/TASK-1/commitment", "", ""}}; !reflect.DeepEqual(seen, want) {
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
			{"deadline none clears only the deadline", map[string]any{"deadline": "none"}, map[string]any{"roles": map[string]any{"deadline_days": nil, "deadline_secs": nil, "deadline_behavior": nil}}},
			{"deadline hard", map[string]any{"deadline": "hard"}, map[string]any{"roles": map[string]any{"deadline_days": float64(0), "deadline_secs": float64(0), "deadline_behavior": "stays_owed"}}},
			{"start and rule", map[string]any{"start": "2026-11-16T10:00", "every": "weekly"}, map[string]any{"dtstart": "2026-11-16T10:00:00", "rrule": "FREQ=WEEKLY"}},
			{"a window by end wall", map[string]any{"window": "2026-11-09T17:00"}, map[string]any{"roles": map[string]any{"occupied_days": float64(0), "occupied_secs": float64(28800)}}},
			{"a window by duration with the start", map[string]any{"start": "2026-11-09T09:00", "window": "90m"}, map[string]any{"dtstart": "2026-11-09T09:00:00", "roles": map[string]any{"occupied_days": float64(0), "occupied_secs": float64(5400)}}},
			{"tz pins", map[string]any{"tz": "Europe/Paris"}, map[string]any{"tz": "Europe/Paris", "floating": false}},
			{"floating unpins", map[string]any{"floating": true}, map[string]any{"floating": true}},
			{"audience and anchor", map[string]any{"audience": "human", "anchor": "calendar"}, map[string]any{"audience": "human", "anchor": "calendar"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := withWorkspace(tc.in)
				in["ref"] = "TASK-1"
				res, seen := remoteCall(t, "schedule-edit", in)
				if len(seen) == 2 && seen[0].Method == "GET" {
					seen = seen[1:]
				}
				if res.IsError || len(seen) != 1 || seen[0].Method != "PATCH" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-1/commitment" {
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
		if want := []recordedRequest{{"DELETE", "/api/v1/workspaces/docapp/items/TASK-1/commitment", "", ""}}; !reflect.DeepEqual(seen, want) {
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
			if res.IsError || len(seen) != 1 || seen[0].Method != "POST" || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-1/commitment/resolve" {
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
	if isErr || view["kind"] != nil || view["roles"] == nil || view["rrule"] == nil {
		t.Fatalf("viewer read: err=%v %s", isErr, text)
	}
	if _, text, isErr := call(viewer, "schedule-delete", map[string]any{"ref": ref}); !isErr {
		t.Fatalf("a viewer must not delete a schedule: %s", text)
	}
	if view, text, isErr := call(owner, "schedule-edit", map[string]any{"ref": ref, "deadline": "soft", "audience": "agent"}); isErr || view["kind"] != nil || view["roles"] == nil || view["audience"] != "agent" {
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
	if d := declared["comment"].Description; !strings.Contains(d, "schedule-resolve") || !strings.Contains(d, "Optional") {
		t.Errorf("comment must describe optional schedule-resolve text: %q", d)
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
		if len(seen) != 1 || seen[0].Body != `{"roles":{"deadline_behavior":"stays_owed","deadline_days":0,"deadline_secs":0}}` {
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
		if len(seen) != 1 || !strings.HasSuffix(seen[0].Path, "/commitment") {
			t.Errorf("remind_at=%#v: remote requests = %+v", blank, seen)
		}
	}
}

func TestCommitmentResolveBodyHasOnlySupportedMembers(t *testing.T) {
	_, _, raw, err := mapScheduleResolve("done")(map[string]any{"workspace": "w", "ref": "TASK-1", "occurrence": "2099-01-01T09:00:00Z", "comment": "done", "until": "2099-01-02T09:00:00Z", "actual_done_at": "2099-01-01T08:00:00Z", "reason": "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	want := map[string]any{"state": "done", "occurrence_at": "2099-01-01T09:00:00Z", "comment": "done", "until": "2099-01-02T09:00:00Z", "actual_done_at": "2099-01-01T08:00:00Z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body %s", raw)
	}
}

// TASK-4356: schedule-resolve takes actual_done_at (done only), and
// schedule-renegotiate previews or applies a deadline change.
func TestTASK4356_ActualDoneAtParity(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"ref": "TASK-1", "state": "done", "occurrence_at": "2026-11-09T09:00:00Z"}
	}
	refusals := map[string]map[string]any{
		"not a string":   {"actual_done_at": 7},
		"not an instant": {"actual_done_at": "yesterday"},
		"a bare date":    {"actual_done_at": "2026-11-08"},
		"a fraction":     {"actual_done_at": "2026-11-08T09:00:00.5Z"},
		"on skip":        {"state": "skip", "actual_done_at": "2026-11-08T09:00:00Z"},
		"on snooze":      {"state": "snooze", "until": "2099-01-01T09:00:00Z", "actual_done_at": "2026-11-08T09:00:00Z"},
		"on move":        {"state": "move", "until": "2099-01-01T09:00:00Z", "actual_done_at": "2026-11-08T09:00:00Z"},
	}
	for name, extra := range refusals {
		t.Run(name, func(t *testing.T) {
			in := base()
			for k, v := range extra {
				in[k] = v
			}
			sres, disp := stdioCall(t, "schedule-resolve", in)
			rres, seen := remoteCall(t, "schedule-resolve", withWorkspace(in))
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("both must refuse before dispatch: stdio=%v remote=%v %v %+v", sres.IsError, rres.IsError, disp.gotPath, seen)
			}
			if textOf(sres) != textOf(rres) || !strings.Contains(textOf(sres), "actual_done_at") {
				t.Fatalf("stdio: %s\nremote: %s", textOf(sres), textOf(rres))
			}
		})
	}
	t.Run("given", func(t *testing.T) {
		in := base()
		in["actual_done_at"] = " 2026-11-08T09:00:00Z "
		_, disp := stdioCall(t, "schedule-resolve", in)
		if !hasFlagPair(disp.gotArgs, "--at", "2026-11-08T09:00:00Z") {
			t.Fatalf("stdio args = %q", disp.gotArgs)
		}
		res, seen := remoteCall(t, "schedule-resolve", withWorkspace(in))
		if res.IsError || len(seen) != 1 || seen[0].Body != `{"occurrence_at":"2026-11-09T09:00:00Z","state":"done","actual_done_at":"2026-11-08T09:00:00Z"}` {
			t.Fatalf("remote: %s %+v", textOf(res), seen)
		}
	})
	// Inert values are not given: no flag on stdio, no member on the route.
	for name, v := range map[string]any{"blank": "  ", "null": nil} {
		t.Run("inert "+name, func(t *testing.T) {
			in := base()
			in["actual_done_at"] = v
			_, disp := stdioCall(t, "schedule-resolve", in)
			for _, a := range disp.gotArgs {
				if a == "--at" {
					t.Fatalf("stdio sent --at: %q", disp.gotArgs)
				}
			}
			_, seen := remoteCall(t, "schedule-resolve", withWorkspace(in))
			if len(seen) != 1 || strings.Contains(seen[0].Body, "actual_done_at") {
				t.Fatalf("remote body: %+v", seen)
			}
		})
	}
}

func TestTASK4356_RenegotiateParity(t *testing.T) {
	good := map[string]any{"ref": "TASK-1", "reason": "slipped", "deadline": "2099-01-01T09:00:00Z"}
	refusals := map[string]map[string]any{
		"no reason":              {"reason": nil},
		"blank reason":           {"reason": "  "},
		"reason not a string":    {"reason": 3},
		"apply with no revision": {"preview": false},
		"apply blank revision":   {"preview": false, "revision": " "},
		"preview not a bool":     {"preview": "no"},
		"deadline not instant":   {"deadline": "tomorrow"},
		"deadline a bare date":   {"deadline": "2099-01-01"},
		"deadline a fraction":    {"deadline": "2099-01-01T09:00:00.5Z"},
		"days not whole":         {"scope": "series", "deadline": nil, "deadline_days": 1.5},
		"secs not a number":      {"scope": "series", "deadline": nil, "deadline_secs": "abc"},
		"remove not a bool":      {"scope": "series", "deadline": nil, "remove_deadline": "yes"},
	}
	for name, extra := range refusals {
		t.Run(name, func(t *testing.T) {
			in := copyInput(good)
			for k, v := range extra {
				if v == nil {
					delete(in, k)
				} else {
					in[k] = v
				}
			}
			sres, disp := stdioCall(t, "schedule-renegotiate", in)
			rres, seen := remoteCall(t, "schedule-renegotiate", withWorkspace(in))
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("both must refuse before dispatch: stdio=%v remote=%v %v %+v", sres.IsError, rres.IsError, disp.gotPath, seen)
			}
			if textOf(sres) != textOf(rres) {
				t.Fatalf("stdio: %s\nremote: %s", textOf(sres), textOf(rres))
			}
		})
	}
	const route = "/api/v1/workspaces/docapp/items/TASK-1/commitment/renegotiate"
	check := func(t *testing.T, in map[string]any, wantFlags [][2]string, wantBody map[string]any) {
		t.Helper()
		_, disp := stdioCall(t, "schedule-renegotiate", in)
		if got := strings.Join(disp.gotPath, " "); got != "item remind renegotiate" {
			t.Fatalf("cmdPath = %q", got)
		}
		for _, f := range wantFlags {
			if !hasFlagPair(disp.gotArgs, f[0], f[1]) {
				t.Errorf("args %q lack %s %s", disp.gotArgs, f[0], f[1])
			}
		}
		res, seen := remoteCall(t, "schedule-renegotiate", withWorkspace(in))
		if res.IsError || len(seen) != 1 || seen[0].Method != "POST" || seen[0].Path != route {
			t.Fatalf("remote: %s %+v", textOf(res), seen)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(seen[0].Body), &body); err != nil || !reflect.DeepEqual(body, wantBody) {
			t.Fatalf("body = %s, want %v", seen[0].Body, wantBody)
		}
	}
	t.Run("preview is the default", func(t *testing.T) {
		check(t, copyInput(good), [][2]string{{"--reason", "slipped"}, {"--deadline", "2099-01-01T09:00:00Z"}},
			map[string]any{"reason": "slipped", "scope": "occurrence", "deadline": "2099-01-01T09:00:00Z", "preview": true})
	})
	t.Run("blank members are inert", func(t *testing.T) {
		in := copyInput(good)
		in["preview"], in["revision"], in["scope"], in["deadline_behavior"] = true, "", "", " "
		check(t, in, nil, map[string]any{"reason": "slipped", "scope": "occurrence", "deadline": "2099-01-01T09:00:00Z", "preview": true})
	})
	t.Run("apply", func(t *testing.T) {
		in := copyInput(good)
		in["preview"], in["revision"] = false, "rev-1"
		_, disp := stdioCall(t, "schedule-renegotiate", in)
		if !hasFlagPair(disp.gotArgs, "--revision", "rev-1") || !contains(disp.gotArgs, "--apply") {
			t.Fatalf("stdio args = %q", disp.gotArgs)
		}
		check(t, in, nil, map[string]any{"reason": "slipped", "scope": "occurrence", "deadline": "2099-01-01T09:00:00Z", "preview": false, "revision": "rev-1"})
	})
	t.Run("series", func(t *testing.T) {
		in := map[string]any{"ref": "TASK-1", "reason": "r", "scope": "series", "deadline_days": 2, "deadline_secs": float64(3600), "deadline_behavior": "lapses"}
		check(t, in, [][2]string{{"--scope", "series"}, {"--deadline-offset-days", "2"}, {"--deadline-offset-secs", "3600"}, {"--behavior", "lapses"}},
			map[string]any{"reason": "r", "scope": "series", "deadline_offset": map[string]any{"deadline_days": float64(2), "deadline_secs": float64(3600)}, "deadline_behavior": "lapses", "preview": true})
		check(t, map[string]any{"ref": "TASK-1", "reason": "r", "scope": "series", "remove_deadline": true}, [][2]string{{"--scope", "series"}},
			map[string]any{"reason": "r", "scope": "series", "remove_deadline": true, "preview": true})
		// remove_deadline=false is not given.
		check(t, map[string]any{"ref": "TASK-1", "reason": "r", "scope": "series", "remove_deadline": false, "deadline_behavior": "stays_owed"}, nil,
			map[string]any{"reason": "r", "scope": "series", "deadline_behavior": "stays_owed", "preview": true})
	})
}

func TestTASK4356_NewParamsAreDeclared(t *testing.T) {
	declared := map[string]ParamDef{}
	for _, p := range padItemSchemaParams {
		declared[p.Name] = p
	}
	for name, typ := range map[string]string{"actual_done_at": "string", "reason": "string", "scope": "string", "deadline_days": "number", "deadline_secs": "number",
		"deadline_behavior": "string", "remove_deadline": "bool", "revision": "string", "preview": "bool"} {
		p, ok := declared[name]
		if !ok || p.Type != typ {
			t.Errorf("param %q: declared=%v type=%q want %q", name, ok, p.Type, typ)
		}
	}
	if d := declared["deadline"].Description; !strings.Contains(d, "schedule-renegotiate") {
		t.Errorf("deadline must say what schedule-renegotiate takes: %q", d)
	}
	if d := declared["actual_done_at"].Description; !strings.Contains(d, "schedule-resolve") || !strings.Contains(d, "done") {
		t.Errorf("actual_done_at: %q", d)
	}
}
