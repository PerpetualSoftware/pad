package main

import (
	"strings"
	"testing"
	"time"
)

const agendaEmptyWindow = `{"from":"x","to":"y","tz":"UTC","entries":[],"window_complete":true`

func TestAgendaRecipientFlagMapsToQuery(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string // "" means no recipient is sent
	}{
		{"default", nil, "me"},
		{"me", []string{"--recipient", "me"}, "me"},
		{"all", []string{"--recipient", "all"}, ""},
		{"id", []string{"--recipient", "user-123"}, "user-123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, agendaEmptyWindow + `}` })
			args := append([]string{"item", "remind", "agenda", "--from", "2031-03-10"}, tc.args...)
			if _, _, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), args...); err != nil {
				t.Fatal(err)
			}
			q := agendaCall(t, calls())
			if q.Get("recipient") != tc.want || q.Has("recipient") != (tc.want != "") {
				t.Fatalf("query=%v; want recipient %q", q, tc.want)
			}
		})
	}
}

func TestAgendaStaleRefusesRecipient(t *testing.T) {
	h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, `{"tz":"UTC","stale":true,"entries":[]}` })
	_, _, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), "item", "remind", "agenda", "--stale", "--recipient", "user-123")
	if err == nil || !strings.Contains(err.Error(), "--stale lists your own occurrences; drop --recipient") {
		t.Fatalf("err=%v", err)
	}
	if len(calls()) != 0 {
		t.Fatalf("sent a request: %+v", calls())
	}
	// The default recipient is ignored by --stale.
	h, calls = recordingStub(func(c recordedCall) (int, string) { return 200, `{"tz":"UTC","stale":true,"entries":[]}` })
	if _, _, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), "item", "remind", "agenda", "--stale"); err != nil {
		t.Fatal(err)
	}
	if q := agendaCall(t, calls()); q.Has("recipient") {
		t.Fatalf("stale query=%v", q)
	}
}

// The stub's /auth/me zone is America/New_York, so times show in it.
func agendaGroupsOut(t *testing.T, body string, args ...string) string {
	t.Helper()
	h, _ := recordingStub(func(c recordedCall) (int, string) { return 200, body })
	out, _, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), append([]string{"item", "remind", "agenda", "--from", "2031-03-10"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAgendaRendersGroups(t *testing.T) {
	out := agendaGroupsOut(t, agendaEmptyWindow+`,
"priority_list":[
 {"type":"schedule","item_ref":"TASK-1","item_title":"Ship\n  it","collection":"tasks","rank":"high","reason":"outstanding","at":"2031-03-10T09:00:00Z","roles":{}},
 {"type":"schedule","item_ref":"TASK-2","item_title":"Day","collection":"tasks","rank":"normal","reason":"target_passed","at":"2031-03-09T00:00:00Z","all_day":true,"roles":{}},
 {"type":"item","item_ref":"TASK-3","item_title":"Ended","collection":"tasks","rank":"low","reason":"schedule_ended","schedule_id":"s1","ended_at":"2031-03-01T00:00:00Z"}],
"priority_list_complete":true,
"waiting_on":[{"type":"item","item_ref":"TASK-4","item_title":"Blocked","collection":"tasks","reason":"waiting"}],
"waiting_on_complete":false,
"no_date":[],"no_date_complete":true}`)
	for _, want := range []string{
		"Nothing on the agenda in this window.\n\nPriority (3):\n",
		"RANK", "WHEN", "REASON",
		"2031-03-10 05:00 EDT", "Ship it", "outstanding",
		"2031-03-09 all day", "target_passed",
		"\nWaiting on (1):\n", "Blocked", "waiting", "  list incomplete: more not shown\n",
		"\nNo date (0):\n  none\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The item-level schedule_ended row has an empty WHEN: rank, then the ref.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "TASK-3") {
			if f := strings.Fields(line); len(f) != 4 || f[0] != "low" || f[1] != "TASK-3" || f[3] != "schedule_ended" {
				t.Fatalf("schedule_ended row: %q", line)
			}
		}
	}
}

func TestAgendaGroupsEmptyIncompleteAndPersonalOnly(t *testing.T) {
	out := agendaGroupsOut(t, agendaEmptyWindow+`,"priority_list":[],"priority_list_complete":false,"waiting_on":[],"waiting_on_complete":true,"no_date":[],"no_date_complete":true,"no_date_scope":"personal_only"}`, "--recipient", "all")
	for _, want := range []string{
		"\nPriority (0):\n  list incomplete\n",
		"\nWaiting on (0):\n  none\n",
		"\nNo date: not listed for the whole workspace; run with --recipient me to list your undated work.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "No date (") {
		t.Errorf("listed a No date table under personal_only:\n%s", out)
	}
}

func TestAgendaResumeHintRepeatsRecipient(t *testing.T) {
	body := `{"from":"x","to":"y","tz":"UTC","entries":[{"type":"schedule","at":"2031-03-10T09:00:00Z","day":"2031-03-10","state":"scheduled","item_ref":"TASK-1","item_title":"A","roles":{}}],"truncated":true,"next_from":"2031-03-11T00:00:00Z","priority_list":[],"priority_list_complete":true,"waiting_on":[],"waiting_on_complete":true,"no_date":[],"no_date_complete":true}`
	if out := agendaGroupsOut(t, body, "--recipient", "all"); !strings.Contains(out, "--from 2031-03-11T00:00:00Z --to 2031-03-17T04:00:00Z --recipient all\n") {
		t.Fatalf("hint without --recipient all:\n%s", out)
	}
	if out := agendaGroupsOut(t, body); strings.Contains(out, "--recipient") {
		t.Fatalf("default hint repeats --recipient:\n%s", out)
	}
}
