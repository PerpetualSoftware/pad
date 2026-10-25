package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// BUG-4070: on stdio, a "number" param gets remote's rule (numericInput)
// before it becomes CLI text. A string used to be forwarded verbatim, so an
// int flag read "010" as 8 (pflag parses base 0) and chat delete's positional
// seq read it as 10; a float64 of 2^53 or more was formatted and forwarded,
// where remote refuses it as possibly rounded (BUG-4061 / ba981cb4f).

// stdioNumberCall drives the stdio fan-out handler for tool with args (decoded
// from JSON, as mcp-go decodes a request) and reports the result and what, if
// anything, reached the dispatcher.
func stdioNumberCall(t *testing.T, tool ToolDef, rawArgs string) (string, bool, *fakeDispatcher) {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		t.Fatal(err)
	}
	d := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: d}
	res, err := makeFanOutHandler(tool, env)(context.Background(), callToolRequest(args))
	if err != nil {
		t.Fatal(err)
	}
	return textOf(res), res != nil && res.IsError, d
}

func TestBUG4070_StdioNumberStringsAndFloats(t *testing.T) {
	// Each value is a JSON literal for the param, so a quoted one is a string
	// and a bare one a JSON number (a float64 after decoding).
	refused := []struct{ value, want string }{
		{`"010"`, "must be a whole number, got the string"},
		{`"00"`, "must be a whole number"},
		{`"-010"`, "must be a whole number"},
		{`"0x10"`, "must be a whole number"},
		{`"0b1"`, "must be a whole number"},
		{`"0o7"`, "must be a whole number"},
		{`"1_000"`, "must be a whole number"},
		{`"+5"`, "must be a whole number"},
		{`" 5"`, "must be a whole number"},
		{`"5 "`, "must be a whole number"},
		{`""`, "must be a whole number"},
		{`"1.0"`, "must be a whole number"},
		{`"1e3"`, "must be a whole number"},
		{`"9007199254740992"`, "is beyond the exact integer range"},
		{`9007199254740992`, "is beyond the exact integer range"},
		{`-9007199254740992`, "is beyond the exact integer range"},
		{`1e300`, "is out of range"},
		{`1.5`, "must be a whole number, got 1.5"},
	}
	// Three doors: a flag param (show's limit), a positional param (delete's
	// seq) and a custom handler (send's reply_to, behind its body check).
	doors := []struct {
		name, action, param, extra string
	}{
		{"flag", "show", "limit", `"target":"C1"`},
		{"positional", "delete", "seq", `"target":"C1"`},
		{"custom handler", "send", "reply_to", `"target":"C1","body":"hi"`},
	}
	for _, door := range doors {
		for _, tc := range refused {
			t.Run(door.name+"/"+tc.value, func(t *testing.T) {
				text, isErr, d := stdioNumberCall(t, padChatTool,
					`{"action":"`+door.action+`",`+door.extra+`,"`+door.param+`":`+tc.value+`}`)
				if !isErr || !strings.Contains(text, string(ErrValidationFailed)) ||
					!strings.Contains(text, door.param+" "+tc.want) {
					t.Fatalf("want validation_failed with %q, got %s", door.param+" "+tc.want, text)
				}
				if d.gotPath != nil {
					t.Fatalf("a refused value was dispatched: %v %v", d.gotPath, d.gotArgs)
				}
			})
		}
	}

	// Every number param is checked, not only the first present one: since is
	// declared before limit, and a valid since must not let limit through.
	t.Run("a valid earlier param does not end the check", func(t *testing.T) {
		text, isErr, d := stdioNumberCall(t, padChatTool, `{"action":"show","target":"C1","since":"5","limit":"010"}`)
		if !isErr || !strings.Contains(text, "limit must be a whole number") {
			t.Fatalf("want limit refused, got %s", text)
		}
		if d.gotPath != nil {
			t.Fatalf("dispatched: %v", d.gotArgs)
		}
	})

	// Other types are not this check's to refuse (r1 ruling, row 1): they keep
	// their path, and these rows pin where it goes. At a flag, encodeFlagValue
	// sends text that pflag's ParseInt(s, 0, 64) refuses; at the positional
	// seq, fmt.Sprint sends text that cmd_chat.go's ParseInt(s, 10, 64)
	// refuses. (pad_item list and history read limit through intParam, which
	// refuses them before the CLI.)
	passedThrough := []struct{ value, flagWire, positionalWire string }{
		{`true`, "true", "true"},
		{`false`, "false", "false"},
		{`null`, "", "<nil>"},
		{`[1]`, "[1]", "[1]"},
		{`{}`, "{}", "map[]"},
	}
	for _, tc := range passedThrough {
		t.Run("flag passes through "+tc.value, func(t *testing.T) {
			text, isErr, d := stdioNumberCall(t, padChatTool, `{"action":"show","target":"C1","limit":`+tc.value+`}`)
			if isErr {
				t.Fatalf("refused here, not by the CLI: %s", text)
			}
			if !containsPair(d.gotArgs, "--limit", tc.flagWire) {
				t.Fatalf("want --limit %q, got %q", tc.flagWire, d.gotArgs)
			}
		})
		t.Run("positional passes through "+tc.value, func(t *testing.T) {
			text, isErr, d := stdioNumberCall(t, padChatTool, `{"action":"delete","target":"C1","seq":`+tc.value+`}`)
			if isErr {
				t.Fatalf("refused here, not by the CLI: %s", text)
			}
			if n := len(d.gotArgs); n < 3 || d.gotArgs[n-3] != "--" || d.gotArgs[n-2] != "C1" || d.gotArgs[n-1] != tc.positionalWire {
				t.Fatalf("want positionals -- C1 %q, got %q", tc.positionalWire, d.gotArgs)
			}
		})
	}

	accepted := []struct{ value, wire string }{
		{`"10"`, "10"},
		{`"0"`, "0"},
		{`"-0"`, "-0"},
		{`"-5"`, "-5"},
		{`"9007199254740991"`, "9007199254740991"},
		{`10`, "10"},
		{`9007199254740991`, "9007199254740991"},
		{`-9007199254740991`, "-9007199254740991"},
	}
	for _, tc := range accepted {
		t.Run("flag accepts "+tc.value, func(t *testing.T) {
			text, isErr, d := stdioNumberCall(t, padChatTool, `{"action":"show","target":"C1","limit":`+tc.value+`}`)
			if isErr {
				t.Fatalf("refused: %s", text)
			}
			if !containsPair(d.gotArgs, "--limit", tc.wire) {
				t.Fatalf("want --limit %s, got %v", tc.wire, d.gotArgs)
			}
		})
		t.Run("positional accepts "+tc.value, func(t *testing.T) {
			text, isErr, d := stdioNumberCall(t, padChatTool, `{"action":"delete","target":"C1","seq":`+tc.value+`}`)
			if isErr {
				t.Fatalf("refused: %s", text)
			}
			if n := len(d.gotArgs); n < 3 || d.gotArgs[n-3] != "--" || d.gotArgs[n-2] != "C1" || d.gotArgs[n-1] != tc.wire {
				t.Fatalf("want positionals -- C1 %s, got %v", tc.wire, d.gotArgs)
			}
		})
	}
}

