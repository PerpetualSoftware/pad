package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestTASK4412_RenderElapsedOffsets(t *testing.T) {
	for _, tc := range []struct {
		seconds int64
		want    string
	}{
		{1800, "30m"},
		{600, "10m"},
		{4200, "1h10m"},
		{-43200, "-12h"},
		{90, "1m30s"},
		{108000, "30h"}, // --offset accepts 1d6h; existing output uses hours.
		{0, "0s"},
		{-1800, "-30m"},
		{9460800000, "9460800000s elapsed"}, // Beyond time.Duration's range.
	} {
		t.Run(tc.want, func(t *testing.T) {
			link := displayCommitmentLink{ScheduleLink: models.ScheduleLink{
				ID: "l1", ParentItemID: "p", ChildItemID: "c", ParentRole: "target", ChildRole: "earliest",
				Offset: models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: tc.seconds}, CancelPolicy: "waiting_on",
			}}
			var out strings.Builder
			renderCommitmentLink(&out, link)
			want := "Link l1: target -> earliest: p -> c; " + tc.want + "; waiting_on\n"
			if out.String() != want {
				t.Errorf("got %q; want %q", out.String(), want)
			}
		})
	}
}

const linkPlan = `{"link":null,"moved":{"revision":"rev-1","changes":[{"item_id":"child","role":"target","from":"2099-01-01T09:00:00Z","to":"2099-01-02T09:00:00Z"}],"violations":[{"item_id":"child","role":"target","to":"2099-01-02T09:00:00Z"}],"left_in_place":[{"item_id":"finished","role":"target","to":"2099-01-02T09:00:00Z"}],"renegotiates_deadline":true},"future_member":7}`

