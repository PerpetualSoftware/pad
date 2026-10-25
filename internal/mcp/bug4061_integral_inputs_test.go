package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/mark3labs/mcp-go/mcp"
)

// BUG-4061: a number that is not a whole int64 is refused, never changed:
// int64(1.5) is 1, int64(-2.7) is -2, int64(1e19) is implementation-defined,
// and a json.Number that does not parse used to read as absent.
func TestBUG4061_NumericInputRefusesWhatItWouldChange(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    int64
		present bool
		wantErr string
	}{
		{name: "whole float64", in: float64(42), want: 42, present: true},
		{name: "negative whole float64", in: float64(-3), want: -3, present: true},
		{name: "fraction", in: 1.5, wantErr: "must be a whole number, got 1.5"},
		{name: "negative fraction", in: -2.7, wantErr: "must be a whole number, got -2.7"},
		{name: "above int64", in: 1e19, wantErr: "is out of range"},
		{name: "below int64", in: -1e19, wantErr: "is out of range"},
		{name: "2^63 is out of range", in: float64(1 << 63), wantErr: "is out of range"},
		// r3/r4 (lead ruling on Q3, corrected on r4): from 2^53 up, a float64
		// may already be rounded; 2^53 is the image of 2^53+1.
		{name: "float64 2^53-1 is accepted", in: float64(1<<53 - 1), want: 1<<53 - 1, present: true},
		{name: "float64 -(2^53-1) is accepted", in: -float64(1<<53 - 1), want: -(1<<53 - 1), present: true},
		{name: "float64 2^53 is refused", in: float64(1 << 53), wantErr: "is beyond the exact integer range of a JSON number"},
		{name: "float64 -2^53 is refused", in: -float64(1 << 53), wantErr: "is beyond the exact integer range of a JSON number"},
		{name: "float64 2^53+2 is refused", in: float64(1<<53 + 2), wantErr: "is beyond the exact integer range of a JSON number"},
		{name: "float64 -2^63 is refused", in: -float64(1 << 63), wantErr: "is beyond the exact integer range"},
		{name: "json.Number 2^53+1 is exact", in: json.Number("9007199254740993"), want: 9007199254740993, present: true},
		{name: "int", in: 7, want: 7, present: true},
		{name: "int64", in: int64(8), want: 8, present: true},
		{name: "json.Number whole", in: json.Number("42"), want: 42, present: true},
		{name: "json.Number fraction", in: json.Number("1.5"), wantErr: "got 1.5"},
		{name: "json.Number above int64", in: json.Number("1e19"), wantErr: "is out of range"},
		{name: "json.Number whole with an exponent", in: json.Number("1e6"), want: 1000000, present: true},
		{name: "json.Number not a number", in: json.Number("abc"), wantErr: "must be a whole number in range, got abc"},
		// r2: exact, not via float64, which would round these two.
		{name: "json.Number 2^53+1 with an exponent", in: json.Number("9007199254740993e0"), want: 9007199254740993, present: true},
		// r9 (h2 F1): the fraction refusal, not the "in range" one it prefixes.
		{name: "json.Number a fraction float64 rounds to 1", in: json.Number("1.0000000000000001e0"), wantErr: "must be a whole number, got 1.0000000000000001e0"},
		{name: "json.Number huge exponent", in: json.Number("1e999999999"), wantErr: "in range"},
		{name: "json.Number 1e64 is out of range", in: json.Number("1e64"), wantErr: "is out of range"},
		// r5: big.Rat syntax that is not a JSON number is refused, not parsed.
		{name: "json.Number binary prefix", in: json.Number("0b101"), wantErr: "must be a whole number in range, got 0b101"},
		{name: "json.Number octal prefix", in: json.Number("0o17"), wantErr: "must be a whole number in range, got 0o17"},
		{name: "json.Number hex prefix", in: json.Number("0x10"), wantErr: "must be a whole number in range, got 0x10"},
		{name: "json.Number underscore", in: json.Number("1_000"), wantErr: "must be a whole number in range, got 1_000"},
		{name: "json.Number fraction syntax", in: json.Number("4/2"), wantErr: "must be a whole number in range, got 4/2"},
		{name: "json.Number signed exponent", in: json.Number("12e+0"), want: 12, present: true},
		{name: "nil is absent", in: nil},
		// BUG-4065 (lead ruling af4bff40, Q1): a plain decimal string is the
		// number it spells; it used to read as absent. The full string table is
		// TestBUG4065_NumericInputAdmitsOrRefusesEveryType.
		{name: "a plain decimal string is its number (BUG-4065)", in: "10", want: 10, present: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := numericInput(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("numericInput(%#v) = %d, %v, %v; want a refusal containing %q", tc.in, got, present, err, tc.wantErr)
				}
				if present {
					t.Fatal("a refused value reported present")
				}
				return
			}
			if err != nil || got != tc.want || present != tc.present {
				t.Fatalf("numericInput(%#v) = %d, %v, %v; want %d, %v, nil", tc.in, got, present, err, tc.want, tc.present)
			}
		})
	}
	if _, _, err := intParam(map[string]any{"limit": 1.5}, "limit"); err == nil || err.Error() != "limit must be a whole number, got 1.5" {
		t.Fatalf("intParam does not name the parameter: %v", err)
	}
}

