package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// BUG-4082: pad_item patch's edits[].line_hint gets one rule on both
// transports. Remote's mapItemPatch had it inline (BUG-4065); stdio forwarded
// edits to the CLI unread, so a string hint was refused there where remote
// coerced it, and 2^53 or more reached bodypatch as a line number. Both now
// call editsWithLineHints. Lead ruling 86931478: C1, one table drives both
// transports, so a change to the helper turns both red and stdio == remote is
// pinned, not asserted; C2, neither transport modifies the caller's map.

// bug4082Cases is the shared table. edits is the JSON the caller sends; want
// is the edits JSON each transport must forward (after a decode and re-encode,
// so key order and number spelling do not matter); refuse, when set, is the
// refusal both must give instead, with nothing forwarded.
var bug4082Cases = []struct {
	name, edits, want, refuse string
}{
	{"string hint coerced", `[{"old":"a","new":"b","line_hint":"5"}]`, `[{"line_hint":5,"new":"b","old":"a"}]`, ""},
	{"number hint kept", `[{"old":"a","new":"b","line_hint":5}]`, `[{"line_hint":5,"new":"b","old":"a"}]`, ""},
	{"null hint absent", `[{"old":"a","new":"b","line_hint":null}]`, `[{"line_hint":null,"new":"b","old":"a"}]`, ""},
	{"no hint", `[{"old":"a","new":"b"}]`, `[{"new":"b","old":"a"}]`, ""},
	{"largest safe hint", `[{"old":"a","new":"b","line_hint":9007199254740991}]`, `[{"line_hint":9007199254740991,"new":"b","old":"a"}]`, ""},
	{"non-object edit left alone", `["x",{"old":"a","new":"b","line_hint":"2"}]`, `["x",{"line_hint":2,"new":"b","old":"a"}]`, ""},
	{"leading zero", `[{"old":"a","new":"b","line_hint":"010"}]`, "", `line_hint must be a whole number, got the string \"010\" (in edits[0])`},
	{"fraction", `[{"old":"a","new":"b","line_hint":1.5}]`, "", "line_hint must be a whole number, got 1.5 (in edits[0])"},
	{"2^53", `[{"old":"a","new":"b","line_hint":9007199254740992}]`, "", "line_hint is beyond the exact integer range of a JSON number, got 9.007199254740992e+15 (in edits[0])"},
	{"bool", `[{"old":"a","new":"b","line_hint":true}]`, "", "line_hint must be a number, got a boolean (in edits[0])"},
	{"object", `[{"old":"a","new":"b","line_hint":{}}]`, "", "line_hint must be a number, got an object (in edits[0])"},
	{"second edit names its index", `[{"old":"a","new":"b","line_hint":1},{"old":"c","new":"d","line_hint":"0x10"}]`, "", "(in edits[1])"},
}

func canonicalJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func bug4082Input(t *testing.T, edits string) map[string]any {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal([]byte(`{"workspace":"docapp","ref":"TASK-1","edits":`+edits+`}`), &in); err != nil {
		t.Fatal(err)
	}
	return in
}

// remoteLineHints runs the remote mapper and returns the forwarded edits JSON
// or the refusal text.
func remoteLineHints(t *testing.T, edits string) (forwarded, refusal string) {
	t.Helper()
	_, _, body, err := mapItemPatch(bug4082Input(t, edits))
	if err != nil {
		return "", err.Error()
	}
	var b map[string]json.RawMessage
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("remote body %s: %v", body, err)
	}
	return canonicalJSON(t, string(b["edits"])), ""
}