// TestBUG4070_EveryNumberParamIsGuarded is the population check (CONVE-35,
// lead ruling e522d1be): for every catalog tool and every param it declares
// "number", a leading-zero string and a 2^53 float are each refused naming that
// param, before any action runs or anything is dispatched. It walks Catalog,
// so a number param added later is covered without editing this test.
func TestBUG4070_EveryNumberParamIsGuarded(t *testing.T) {
	var checked []string
	for _, tool := range Catalog {
		actions := sortedActionNames(tool)
		if len(actions) == 0 {
			continue
		}
		for _, p := range tool.Schema.Params {
			if p.Type != "number" {
				continue
			}
			checked = append(checked, tool.Name+"."+p.Name)
			for _, bad := range []struct{ value, want string }{
				{`"010"`, p.Name + " must be a whole number"},
				{`9007199254740992`, p.Name + " is beyond the exact integer range"},
			} {
				text, isErr, d := stdioNumberCall(t, tool, `{"action":"`+actions[0]+`","`+p.Name+`":`+bad.value+`}`)
				if !isErr || !strings.Contains(text, bad.want) {
					t.Errorf("%s.%s=%s: want a refusal with %q, got %s", tool.Name, p.Name, bad.value, bad.want, text)
				}
				if d.gotPath != nil {
					t.Errorf("%s.%s=%s was dispatched: %v", tool.Name, p.Name, bad.value, d.gotArgs)
				}
			}
		}
	}
	// The recon counted 20 (plan a285657c). Fewer means the walk lost some.
	sort.Strings(checked)
	if len(checked) < 20 {
		t.Fatalf("checked %d number params, want at least 20: %v", len(checked), checked)
	}
}