// BUG-4061 through the remote dispatcher: every door that reads a numeric
// parameter refuses a fraction with a validation error naming it, and the
// request never reaches the handler. One row per (door, parameter), covering
// both refusal paths: a route mapper (validationFailedResult in Dispatch) and
// a special dispatch function (its own validationFailedResult).
func TestBUG4061_EveryDispatchDoorRefusesAFractionNamingIt(t *testing.T) {
	cases := []struct {
		cmd   []string
		input map[string]any
		param string
		// readsFirst: the door reads the item before it validates, so a GET
		// may reach the handler; no write may.
		readsFirst bool
	}{
		{[]string{"attachment", "list"}, map[string]any{"limit": 1.5}, "limit", false},
		{[]string{"attachment", "list"}, map[string]any{"offset": 2.5}, "offset", false},
		{[]string{"item", "history"}, map[string]any{"ref": "TASK-1", "limit": 1.5}, "limit", false},
		{[]string{"item", "starred"}, map[string]any{"limit": 1.5}, "limit", false},
		{[]string{"workspace", "audit-log"}, map[string]any{"days": 1.5}, "days", false},
		{[]string{"workspace", "audit-log"}, map[string]any{"limit": 1e19}, "limit", false},
		{[]string{"item", "list"}, map[string]any{"limit": -2.7}, "limit", false},
		{[]string{"item", "list"}, map[string]any{"offset": 1.5}, "offset", false},
		// The four buildQuery callers (lead ruling on Q2): the generic route
		// mapper, mapItemSearch, and the standup and changelog dispatchers.
		{[]string{"item", "backlinks"}, map[string]any{"ref": "TASK-1", "limit": 1.5}, "limit", false},
		{[]string{"item", "search"}, map[string]any{"query": "x", "offset": 2.5}, "offset", false},
		{[]string{"project", "standup"}, map[string]any{"days": 0.5}, "days", false},
		{[]string{"project", "changelog"}, map[string]any{"days": 1e19}, "days", false},
		// r2: expected_seq used to come back as a raw error, not an envelope.
		{[]string{"item", "update"}, map[string]any{"ref": "TASK-1", "content": "x", "expected_seq": 1.5}, "expected_seq", true},
		// r7 (h2 L1): the generic route mapper's other numeric query keys, and
		// a json.Number through Dispatch rather than at the unit level.
		{[]string{"item", "context"}, map[string]any{"ref": "TASK-1", "budget": 1.5}, "budget", false},
		{[]string{"project", "activity"}, map[string]any{"limit": 2.5}, "limit", false},
		{[]string{"item", "list"}, map[string]any{"limit": json.Number("1.5")}, "limit", false},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.cmd, " ")+" "+tc.param, func(t *testing.T) {
			var reached string
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if tc.readsFirst && r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
					return
				}
				reached = r.Method + " " + r.URL.RequestURI()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			})
			d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
			input := map[string]any{"workspace": "docapp"}
			for k, v := range tc.input {
				input[k] = v
			}
			res, err := d.Dispatch(WithDispatchInput(context.Background(), input), tc.cmd, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("not refused; the request reached %q", reached)
			}
			// The refusal clause must START with the key: the message renders as
			// "<cmdKey>: <param> must be ...". A bare "<param> " substring also
			// matched the hint (expected_seq's "Pass expected_seq as ..."), so it
			// could not see a door naming the wrong key (BUG-4061 r8, Q8).
			txt := textOf(res)
			if !strings.Contains(txt, ": "+tc.param+" must be ") && !strings.Contains(txt, ": "+tc.param+" is ") {
				t.Fatalf("refusal clause does not start with %q: %s", tc.param, txt)
			}
			if !strings.Contains(txt, string(ErrValidationFailed)) {
				t.Fatalf("refusal is not a validation failure: %s", txt)
			}
			if reached != "" {
				t.Fatalf("a refused input reached the handler at %q", reached)
			}
		})
	}
}

