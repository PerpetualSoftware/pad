package mcp

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestFormatNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want string
		ok   bool
	}{
		{"million", float64(1000000), "1000000", true},
		{"one and a half million", float64(1500000), "1500000", true},
		{"quadrillion", float64(1e15), "1000000000000000", true},
		{"outside int64", float64(1e19), "10000000000000000000", true},
		{"fractional", 2.5, "2.5", true},
		{"negative", float64(-3), "-3", true},
		{"negative zero", math.Copysign(0, -1), "0", true},
		{"last float below int64 upper bound", math.Nextafter(0x1p63, 0), "9223372036854774784", true},
		{"int64 upper bound", float64(0x1p63), "9223372036854776000", true},
		{"int64 minimum", float64(math.MinInt64), "-9223372036854775808", true},
		{"below int64 minimum", math.Nextafter(-0x1p63, math.Inf(-1)), "-9223372036854778000", true},
		{"float32 integral", float32(1000000), "1000000", true},
		{"float32 fractional", float32(0.1), "0.10000000149011612", true},
		{"float32 outside int64", float32(0x1p64), "18446744073709552000", true},
		{"float32 negative zero", float32(math.Copysign(0, -1)), "0", true},
		{"int", int(7), "7", true},
		{"int32", int32(7), "7", true},
		{"int64", int64(7), "7", true},
		{"uint", uint(7), "7", true},
		{"uint32", uint32(7), "7", true},
		{"uint64", uint64(math.MaxUint64), "18446744073709551615", true},
		{"json number", json.Number("7"), "7", true},
		{"json exponent preserved", json.Number("1e6"), "1e6", true},
		{"nan", math.NaN(), "NaN", true},
		{"positive infinity", math.Inf(1), "+Inf", true},
		{"negative infinity", math.Inf(-1), "-Inf", true},
		{"string", "7", "", false},
		{"bool", true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := formatNumber(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("formatNumber(%v) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// BUG-4056 r1: array elements and playbook argument tokens share the helper.
func TestBUG4056SliceAndPlaybookNumbers(t *testing.T) {
	got, err := toStringSlice([]any{float64(1000000), float64(2.5), "x", true})
	if err != nil || strings.Join(got, ",") != "1000000,2.5,x,true" {
		t.Fatalf("toStringSlice = %q, %v", got, err)
	}

	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	if _, err := padPlaybookTool.Actions["run"](context.Background(), map[string]any{
		"ref":  "release",
		"args": map[string]any{"n": float64(1000000), "version": float64(1500000)},
	}, env); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	for _, want := range []string{"n=1000000", "version=1500000"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("playbook tokens %q lack %q", joined, want)
		}
	}
	if strings.Contains(joined, "e+06") {
		t.Fatalf("playbook tokens carry exponent text: %q", joined)
	}
}
