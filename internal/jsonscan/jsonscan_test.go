package jsonscan

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// readAll drives the scanner to its end, reporting whether it accepted the
// input as exactly one well-formed value.
func readAll(doc []byte) (strs []string, ok bool) {
	sc := NewScanner(doc)
	for {
		k, s, err := sc.Next()
		if err == io.EOF {
			return strs, true
		}
		if err != nil {
			return strs, false
		}
		if k == String {
			strs = append(strs, string(s))
		}
	}
}

// The scanner's acceptance must be encoding/json v1's, or a check built on it
// answers about documents the typed decode reads differently. The oracle is
// json.Unmarshal into `any` for syntax, and json.Valid where Unmarshal refuses
// on range alone (1e999), since the scanner never converts a number.
func TestScannerAcceptsWhatEncodingJSONAccepts(t *testing.T) {
	inputs := []string{
		`{}`, `[]`, `null`, `"s"`, `0`, `-0`, `1e999`, `-1.5e-10`, `true`,
		`{"a":1,"a":2}`, `{"a":{"b":[1,{"c":null}]}}`,
		" \t\r\n{\"a\":1} \n", // surrounding whitespace
		`{"a":1} {}`, `{"a":1}x`, `{"a":1`, `{"a":}`, `{a:1}`, `[1,]`, `01`, `+1`, `.5`,
		"\"a\x01b\"",   // raw control character in a string
		"\"\xff\xfe\"", // invalid UTF-8 inside a string
		`"\ud800"`,     // lone surrogate escape
		`"\u0000"`,     // NUL escape
		`"\x"`,         // invalid escape
		``, ` `,        // empty
		strings.Repeat("[", 500) + strings.Repeat("]", 500),
		// encoding/json's nesting limit is 10000; the two must agree at it.
		strings.Repeat("[", 10000) + strings.Repeat("]", 10000),
		strings.Repeat("[", 10001) + strings.Repeat("]", 10001),
	}
	for _, in := range inputs {
		want := json.Valid([]byte(in))
		_, got := readAll([]byte(in))
		if got != want {
			t.Errorf("%q: scanner accepts=%v, encoding/json accepts=%v", in, got, want)
		}
	}
}

// A lone surrogate is v1's U+FFFD, not a malformed document: a scanner that
// called it malformed made the whole body report nothing, so a NUL beside it
// passed every check (found by the differential above).
func TestScannerReadsALoneSurrogateAsV1Does(t *testing.T) {
	strs, ok := readAll([]byte(`["\ud800","a\u0000b"]`))
	if !ok {
		t.Fatal("rejected a document encoding/json accepts")
	}
	if len(strs) != 2 || strs[0] != "\ufffd" || strs[1] != "a\x00b" {
		t.Errorf("strings = %q", strs)
	}
}

// Strings come back DECODED, keys included, and every occurrence of a repeated
// key arrives.
func TestScannerDecodesStringsAndKeepsRepeats(t *testing.T) {
	strs, ok := readAll([]byte(`{"a":"x\u0000y","a":"plain","ké":"\\u0000"}`))
	if !ok {
		t.Fatal("rejected a valid document")
	}
	want := []string{"a", "x\x00y", "a", "plain", "ké", `\u0000`}
	if strings.Join(strs, "|") != strings.Join(want, "|") {
		t.Errorf("strings = %q, want %q", strs, want)
	}
}

func TestScannerMalformedIsErrMalformed(t *testing.T) {
	sc := NewScanner([]byte(`{"a":`))
	var err error
	for err == nil {
		_, _, err = sc.Next()
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("want ErrMalformed, got %v", err)
	}
}

func TestFoldNameMatchesEncodingJSON(t *testing.T) {
	for _, key := range []string{"k", "K", "K", "kk", "k ", "ｋ", "ß", "ſ", "s", "S"} {
		var v struct {
			K json.RawMessage `json:"k"`
			S json.RawMessage `json:"s"`
		}
		if err := json.Unmarshal([]byte(`{"`+key+`":1}`), &v); err != nil {
			t.Fatal(err)
		}
		reachesK, reachesS := v.K != nil, v.S != nil
		if got := FoldName([]byte(key)) == FoldName([]byte("k")); got != reachesK {
			t.Errorf("%q: folds to k = %v, encoding/json decodes it into k = %v", key, got, reachesK)
		}
		if got := FoldName([]byte(key)) == FoldName([]byte("s")); got != reachesS {
			t.Errorf("%q: folds to s = %v, encoding/json decodes it into s = %v", key, got, reachesS)
		}
	}
}
