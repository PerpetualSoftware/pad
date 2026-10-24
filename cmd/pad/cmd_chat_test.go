package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/spf13/cobra"
)

func TestChatShowReadMarker(t *testing.T) {
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })

	for _, tc := range []struct {
		name, page string
		mark       string
		flags      []string
		want       []int64
	}{
		{"returned max", `{"messages":[{"seq":7,"body":"seven"},{"seq":4,"deleted":true}],"read_seq":3,"last_seq":99,"newest_seq":98}`, `{"marked":true,"read_seq":7}`, nil, []int64{7}},
		{"distance two leaves one unseen", `{"messages":[{"seq":12},{"seq":13}],"read_seq":10}`, `{"marked":false,"read_seq":10,"reason":"gap","first_unshown":11,"last_unshown":11}`, nil, nil},
		{"distance one is contiguous", `{"messages":[{"seq":11},{"seq":12}],"read_seq":10}`, `{"marked":true,"read_seq":12}`, nil, []int64{12}},
		{"no mark", `{"messages":[{"seq":7}],"last_seq":99}`, "", []string{"--no-mark"}, nil},
		{"empty", `{"messages":[],"last_seq":99}`, `{"marked":false,"read_seq":0,"reason":"empty"}`, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var marks []int64
			ts := setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/me/conversations/12/messages":
					if r.URL.Query().Get("after_seq") != "2" || r.URL.Query().Get("limit") != "3" {
						t.Errorf("query: %s", r.URL.RawQuery)
					}
					fmt.Fprint(w, tc.page)
				case "/api/v1/me/conversations/12/read":
					var in struct{ Seq int64 }
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						t.Error(err)
					}
					marks = append(marks, in.Seq)
					fmt.Fprintf(w, `{"read_seq":%d}`, in.Seq)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			var out bytes.Buffer
			cmd := newRootCmd()
			cmd.SetOut(&out)
			cmd.SetArgs(append([]string{"--url", ts.URL, "chat", "show", "C12", "--since", "2", "--limit", "3", "--format", "json"}, tc.flags...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			assertChatJSONMark(t, out.String(), tc.mark)
			if fmt.Sprint(marks) != fmt.Sprint(tc.want) {
				t.Fatalf("marks=%v want=%v", marks, tc.want)
			}
		})
	}
}

