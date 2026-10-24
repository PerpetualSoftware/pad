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
		{name: "-2^63 is in range", in: -float64(1 << 63), want: -1 << 63, present: true},
		{name: "int", in: 7, want: 7, present: true},
		{name: "int64", in: int64(8), want: 8, present: true},
		{name: "json.Number whole", in: json.Number("42"), want: 42, present: true},
		{name: "json.Number fraction", in: json.Number("1.5"), wantErr: "got 1.5"},
		{name: "json.Number above int64", in: json.Number("1e19"), wantErr: "is out of range"},
		{name: "json.Number whole with an exponent", in: json.Number("1e6"), want: 1000000, present: true},
		{name: "json.Number not a number", in: json.Number("abc"), wantErr: "must be a whole number, got abc"},
		{name: "nil is absent", in: nil},
		{name: "a string is absent, as before", in: "10"},
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
	}{
		{[]string{"attachment", "list"}, map[string]any{"limit": 1.5}, "limit"},
		{[]string{"attachment", "list"}, map[string]any{"offset": 2.5}, "offset"},
		{[]string{"item", "history"}, map[string]any{"ref": "TASK-1", "limit": 1.5}, "limit"},
		{[]string{"item", "starred"}, map[string]any{"limit": 1.5}, "limit"},
		{[]string{"workspace", "audit-log"}, map[string]any{"days": 1.5}, "days"},
		{[]string{"workspace", "audit-log"}, map[string]any{"limit": 1e19}, "limit"},
		{[]string{"item", "list"}, map[string]any{"limit": -2.7}, "limit"},
		{[]string{"item", "list"}, map[string]any{"offset": 1.5}, "offset"},
		// The four buildQuery callers (lead ruling on Q2): the generic route
		// mapper, mapItemSearch, and the standup and changelog dispatchers.
		{[]string{"item", "backlinks"}, map[string]any{"ref": "TASK-1", "limit": 1.5}, "limit"},
		{[]string{"item", "search"}, map[string]any{"query": "x", "offset": 2.5}, "offset"},
		{[]string{"project", "standup"}, map[string]any{"days": 0.5}, "days"},
		{[]string{"project", "changelog"}, map[string]any{"days": 1e19}, "days"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.cmd, " ")+" "+tc.param, func(t *testing.T) {
			var reached string
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				reached = r.URL.RequestURI()
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
			if txt := textOf(res); !strings.Contains(txt, tc.param+" ") || !strings.Contains(txt, string(ErrValidationFailed)) {
				t.Fatalf("refusal does not name %q as a validation failure: %s", tc.param, txt)
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
