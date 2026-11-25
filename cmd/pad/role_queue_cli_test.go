package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestReadyRole_JSONAndStopMessage(t *testing.T) {
	prevFormat := formatFlag
	t.Cleanup(func() {
		formatFlag = prevFormat
		cli.SetDisplayLocationResolver(nil)
	})
	cli.SetDisplayLocationResolver(func() *time.Location { return time.UTC })

	setAt := time.Date(2026, 11, 25, 14, 30, 0, 0, time.UTC)
	var gotPath, gotQuery string
	var queuePayload map[string]any
	var dashPayload string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/demo/agent-roles/implementer/queue", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(queuePayload)
	})
	mux.HandleFunc("/api/v1/workspaces/demo/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dashPayload))
	})

	setupFormatRoutingTest(t, mux)

	// 1. JSON shape with ready items, parked items, now items, and park metadata.
	queuePayload = map[string]any{
		"workspace":   "demo",
		"role":        map[string]any{"slug": "implementer", "name": "Implementer", "icon": "🔨"},
		"can_reorder": true,
		"can_park":    true,
		"now": []map[string]any{
			{"ref": "TASK-10", "title": "In flight", "status": "in_progress", "since": setAt, "lease": nil, "live": "unknown"},
		},
		"queue": []map[string]any{
			{"ref": "TASK-1", "title": "One", "status": "open", "priority": "high", "role_sort_order": 1024, "blocked": false, "ready": true, "can_edit": true},
			{"ref": "TASK-2", "title": "Two", "status": "open", "priority": "medium", "role_sort_order": 2048, "blocked": false, "ready": true, "can_edit": true},
			{"ref": "TASK-3", "title": "Three", "status": "open", "priority": "low", "role_sort_order": 3072, "blocked": false, "ready": true, "can_edit": true},
			{"ref": "TASK-4", "title": "Four", "status": "open", "priority": "low", "role_sort_order": 4096, "blocked": false, "ready": true, "can_edit": true},
			{"ref": "TASK-5", "title": "Parked", "status": "open", "priority": "high", "role_sort_order": 5120, "blocked": false, "ready": false, "can_edit": true},
		},
		"park": map[string]any{
			"mode":             "line",
			"index":            4,
			"first_parked_ref": "TASK-5",
			"set_by":           "Alice",
			"set_at":           setAt,
			"park_seq":         1,
		},
		"queue_total":  5,
		"parked_total": 1,
		"queue_more":   false,
	}

	formatFlag = "json"
	cmd := readyCmd()
	cmd.SetArgs([]string{"--role", "implementer"})
	var runErr error
	out := captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("ready --role json: %v", runErr)
	}
	if gotPath != "/api/v1/workspaces/demo/agent-roles/implementer/queue" || gotQuery != "limit=50" {
		t.Fatalf("requested %s?%s, want .../queue?limit=50", gotPath, gotQuery)
	}
	var readyJSON struct {
		Count   int              `json:"count"`
		Results []map[string]any `json:"results"`
		Role    string           `json:"role"`
		Now     []map[string]any `json:"now"`
		Park    map[string]any   `json:"park"`
	}
	if err := json.Unmarshal([]byte(out), &readyJSON); err != nil {
		t.Fatalf("unmarshal ready --role json: %v\n%s", err, out)
	}
	if readyJSON.Count != 4 || len(readyJSON.Results) != 4 || readyJSON.Role != "implementer" || len(readyJSON.Now) != 1 || readyJSON.Park["mode"] != "line" {
		t.Fatalf("unexpected ready --role JSON: %+v", readyJSON)
	}

	// next --role caps results to 3 with the same shape.
	ncmd := nextCmd()
	ncmd.SetArgs([]string{"--role", "implementer"})
	out = captureStdout(t, func() { runErr = ncmd.Execute() })
	if runErr != nil {
		t.Fatalf("next --role json: %v", runErr)
	}
	var nextJSON struct {
		Count   int              `json:"count"`
		Results []map[string]any `json:"results"`
		Role    string           `json:"role"`
		Now     []map[string]any `json:"now"`
		Park    map[string]any   `json:"park"`
	}
	if err := json.Unmarshal([]byte(out), &nextJSON); err != nil {
		t.Fatalf("unmarshal next --role json: %v\n%s", err, out)
	}
	if nextJSON.Count != 3 || len(nextJSON.Results) != 3 || nextJSON.Role != "implementer" {
		t.Fatalf("next --role JSON should cap to 3: %+v", nextJSON)
	}

	// 2. Parked and empty with Now items -> exact stop message.
	queuePayload["queue"] = []map[string]any{
		{"ref": "TASK-5", "title": "Parked", "status": "open", "priority": "high", "role_sort_order": 5120, "blocked": false, "ready": false, "can_edit": true},
	}
	queuePayload["now"] = []map[string]any{
		{"ref": "TASK-10", "title": "In flight", "status": "in_progress", "since": setAt, "lease": nil, "live": "unknown"},
		{"ref": "TASK-11", "title": "In flight 2", "status": "in_progress", "since": setAt, "lease": nil, "live": "unknown"},
	}
	formatFlag = "table"
	cmd = readyCmd()
	cmd.SetArgs([]string{"--role", "implementer"})
	out = captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("ready --role parked with now: %v", runErr)
	}
	wantStopWithNow := "Parked by Alice at 14:30: finish TASK-10, TASK-11, then stop.\n"
	if out != wantStopWithNow {
		t.Fatalf("stop message with Now:\n got: %q\nwant: %q", out, wantStopWithNow)
	}

	// 3. Parked and empty with NO Now items and null set_by -> fallback to "someone".
	queuePayload["now"] = []map[string]any{}
	queuePayload["park"] = map[string]any{
		"mode":             "top",
		"index":            0,
		"first_parked_ref": "TASK-5",
		"set_by":           nil,
		"set_at":           setAt,
		"park_seq":         2,
	}
	cmd = readyCmd()
	cmd.SetArgs([]string{"--role", "implementer"})
	out = captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("ready --role parked no now: %v", runErr)
	}
	wantStopNoNow := "Parked by someone at 14:30: stop.\n"
	if out != wantStopNoNow {
		t.Fatalf("stop message without Now:\n got: %q\nwant: %q", out, wantStopNoNow)
	}

	// 4. Unparked and empty -> exact empty queue message; JSON omits "park".
	delete(queuePayload, "park")
	queuePayload["queue"] = []map[string]any{}
	cmd = readyCmd()
	cmd.SetArgs([]string{"--role", "implementer"})
	out = captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("ready --role unparked empty: %v", runErr)
	}
	wantEmpty := "Nothing ready in implementer's queue.\n"
	if out != wantEmpty {
		t.Fatalf("unparked empty message:\n got: %q\nwant: %q", out, wantEmpty)
	}

	formatFlag = "json"
	cmd = readyCmd()
	cmd.SetArgs([]string{"--role", "implementer"})
	out = captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("ready --role unparked json: %v", runErr)
	}
	if strings.Contains(out, `"park"`) {
		t.Fatalf("unparked ready --role JSON must omit park:\n%s", out)
	}

	// 5. Without --role, output is unchanged (uses dashboard).
	dashPayload = `{"suggested_next":[{"item_slug":"task-1","item_ref":"TASK-1","item_title":"Dash Task","collection":"tasks","reason":"High priority"}]}`
	formatFlag = "json"
	cmd = readyCmd()
	cmd.SetArgs(nil)
	out = captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("ready without --role: %v", runErr)
	}
	var plainReady map[string]any
	if err := json.Unmarshal([]byte(out), &plainReady); err != nil {
		t.Fatalf("unmarshal plain ready: %v", err)
	}
	if _, hasRole := plainReady["role"]; hasRole {
		t.Fatalf("plain ready without --role must not have role key: %s", out)
	}
	if plainReady["count"] != float64(1) {
		t.Fatalf("plain ready count = %v, want 1", plainReady["count"])
	}
}