func TestChatSendAndTargetResolution(t *testing.T) {
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })

	for _, tc := range []struct {
		name     string
		args     []string
		wantMark int64
		ensure   bool
		notFound bool
	}{
		{"reply", []string{"send", "12", "hello", "--reply-to", "4", "--op-key", "my-key", "--context-workspace", "demo"}, 4, false, false},
		{"plain", []string{"send", "C12", "hello"}, 0, false, false},
		{"ensure unknown", []string{"send", "@rook", "-"}, 0, true, false},
		{"show unknown", []string{"show", "@rook"}, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			var marks []int64
			ts := setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/me/conversations":
					fmt.Fprint(w, `{"conversations":[],"unread_total":0}`)
				case "POST /api/v1/me/conversations":
					var in map[string]any
					_ = json.NewDecoder(r.Body).Decode(&in)
					if in["agent_key"] != "rook" {
						t.Errorf("ensure: %v", in)
					}
					fmt.Fprint(w, `{"conversation":{"number":12},"created":true}`)
				case "POST /api/v1/me/conversations/12/messages":
					var in map[string]any
					_ = json.NewDecoder(r.Body).Decode(&in)
					if in["body"] != "hello" {
						t.Errorf("body: %v", in)
					}
					if tc.wantMark != 0 {
						if in["reply_to_seq"] != float64(4) || in["op_key"] != "my-key" || in["context_workspace"] != "demo" {
							t.Errorf("send: %v", in)
						}
					} else if _, ok := in["op_key"]; ok {
						t.Error("unsolicited op_key")
					}
					fmt.Fprint(w, `{"message":{"seq":100,"body":"hello"},"replayed":false}`)
				case "POST /api/v1/me/conversations/12/read":
					var in struct{ Seq int64 }
					_ = json.NewDecoder(r.Body).Decode(&in)
					marks = append(marks, in.Seq)
					fmt.Fprintf(w, `{"read_seq":%d}`, in.Seq)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			var out bytes.Buffer
			cmd := newRootCmd()
			cmd.SetOut(&out)
			cmd.SetIn(bytes.NewBufferString("hello"))
			cmd.SetArgs(append(append([]string{"--url", ts.URL, "chat"}, tc.args...), "--format", "json"))
			err := cmd.Execute()
			if tc.notFound {
				if err == nil || !strings.Contains(err.Error(), "not found") {
					t.Fatalf("want not found: %v", err)
				}
				if len(calls) != 1 || calls[0] != "GET /api/v1/me/conversations" {
					t.Fatalf("calls: %v", calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantMark := ""
			if tc.wantMark != 0 {
				wantMark = fmt.Sprintf(`{"marked":true,"read_seq":%d}`, tc.wantMark)
			}
			assertChatJSONMark(t, out.String(), wantMark)
			if tc.wantMark == 0 && len(marks) != 0 || tc.wantMark != 0 && (len(marks) != 1 || marks[0] != tc.wantMark) {
				t.Fatalf("marks: %v", marks)
			}
			if tc.ensure && strings.Join(calls, ",") != "GET /api/v1/me/conversations,POST /api/v1/me/conversations,POST /api/v1/me/conversations/12/messages" {
				t.Fatalf("ensure order: %v", calls)
			}
		})
	}
}

func TestChatHumanTombstonesAndQuotes(t *testing.T) {
	old := formatFlag
	formatFlag = "table"
	t.Cleanup(func() { formatFlag = old })
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	raw := json.RawMessage(`{"messages":[
		{"seq":3,"deleted":true,"body":"hidden deleted body"},
		{"seq":4,"author_agent_key":"rook","body":"reply body","reply_to":{"seq":2,"excerpt":"short quote","body":"hidden full quote"}}
	]}`)
	if err := printChatResponse(cmd, "show", raw); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"#3 human: Message deleted", "#4 @rook: reply body", "> #2: short quote"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "hidden") {
		t.Fatalf("leaked hidden body: %s", got)
	}
}

func TestChatSendWorkspaceContext(t *testing.T) {
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })

	for _, tc := range []struct{ name, link, explicit, want string }{
		{"linked cwd", "linked", "", "linked"},
		{"explicit overrides link", "linked", "explicit", "explicit"},
		{"no link", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if tc.link != "" {
				if err := cli.WriteWorkspaceLink(dir, tc.link, ""); err != nil {
					t.Fatal(err)
				}
			}
			ts := setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/api/v1/me/conversations/12/messages" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				if tc.want == "" {
					if _, ok := in["context_workspace"]; ok {
						t.Errorf("context should be omitted: %v", in)
					}
				} else if in["context_workspace"] != tc.want {
					t.Errorf("context: %v want=%q", in, tc.want)
				}
				fmt.Fprint(w, `{"message":{"seq":1}}`)
			}))
			cmd := newRootCmd()
			args := []string{"--url", ts.URL, "chat", "send", "12", "hello", "--format", "json"}
			if tc.explicit != "" {
				args = append(args, "--context-workspace", tc.explicit)
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChatShowContiguousPage(t *testing.T) {
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })
	for _, first := range []int{31, 1} {
		t.Run(fmt.Sprint(first), func(t *testing.T) {
			var marks []int64
			ts := setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/read") {
					var in struct{ Seq int64 }
					_ = json.NewDecoder(r.Body).Decode(&in)
					marks = append(marks, in.Seq)
					fmt.Fprintf(w, `{"read_seq":%d}`, in.Seq)
					return
				}
				var messages []map[string]any
				for seq := first; seq < first+50; seq++ {
					messages = append(messages, map[string]any{"seq": seq, "body": "hello"})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"messages": messages, "read_seq": 0})
			}))
			var out, stderr bytes.Buffer
			cmd := newRootCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"--url", ts.URL, "chat", "show", "C12", "--format", "json"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), `"messages"`) {
				t.Fatalf("missing page: %s", out.String())
			}
			if first == 31 {
				if len(marks) != 0 {
					t.Errorf("marked across gap: %v", marks)
				}
				if !strings.Contains(stderr.String(), "Not marked read: messages #1-#30 were not shown; run pad chat show C12 --since 0") {
					t.Errorf("missing gap note: %s", stderr.String())
				}
			} else if len(marks) != 1 || marks[0] != 50 {
				t.Errorf("marks=%v, want [50]", marks)
			}
		})
	}
}