// BUG-4061 at the catalog doors: pad_item.list (whose limit used to truncate a
// fraction to the default by design, ruled refuse) and pad_item.history.
func TestBUG4061_CatalogLimitRefusesAFraction(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(context.Context, map[string]any, ActionEnv) (*CallToolResult, error)
	}{
		{"pad_item.list", actionItemList},
		{"pad_item.history", actionItemHistory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &fakeDispatcher{}
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
			res, err := tc.act(context.Background(), map[string]any{"ref": "TASK-1", "limit": 0.5}, env)
			if err != nil {
				t.Fatal(err)
			}
			if res == nil || !res.IsError || !strings.Contains(textOf(res), tc.name+": limit must be a whole number, got 0.5") {
				t.Fatalf("expected a refusal naming limit, got %#v", res)
			}
			if len(disp.gotPath) != 0 {
				t.Fatalf("a refused limit reached the dispatcher: %v", disp.gotPath)
			}
			// Zero still means the default: refusing fractions changes nothing else.
			if res, err := tc.act(context.Background(), map[string]any{"ref": "TASK-1", "limit": float64(0)}, env); err != nil || res.IsError {
				t.Fatalf("limit 0 was refused: %v %s", err, textOf(res))
			}
		})
	}
}

// structuredDispatcher answers every dispatch with a fixed structured body.
type structuredDispatcher struct{ body map[string]any }

func (s structuredDispatcher) Dispatch(context.Context, []string, []string) (*CallToolResult, error) {
	return &mcp.CallToolResult{StructuredContent: s.body}, nil
}

// BUG-4061 site 5: the lines view reads the SERVER's seq, not caller input,
// so it stays lenient (ruled not applicable): absent, nil or malformed leaves
// Seq zero rather than failing the read.
func TestBUG4061_LinesViewReadsTheServerSeqLeniently(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  any
		want int64
	}{
		{"absent", nil, 0},
		{"whole", float64(17), 17},
		{"fraction", 1.5, 0},
		// r7 (h2 L2): a float64 seq of magnitude 2^53 or more is refused by
		// wholeInt64, and the lenient read leaves Seq zero, as for a fraction.
		{"at 2^53", float64(1 << 53), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"ref": "TASK-1", "content": "a\nb\n"}
			if tc.seq != nil {
				body["seq"] = tc.seq
			}
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: structuredDispatcher{body: body}}
			res, err := actionItemGet(context.Background(), map[string]any{"ref": "TASK-1", "lines": "1-2"}, env)
			if err != nil || res.IsError {
				t.Fatalf("lines read failed: %v %s", err, textOf(res))
			}
			b, _ := json.Marshal(res.StructuredContent)
			var view struct {
				Seq int64 `json:"seq"`
			}
			_ = json.Unmarshal(b, &view)
			if view.Seq != tc.want {
				t.Fatalf("seq = %d, want %d", view.Seq, tc.want)
			}
		})
	}
}

// BUG-4061 B: expected_seq outside int64, or as a json.Number, gets the same
// refusal as a fraction rather than an implementation-defined token or an
// unguarded write.
func TestBUG4061_ExpectedSeqRangeAndJSONNumber(t *testing.T) {
	for _, tc := range []struct {
		name    string
		v       any
		want    int64
		wantErr string
	}{
		{"above int64", 1e19, 0, "expected_seq is out of range"},
		// r9 (h2 F4): the float64 path's own bound, not only the unit-level one.
		{"float64 2^53-1 is accepted", float64(1<<53 - 1), 1<<53 - 1, ""},
		{"float64 2^53 is refused", float64(1 << 53), 0, "expected_seq is beyond the exact integer range of a JSON number"},
		{"float64 -2^53 is refused", -float64(1 << 53), 0, "expected_seq is beyond the exact integer range of a JSON number"},
		{"json.Number whole", json.Number("42"), 42, ""},
		{"json.Number fraction", json.Number("42.7"), 0, "expected_seq must be a whole number, got 42.7"},
		{"json.Number whole with an exponent", json.Number("4.2e1"), 42, ""},
		{"json.Number above int64", json.Number("1e19"), 0, "expected_seq is out of range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := itemExpectedSeqParam(map[string]any{"expected_seq": tc.v})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || present {
					t.Fatalf("got %d, %v, %v; want a refusal containing %q", got, present, err, tc.wantErr)
				}
				return
			}
			if err != nil || !present || got != tc.want {
				t.Fatalf("got %d, %v, %v; want %d present", got, present, err, tc.want)
			}
		})
	}
}
