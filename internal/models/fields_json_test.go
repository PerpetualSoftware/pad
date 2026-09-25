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