func TestTASK4412_LinkBodies(t *testing.T) {
	base := []string{"remind", "link", "add", "TASK-5", "TASK-6", "--parent-role", "target", "--child-role", "earliest"}
	for _, tc := range []struct {
		name   string
		flags  []string
		offset string
		apply  bool
	}{
		{"negative elapsed", []string{"--offset", "-12h"}, `{"mode":"elapsed","elapsed_secs":-43200}`, false},
		{"days elapsed", []string{"--offset", "1d6h"}, `{"mode":"elapsed","elapsed_secs":108000}`, false},
		{"fractional days", []string{"--offset", ".5d"}, `{"mode":"elapsed","elapsed_secs":43200}`, false},
		{"decimal days", []string{"--offset", "0.7d"}, `{"mode":"elapsed","elapsed_secs":60480}`, false},
		{"signed days", []string{"--offset", "-1d6h"}, `{"mode":"elapsed","elapsed_secs":-108000}`, false},
		{"civil", []string{"--days", "-1", "--clock", "19:00", "--zone", "America/New_York"}, `{"mode":"civil","days":-1,"clock_secs":68400,"all_day":false,"zone":"America/New_York"}`, false},
		{"apply", []string{"--offset", "0s", "--apply", "--revision", "X", "--ack-violations"}, `{"mode":"elapsed","elapsed_secs":0}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err, calls := runSched(t, okReply(linkPlan), "", append(append([]string{}, base...), tc.flags...)...)
			if err != nil || len(calls) != 1 {
				t.Fatalf("err=%v calls=%+v", err, calls)
			}
			if calls[0].method != "POST" || calls[0].path != schedPath+"/links" {
				t.Fatalf("calls=%+v", calls)
			}
			var got, want map[string]any
			_ = json.Unmarshal([]byte(calls[0].body), &got)
			_ = json.Unmarshal([]byte(`{"child":"TASK-6","parent_role":"target","child_role":"earliest","offset":`+tc.offset+`}`), &want)
			if tc.apply {
				want["revision"] = "X"
				want["acknowledge_violations"] = true
			} else {
				want["preview"] = true
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%v want=%v", got, want)
			}
			if !tc.apply {
				for _, s := range []string{"child", "target", "->", "Violation", "Left in place", "deadline", "rev-1", "--apply --revision rev-1"} {
					if !strings.Contains(out, s) {
						t.Errorf("output lacks %q: %s", s, out)
					}
				}
			}
		})
	}
}

func TestTASK4412_UsageBeforeRequests(t *testing.T) {
	for _, tc := range []struct {
		args []string
		hint string
	}{
		{[]string{"link", "add", "TASK-5", "TASK-6", "--parent-role", "target", "--child-role", "target", "--offset", "1h", "--days", "0"}, "offset"},
		{[]string{"link", "edit", "id"}, "offset"},
		{[]string{"link", "add", "TASK-5", "TASK-6", "--parent-role", "target", "--child-role", "target", "--days", "0", "--zone", "Local"}, "zone"},
		{[]string{"move", "TASK-5", "--role", "target", "--until", "2099-01-01T09:00:00Z", "--to", "2099-01-02T09:00:00Z"}, "until"},
		{[]string{"move", "TASK-5", "--role", "target"}, "to"},
		{[]string{"move", "TASK-5", "--role", "target", "--to", "2099-01-01T09:00:00Z", "--occurrence", "2099-01-01T09:00:00Z"}, "occurrence"},
	} {
		_, _, err, calls := runSched(t, okReply(`{}`), "", append([]string{"remind"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.hint) || len(calls) != 0 {
			t.Fatalf("%v: err=%v calls=%+v", tc.args, err, calls)
		}
	}
}

func TestTASK4412_RoutesAndJSON(t *testing.T) {
	for _, tc := range []struct {
		args               []string
		method, path, body string
	}{
		{[]string{"link", "list", "TASK-5"}, "GET", schedPath + "/links", ""},
		{[]string{"link", "edit", "lid", "--cancel-policy", "waiting_on"}, "PATCH", "/workspaces/test/commitment-links/lid", `{"cancel_policy":"waiting_on","preview":true}`},
		{[]string{"link", "rm", "lid"}, "DELETE", "/workspaces/test/commitment-links/lid", ""},
		{[]string{"gate", "add", "TASK-5", "TASK-6"}, "POST", schedPath + "/gates", `{"blocker":"TASK-6"}`},
		{[]string{"gate", "list", "TASK-5"}, "GET", schedPath + "/gates", ""},
		{[]string{"gate", "rm", "gid"}, "DELETE", "/workspaces/test/commitment-gates/gid", ""},
		{[]string{"move", "TASK-5", "--role", "target", "--to", "2099-01-02T09:00:00Z"}, "POST", schedPath + "/move-preview", `{"role":"target","to":"2099-01-02T09:00:00Z"}`},
		{[]string{"move", "TASK-5", "--role", "target", "--to", "2099-01-02T09:00:00Z", "--apply", "--revision", "X", "--ack-violations", "--reason", "why"}, "POST", schedPath + "/move", `{"role":"target","to":"2099-01-02T09:00:00Z","revision":"X","acknowledge_violations":true,"reason":"why"}`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			reply := linkPlan
			status := 200
			if tc.method == "DELETE" {
				reply = ""
				status = 204
			}
			out, _, err, calls := runSched(t, func(recordedCall) (int, string) { return status, reply }, "", append([]string{"--format", "json", "item", "remind"}, tc.args...)...)
			if err != nil || len(calls) != 1 {
				t.Fatalf("err=%v calls=%+v", err, calls)
			}
			if calls[0].method != tc.method || calls[0].path != tc.path {
				t.Fatalf("calls=%+v", calls)
			}
			var got, want any
			_ = json.Unmarshal([]byte(calls[0].body), &got)
			_ = json.Unmarshal([]byte(tc.body), &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("body=%s want=%s", calls[0].body, tc.body)
			}
			if tc.method != "DELETE" {
				_ = json.Unmarshal([]byte(out), &got)
				_ = json.Unmarshal([]byte(reply), &want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("JSON changed: %s", out)
				}
			} else if strings.TrimSpace(out) != "" {
				t.Fatalf("204 JSON: %q", out)
			}
		})
	}
}

func TestTASK4412_ResolvePreviewHint(t *testing.T) {
	for _, verb := range []string{"move", "done"} {
		args := []string{"remind", verb, "TASK-5", "--occurrence", "2099-01-01T09:00:00Z"}
		if verb == "move" {
			args = append(args, "--until", "2099-01-02T09:00:00Z")
		}
		_, _, err, _ := runSched(t, apiError(409, "commitment_preview_required", "Review linked dates first", `{"preview":"route"}`), "", args...)
		if err == nil || !strings.Contains(err.Error(), "Review linked dates first") || !strings.Contains(err.Error(), "pad remind move TASK-5 --role") || !strings.Contains(err.Error(), "--to") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestTASK4412_ShowGraph(t *testing.T) {
	body := strings.TrimSuffix(weekdaysView, "}") + `,"links":[{"id":"l1","parent_item_id":"p","parent_role":"target","child_item_id":"c","child_role":"earliest","offset":{"mode":"elapsed","elapsed_secs":-43200},"cancel_policy":"waiting_on"}],"gates":{"state":"waiting","gates":[{"id":"g1","blocker_item_id":"blocker","outcome":"pending"}]}}`
	out, _, err, _ := runSched(t, okReply(body), "", "remind", "show", "TASK-5")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"target -> earliest", "waiting_on", "-12h", "blocker", "waiting", "pending"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q: %s", s, out)
		}
	}
}

func TestTASK4412_CivilZoneDefault(t *testing.T) {
	args := []string{"remind", "link", "add", "TASK-5", "TASK-6", "--parent-role", "target", "--child-role", "target", "--days", "0"}
	restore := linkAccountZone
	t.Cleanup(func() { linkAccountZone = restore })

	linkAccountZone = func() string { return "Europe/Paris" }
	_, _, err, calls := runSched(t, okReply(linkPlan), "", args...)
	if err != nil || len(calls) != 1 {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
	var got struct {
		Offset map[string]any `json:"offset"`
	}
	_ = json.Unmarshal([]byte(calls[0].body), &got)
	if got.Offset["zone"] != "Europe/Paris" {
		t.Fatalf("zone=%v, want the account time zone", got.Offset["zone"])
	}

	linkAccountZone = func() string { return "" }
	_, _, err, calls = runSched(t, okReply(linkPlan), "", args...)
	if err == nil || !strings.Contains(err.Error(), "--zone") || !strings.Contains(err.Error(), "pad auth timezone") || len(calls) != 0 {
		t.Fatalf("no account zone: err=%v calls=%+v", err, calls)
	}
}