func TestChatMarkFailurePreservesOutcome(t *testing.T) {
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })
	for _, tc := range []struct{ verb, format string }{
		{"send", "json"}, {"show", "json"}, {"send", "table"}, {"show", "table"},
	} {
		verb := tc.verb
		t.Run(verb+"/"+tc.format, func(t *testing.T) {
			readCalls := 0
			ts := setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/read") {
					readCalls++
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"error":{"code":"forbidden","message":"Read-only token"}}`)
					return
				}
				if verb == "send" {
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"message":{"seq":2,"body":"stored"}}`)
				} else {
					fmt.Fprint(w, `{"messages":[{"seq":1,"body":"stored"}],"read_seq":0}`)
				}
			}))
			var out, stderr bytes.Buffer
			cmd := newRootCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			args := []string{"--url", ts.URL, "chat", verb, "C12"}
			if verb == "send" {
				args = append(args, "hello", "--reply-to", "1", "--context-workspace", "")
			}
			cmd.SetArgs(append(args, "--format", tc.format))
			if err := cmd.Execute(); err != nil {
				t.Errorf("successful %s failed: %v", verb, err)
			}
			if tc.format == "json" {
				assertChatJSONMark(t, out.String(), `{"marked":false,"error":"Read-only token"}`)
			}
			wantOutput := "stored"
			if tc.format == "table" && verb == "send" {
				wantOutput = "Sent #2"
			}
			if readCalls != 1 || !strings.Contains(out.String(), wantOutput) || !strings.Contains(stderr.String(), "Warning:") || !strings.Contains(stderr.String(), "Read-only token") {
				t.Fatalf("calls=%d out=%s stderr=%s", readCalls, out.String(), stderr.String())
			}
		})
	}
}

func TestChatLimitValidation(t *testing.T) {
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })
	for _, limit := range []string{"-1", "0", "201"} {
		t.Run(limit, func(t *testing.T) {
			ts := setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("invalid limit reached server")
				fmt.Fprint(w, `{}`)
			}))
			cmd := newRootCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--url", ts.URL, "chat", "show", "@rook", "--limit", limit})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "limit must be between 1 and 200") {
				t.Fatalf("limit %s: %v", limit, err)
			}
		})
	}
}

// Check the additive mark object and ensure stdout remains one JSON response.
func assertChatJSONMark(t *testing.T, output, want string) {
	t.Helper()
	var response map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("invalid JSON output: %v: %s", err, output)
	}
	raw, exists := response["mark"]
	if want == "" {
		if exists {
			t.Fatalf("unexpected mark: %s", raw)
		}
		return
	}
	var gotMark, wantMark any
	if err := json.Unmarshal(raw, &gotMark); err != nil {
		t.Fatalf("missing or invalid mark: %v: %s", err, output)
	}
	if err := json.Unmarshal([]byte(want), &wantMark); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMark, wantMark) {
		t.Fatalf("mark=%s want=%s", raw, want)
	}
}