// stdioLineHints runs the stdio fan-out handler and returns the --edits JSON
// forwarded to the CLI or the refusal text.
func stdioLineHints(t *testing.T, edits string) (forwarded, refusal string) {
	t.Helper()
	in := bug4082Input(t, edits)
	in["action"] = "patch"
	d := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: d}
	res, err := makeFanOutHandler(padItemTool, env)(context.Background(), callToolRequest(in))
	if err != nil {
		t.Fatal(err)
	}
	if res != nil && res.IsError {
		if d.gotPath != nil {
			t.Fatalf("refused but dispatched: %v", d.gotArgs)
		}
		return "", textOf(res)
	}
	for i := 0; i+1 < len(d.gotArgs); i++ {
		if d.gotArgs[i] == "--edits" {
			return canonicalJSON(t, d.gotArgs[i+1]), ""
		}
	}
	t.Fatalf("no --edits forwarded: %v", d.gotArgs)
	return "", ""
}

func TestBUG4082_LineHintSameOnBothTransports(t *testing.T) {
	for _, tc := range bug4082Cases {
		t.Run(tc.name, func(t *testing.T) {
			rFwd, rRef := remoteLineHints(t, tc.edits)
			sFwd, sRef := stdioLineHints(t, tc.edits)
			if tc.refuse != "" {
				// The JSON envelope escapes quotes, so compare stdio against the
				// escaped form of remote's error text.
				escaped, _ := json.Marshal(rRef)
				if !strings.Contains(rRef, strings.ReplaceAll(tc.refuse, `\"`, `"`)) {
					t.Errorf("remote: want refusal containing %q, got %q (forwarded %s)", tc.refuse, rRef, rFwd)
				}
				if !strings.Contains(sRef, string(ErrValidationFailed)) ||
					!strings.Contains(sRef, "item patch: "+strings.Trim(string(escaped), `"`)) {
					t.Errorf("stdio: want validation_failed with remote's message %q, got %q (forwarded %s)", rRef, sRef, sFwd)
				}
				// Checked on its own too, so a helper change that alters both
				// messages alike still turns stdio red (C1). tc.refuse is written
				// in the envelope's escaped form.
				if !strings.Contains(sRef, tc.refuse) {
					t.Errorf("stdio: want refusal containing %q, got %q", tc.refuse, sRef)
				}
				return
			}
			want := canonicalJSON(t, tc.want)
			if rRef != "" || rFwd != want {
				t.Errorf("remote: want edits %s, got %s (refusal %q)", want, rFwd, rRef)
			}
			if sRef != "" || sFwd != want {
				t.Errorf("stdio: want edits %s, got %s (refusal %q)", want, sFwd, sRef)
			}
		})
	}
}

// C2: neither transport modifies the caller's map, its edits slice or an
// edit in it, on a coerced hint or on a refusal.
func TestBUG4082_CallerMapUnmodified(t *testing.T) {
	for _, edits := range []string{
		`[{"old":"a","new":"b","line_hint":"5"},"x"]`,
		`[{"old":"a","new":"b","line_hint":2},{"old":"c","new":"d","line_hint":"010"}]`,
	} {
		t.Run("remote "+edits, func(t *testing.T) {
			in, before := bug4082Input(t, edits), bug4082Input(t, edits)
			_, _, _, _ = mapItemPatch(in)
			if !reflect.DeepEqual(in, before) {
				t.Fatalf("caller's map modified: %v, was %v", in, before)
			}
		})
		t.Run("stdio "+edits, func(t *testing.T) {
			in, before := bug4082Input(t, edits), bug4082Input(t, edits)
			in["action"], before["action"] = "patch", "patch"
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: &fakeDispatcher{}}
			req := callToolRequest(in)
			if _, err := makeFanOutHandler(padItemTool, env)(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(in, before) {
				t.Fatalf("caller's map modified: %v, was %v", in, before)
			}
		})
		// The fan-out handler hands actions a private copy, so the row above
		// cannot see actionItemPatch touch its own input. Call it directly.
		t.Run("stdio action "+edits, func(t *testing.T) {
			in, before := bug4082Input(t, edits), bug4082Input(t, edits)
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: &fakeDispatcher{}}
			if _, err := actionItemPatch(context.Background(), in, env); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(in, before) {
				t.Fatalf("actionItemPatch modified its input: %v, was %v", in, before)
			}
		})
	}
}
