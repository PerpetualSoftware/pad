package models

import (
	"encoding/json"
	"testing"
)

// BUG-3202. DecodeFieldsJSON must change only how an accepted number is
// represented: it accepts and refuses the same blobs json.Unmarshal does, and a
// decode-then-encode leaves every number byte-identical.
func TestDecodeFieldsJSON_RoundTripsNumbersExactly(t *testing.T) {
	const blob = `{"big":9007199254740993,"neg":-9007199254740995,"f":1.0,"e":1e3,"nested":{"n":[18446744073709551617,0.1]}}`
	m, err := DecodeFieldsJSON([]byte(blob))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// json.Marshal sorts keys; compare against the same blob with sorted keys.
	const want = `{"big":9007199254740993,"e":1e3,"f":1.0,"neg":-9007199254740995,"nested":{"n":[18446744073709551617,0.1]}}`
	if string(out) != want {
		t.Fatalf("round trip changed the numbers:\n got %s\nwant %s", out, want)
	}

	// CONTROL: the plain decode this replaces does round, so the assertion
	// above can fail.
	var plain map[string]any
	_ = json.Unmarshal([]byte(blob), &plain)
	if pOut, _ := json.Marshal(plain); string(pOut) == want {
		t.Fatal("control: a plain Unmarshal round trip preserved the numbers, so this test measures nothing")
	}
}

func TestDecodeFieldsJSON_AcceptsWhatUnmarshalAccepts(t *testing.T) {
	cases := []string{
		`{}`, `null`, `{"a":1}`, ` {"a":1} `, `{"a":1e308}`, `{"a":-0}`, `{"a":[1,{"b":2}]}`,
		// refused by both
		`{"a":1e400}`, `{"a":[1e400]}`, `{"a":{"b":-1e999}}`, `{"a":1} x`, `{"a":1}{}`, `{"a":`, `[1]`, `"s"`, ``,
	}
	for _, c := range cases {
		var plain map[string]any
		plainErr := json.Unmarshal([]byte(c), &plain)
		_, err := DecodeFieldsJSON([]byte(c))
		if (plainErr == nil) != (err == nil) {
			t.Errorf("%q: json.Unmarshal err=%v, DecodeFieldsJSON err=%v; they must agree", c, plainErr, err)
		}
	}
}

// CanonicalJSONNumbers: equal values get one spelling, different values keep
// different ones, including two integers above 2^53 that share a float64.
func TestCanonicalJSONNumbers(t *testing.T) {
	same := [][]string{
		{"1000", "1e3", "1.0e3", "1000.0", "1E+3", "10000e-1", "0.1e4"},
		{"0", "-0", "0.0", "0e5"},
		{"-12.5", "-125e-1", "-1.25e1"},
		{"9007199254740993", "9007199254740993.000"},
	}
	for _, group := range same {
		want := CanonicalJSONNumbers(json.Number(group[0]))
		for _, s := range group[1:] {
			if got := CanonicalJSONNumbers(json.Number(s)); got != want {
				t.Errorf("%s canonicalises to %v, but %s to %v; they are the same value", s, got, group[0], want)
			}
		}
	}
	diff := [][2]string{
		{"9007199254740993", "9007199254740992"},
		{"0.1", "0.10000000000000001"},
		{"1000", "-1000"},
		{"1e3", "1e4"},
	}
	for _, p := range diff {
		if CanonicalJSONNumbers(json.Number(p[0])) == CanonicalJSONNumbers(json.Number(p[1])) {
			t.Errorf("%s and %s are different values but canonicalise equal", p[0], p[1])
		}
	}
	// A float64 from a plain decode meets its json.Number twin.
	if CanonicalJSONNumbers(float64(1000)) != CanonicalJSONNumbers(json.Number("1e3")) {
		t.Error("float64 1000 and json.Number 1e3 must canonicalise equal")
	}
	// The output is valid JSON.
	b, err := json.Marshal(CanonicalJSONNumbers(map[string]any{"n": json.Number("1000.0")}))
	if err != nil || !json.Valid(b) {
		t.Errorf("canonical form does not marshal to valid JSON: %s %v", b, err)
	}
}
