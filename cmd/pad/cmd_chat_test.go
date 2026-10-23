package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/spf13/cobra"
)

func TestChatShowReadMarker(t *testing.T) {
	for _, tc := range []struct {
		name, page string
		flags      []string
		want       []int64
	}{
		{"returned max", `{"messages":[{"seq":7,"body":"seven"},{"seq":4,"deleted":true}],"last_seq":99,"newest_seq":98}`, nil, []int64{7}},
		{"no mark", `{"messages":[{"seq":7}],"last_seq":99}`, []string{"--no-mark"}, nil},
		{"empty", `{"messages":[],"last_seq":99}`, nil, nil},
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
			cmd := newRootCmd()
			cmd.SetArgs(append([]string{"--url", ts.URL, "chat", "show", "C12", "--since", "2", "--limit", "3", "--format", "json"}, tc.flags...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(marks) != fmt.Sprint(tc.want) {
				t.Fatalf("marks=%v want=%v", marks, tc.want)
			}
		})
	}
}

func TestChatSendAndTargetResolution(t *testing.T) {
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
			cmd := newRootCmd()
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