func TestRoleParkUnparkAndClaimIgnorePark_RoundTrip(t *testing.T) {
	prevFormat := formatFlag
	t.Cleanup(func() { formatFlag = prevFormat })

	st := storetest.NewSQLite(t)
	srv := server.New(st)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	owner, err := st.CreateUser(models.UserCreate{
		Email: "cli-park@example.com", Name: "Alice Owner", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := st.CreateWorkspace(models.WorkspaceCreate{Name: "CLIParkWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := st.CreateAPIToken(owner.ID, models.APITokenCreate{Name: "cli-pat", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	schema := `{"fields":[{"key":"status","type":"select","options":["open","in_progress","done"],"terminal_options":["done"],"default":"open"}]}`
	coll, err := st.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: schema,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	role, err := st.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Implementer", Slug: "implementer"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}
	var items []*models.Item
	for i, title := range []string{"First", "Second"} {
		it, err := st.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title: title, Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		if err := st.UpdateRoleSortOrder(ws.ID, []store.RoleSortUpdate{{ItemID: it.ID, RoleSortOrder: (i + 1) * 1024}}); err != nil {
			t.Fatalf("UpdateRoleSortOrder: %v", err)
		}
		it, _ = st.GetItem(it.ID)
		items = append(items, it)
	}

	setTempHomeMain(t)
	t.Setenv("PAD_URL", ts.URL)
	t.Setenv("PAD_TOKEN", tok.Token)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".pad.toml"), []byte("workspace = \""+ws.Slug+"\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. pad role park implementer --before TASK-2 --format json
	formatFlag = "json"
	cmd := roleCmd()
	cmd.SetArgs([]string{"park", "implementer", "--before", items[1].Ref})
	var runErr error
	out := captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("pad role park: %v", runErr)
	}
	var parkResp struct {
		Park struct {
			Mode           string  `json:"mode"`
			FirstParkedRef *string `json:"first_parked_ref"`
			ParkSeq        int     `json:"park_seq"`
		} `json:"park"`
	}
	if err := json.Unmarshal([]byte(out), &parkResp); err != nil {
		t.Fatalf("decode park output: %v\n%s", err, out)
	}
	if parkResp.Park.Mode != "line" || parkResp.Park.FirstParkedRef == nil || *parkResp.Park.FirstParkedRef != items[1].Ref || parkResp.Park.ParkSeq != 1 {
		t.Fatalf("unexpected park response: %+v", parkResp.Park)
	}

	// 2. Plain claim on parked TASK-2 fails with structured APIError code item_parked.
	formatFlag = "table"
	claim := itemClaimCmd()
	claim.SetArgs([]string{items[1].Ref, "--holder", "agent-a"})
	claim.SilenceUsage = true
	claim.SilenceErrors = true
	captureStdout(t, func() { runErr = claim.Execute() })
	if runErr == nil {
		t.Fatal("plain claim on parked item must fail")
	}
	var apiErr *cli.APIError
	if !errors.As(runErr, &apiErr) || apiErr.Code != "item_parked" {
		t.Fatalf("plain claim error = %v, want *cli.APIError with code item_parked", runErr)
	}
	// Verify root structured error writer emits the marker with code item_parked.
	var errBuf bytes.Buffer
	writeRootStructuredError(&errBuf, runErr)
	if !strings.Contains(errBuf.String(), cli.StructuredErrorMarker) || !strings.Contains(errBuf.String(), `"item_parked"`) {
		t.Fatalf("writeRootStructuredError did not emit item_parked marker: %q", errBuf.String())
	}

	// 3. Claim with --ignore-park succeeds.
	claim = itemClaimCmd()
	claim.SetArgs([]string{items[1].Ref, "--holder", "agent-a", "--ignore-park"})
	out = captureStdout(t, func() { runErr = claim.Execute() })
	if runErr != nil {
		t.Fatalf("claim --ignore-park: %v", runErr)
	}
	if !strings.Contains(out, "Lease acquired") {
		t.Errorf("unexpected claim output: %s", out)
	}

	// 4. Stale expected_park_seq fails with park_conflict.
	cmd = roleCmd()
	cmd.SetArgs([]string{"unpark", "implementer", "--expected-park-seq", "99"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr == nil || !errors.As(runErr, &apiErr) || apiErr.Code != "park_conflict" {
		t.Fatalf("stale unpark error = %v, want park_conflict", runErr)
	}

	// 5. Unpark with current expected_park_seq=1 succeeds.
	formatFlag = "json"
	cmd = roleCmd()
	cmd.SetArgs([]string{"unpark", "implementer", "--expected-park-seq", "1"})
	out = captureStdout(t, func() { runErr = cmd.Execute() })
	if runErr != nil {
		t.Fatalf("pad role unpark: %v", runErr)
	}
	var unparkResp map[string]any
	if err := json.Unmarshal([]byte(out), &unparkResp); err != nil {
		t.Fatalf("decode unpark output: %v\n%s", err, out)
	}
	if unparkResp["park"] != nil || unparkResp["park_seq"] != float64(2) {
		t.Fatalf("unexpected unpark response: %v", unparkResp)
	}
}
