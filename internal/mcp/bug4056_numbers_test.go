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

func TestBUG4056BuildQueryNumbersUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"integral", float64(1000000), "1000000"},
		{"large integral", float64(1e19), "10000000000000000000"},
		{"fractional", 2.5, "2.5"},
		{"negative zero", math.Copysign(0, -1), "0"},
		{"json number", json.Number("1e6"), "1e6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := buildQuery(map[string]any{"n": tc.in}, map[string]string{"n": "n"})
			values, err := url.ParseQuery(query)
			if err != nil || values.Get("n") != tc.want {
				t.Fatalf("buildQuery = %q, %v; want n=%q", query, err, tc.want)
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
