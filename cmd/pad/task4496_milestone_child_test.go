package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const childView4496 = `{"item_ref":"TASK-5","forecast":null,"milestone":{"counted":1,"total":2,"waivers":1,"result":"pending","children":[` +
	`{"item_ref":"TASK-6","kind":"child","close_state":"open","counts":false,"waived":false,"added_by":"u1","added_at":"2026-11-11T10:00:00Z"},` +
	`{"item_ref":"TASK-7","kind":"waiver","close_state":"open","counts":true,"waived":true,"reason":"not needed","added_by":"u1","added_at":"2026-11-11T10:00:00Z"}]}}`

func TestTASK4496_ChildSelectWaiveRemove(t *testing.T) {
	cases := []struct {
		args   []string
		method string
		body   map[string]any
	}{
		{[]string{"select", "TASK-5", "TASK-6"}, "PUT", map[string]any{"kind": "child"}},
		{[]string{"waive", "TASK-5", "TASK-6", "--reason", "not needed"}, "PUT", map[string]any{"kind": "waiver", "reason": "not needed"}},
		{[]string{"remove", "TASK-5", "TASK-6"}, "DELETE", nil},
	}
	for _, tc := range cases {
		for _, prefix := range [][]string{{"remind", "child"}, {"item", "remind", "child"}} {
			out, _, err, calls := runSched(t, okReply(childView4496), "", append(prefix, tc.args...)...)
			if err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			if len(calls) != 1 || calls[0].method != tc.method || calls[0].path != schedPath+"/children/TASK-6" {
				t.Fatalf("%v calls = %+v", tc.args, calls)
			}
			var body map[string]any
			if calls[0].body != "" {
				if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
					t.Fatalf("body %q: %v", calls[0].body, err)
				}
			}
			if !reflect.DeepEqual(body, tc.body) {
				t.Fatalf("%v body = %v", tc.args, body)
			}
			for _, want := range []string{"Children:", "1 of 2 counted, 1 waivers, result pending", "TASK-6", "TASK-7", "waiver", "reason: not needed"} {
				if !strings.Contains(out, want) {
					t.Fatalf("%v output missing %q: %q", tc.args, want, out)
				}
			}
		}
	}
}

func TestTASK4496_ChildJSONIsTheView(t *testing.T) {
	out, _, err, _ := runSched(t, okReply(childView4496), "", "--format", "json", "remind", "child", "select", "TASK-5", "TASK-6")
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	_ = json.Unmarshal([]byte(childView4496), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("json = %v", got)
	}
}

func TestTASK4496_WaiveNeedsReasonBeforeAnyRequest(t *testing.T) {
	for _, extra := range [][]string{nil, {"--reason", "   "}} {
		_, _, err, calls := runSched(t, okReply(childView4496), "", append([]string{"remind", "child", "waive", "TASK-5", "TASK-6"}, extra...)...)
		if err == nil || !strings.Contains(err.Error(), "--reason") || len(calls) != 0 {
			t.Fatalf("waive %v: err=%v calls=%+v", extra, err, calls)
		}
	}
}

func TestTASK4496_ChildAgainstTheServer(t *testing.T) {
	root, _, it, kid, _, _ := forecastCLI4359(t, "")
	out, _, err := tzRun(t, root, "remind", "child", "select", it.Ref, kid.Ref)
	if err != nil || !strings.Contains(out, "0 of 1 counted, 0 waivers, result pending") || !strings.Contains(out, kid.Ref) {
		t.Fatalf("select: %q %v", out, err)
	}
	out, _, err = tzRun(t, root, "remind", "child", "waive", it.Ref, kid.Ref, "--reason", "not needed")
	if err != nil || !strings.Contains(out, "1 of 1 counted, 1 waivers, result completed") || !strings.Contains(out, "reason: not needed") {
		t.Fatalf("waive: %q %v", out, err)
	}
	out, _, err = tzRun(t, root, "remind", "child", "remove", it.Ref, kid.Ref)
	if err != nil || strings.Contains(out, kid.Ref) || !strings.Contains(out, "Children:") {
		t.Fatalf("remove: %q %v", out, err)
	}
	if _, _, err = tzRun(t, root, "remind", "child", "select", it.Ref, "TASK-999"); err == nil || !strings.Contains(err.Error(), "Item not found") {
		t.Fatalf("unknown child: %v", err)
	}
}
