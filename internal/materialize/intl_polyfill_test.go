package materialize

import (
	"strings"
	"testing"

	pad "github.com/PerpetualSoftware/pad"
)

// TestIntlNumberFormatPolyfill pins the bundle's Intl.NumberFormat fallback
// (web/src/lib/collab/materializer/polyfills.ts) under goja, which has no
// Intl. cssomnom (linkedom's CSS layer) builds a formatter at module load, so
// without the fallback the bundle fails to load at all. Expected strings were
// measured from Node's real Intl.NumberFormat("en-US", {useGrouping: false,
// minimumFractionDigits: 0, maximumFractionDigits: 6}).
func TestIntlNumberFormatPolyfill(t *testing.T) {
	r, err := New(pad.MaterializerJS)
	if err != nil {
		t.Fatalf("load embedded bundle: %v", err)
	}
	if _, err := r.vm.RunString(`var __nf = new Intl.NumberFormat("en-US", {useGrouping: false, minimumFractionDigits: 0, maximumFractionDigits: 6});`); err != nil {
		t.Fatalf("construct cssomnom's formatter: %v", err)
	}
	cases := []struct{ expr, want string }{
		{"1", "1"},
		{"0.5", "0.5"},
		{"-0.5", "-0.5"},
		{"12345678", "12345678"},
		{"0.1+0.2", "0.3"},
		{"1/3", "0.333333"},
		{"-2/3", "-0.666667"},
		{"1e-7", "0"},
		{"-1e-7", "-0"},
		{"-0", "-0"},
		{"5e-7", "0.000001"},
		{"-5e-7", "-0.000001"},
		{"0.0000015", "0.000002"},
		{"0.0000025", "0.000003"},
		{"123.4560001", "123.456"},
		{"1e20", "100000000000000000000"},
		{"1e21", "1000000000000000000000"},
		{"1.23e25", "12300000000000000000000000"},
		{"-42.000004", "-42.000004"},
		{"100", "100"},
		{"0.000001", "0.000001"},
		{"0.9999995", "1"},
		{"9.9999995", "10"},
		{"2.0000005", "2.000001"},
		{"1.0000004999", "1"},
		{"5e-324", "0"},
		{"123456.7890125", "123456.789013"},
		{"0.1234565", "0.123457"},
		{"NaN", "NaN"},
		{"Infinity", "∞"},
		{"-Infinity", "-∞"},
	}
	for _, c := range cases {
		v, err := r.vm.RunString("__nf.format(" + c.expr + ")")
		if err != nil {
			t.Errorf("format(%s): %v", c.expr, err)
			continue
		}
		if got := v.String(); got != c.want {
			t.Errorf("format(%s) = %q, want %q", c.expr, got, c.want)
		}
	}

	zero := []struct{ expr, want string }{
		{"2.5", "3"},
		{"100", "100"},
	}
	for _, c := range zero {
		v, err := r.vm.RunString(`new Intl.NumberFormat("en-US", {useGrouping: false, maximumFractionDigits: 0}).format(` + c.expr + `)`)
		if err != nil {
			t.Errorf("maxFD 0 format(%s): %v", c.expr, err)
			continue
		}
		if got := v.String(); got != c.want {
			t.Errorf("maxFD 0 format(%s) = %q, want %q", c.expr, got, c.want)
		}
	}

	refusals := map[string]string{
		"locale de-DE":   `new Intl.NumberFormat("de-DE", {useGrouping: false})`,
		"useGrouping on": `new Intl.NumberFormat("en-US", {useGrouping: true})`,
		"unknown option": `new Intl.NumberFormat("en-US", {useGrouping: false, style: "percent"})`,
	}
	for name, src := range refusals {
		v, err := r.vm.RunString(`(function(){ try { ` + src + `; return "no throw"; } catch (e) { return (e instanceof TypeError ? "TypeError: " : "other: ") + e.message; } })()`)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := v.String(); !strings.HasPrefix(got, "TypeError: Intl.NumberFormat polyfill: unsupported") {
			t.Errorf("%s: got %q, want a TypeError starting %q", name, got, "Intl.NumberFormat polyfill: unsupported")
		}
	}
}
