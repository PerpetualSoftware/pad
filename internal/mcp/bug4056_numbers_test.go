package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4056 pinned notation: an integral number forwards without an exponent.
// BUG-4061 (lead ruling on its Q2, day 33) superseded the rows that forwarded
// a fraction and a value outside int64 unchanged: every numeric query
// parameter is an integer and the server's parsers default a malformed one,
// so those two are now refusals naming the key. The notation rows stand.
func TestBUG4056BuildQueryNumbersUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      any
		want    string
		wantErr string
	}{
		{name: "integral", in: float64(1000000), want: "1000000"},
		{name: "large integral outside int64", in: float64(1e19), wantErr: "n is out of range"},
		{name: "fractional", in: 2.5, wantErr: "n must be a whole number, got 2.5"},
		{name: "negative zero", in: math.Copysign(0, -1), want: "0"},
		{name: "json number", in: json.Number("1e6"), want: "1000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, qerr := buildQuery(map[string]any{"n": tc.in}, map[string]string{"n": "n"})
			if tc.wantErr != "" {
				if qerr == nil || !strings.Contains(qerr.Error(), tc.wantErr) {
					t.Fatalf("buildQuery = %q, %v; want a refusal containing %q", query, qerr, tc.wantErr)
				}
				return
			}
			values, err := url.ParseQuery(query)
			if qerr != nil || err != nil || values.Get("n") != tc.want {
				t.Fatalf("buildQuery = %q, %v, %v; want n=%q", query, qerr, err, tc.want)
			}
		})
	}
}

func TestBUG4056ChatRemoteNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, verb, field string
		in                any
		invalid           bool
	}{
		{"million", "read", "to", float64(1000000), false},
		{"string", "read", "to", "1000000", false},
		{"int", "read", "to", int(1000000), false},
		{"int64", "read", "to", int64(1000000), false},
		{"out of range", "delete", "seq", float64(1e19), true},
		{"int64 upper bound", "delete", "seq", float64(math.MaxInt64), true},
		{"fractional", "delete", "seq", 2.5, true},
		{"nan", "delete", "seq", math.NaN(), true},
		{"infinity", "delete", "seq", math.Inf(1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := &HTTPHandlerDispatcher{
				UserResolver: func(context.Context) *models.User { return &models.User{ID: "u"} },
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if tc.invalid {
						t.Error("invalid sequence reached handler")
					}
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/me/conversations/12/read" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					}
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body["seq"]) != "1000000" {
						t.Errorf("request body = %s, %v; want seq=1000000", body, err)
					}
					fmt.Fprint(w, `{"read_seq":1000000}`)
				}),
			}
			// @name forces target resolution to make a request if validation
			// ever moves after it; invalid input must reach no handler at all.
			target := "C12"
			if tc.invalid {
				target = "@rook"
			}
			input := map[string]any{"target": target, tc.field: tc.in}
			res, err := d.Dispatch(WithDispatchInput(context.Background(), input), []string{"chat", tc.verb}, nil)
			if err != nil || res == nil {
				t.Fatalf("Dispatch = %v, %v", res, err)
			}
			if tc.invalid {
				if !res.IsError || !strings.Contains(textOf(res), "validation_failed") || !strings.Contains(textOf(res), "seq must be an integer of at least 1") || calls != 0 {
					t.Fatalf("want validation error without requests; calls=%d result=%s", calls, textOf(res))
				}
			} else if res.IsError || calls != 1 {
				t.Fatalf("want successful read request; calls=%d result=%s", calls, textOf(res))
			}
		})
	}
}
