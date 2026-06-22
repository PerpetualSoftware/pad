package artifact

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRoundTripPlaybook(t *testing.T) {
	want := samplePlaybook()
	data, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestRoundTripConvention(t *testing.T) {
	want := sampleConvention()
	data, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestDecodeMissingFrontmatter(t *testing.T) {
	cases := []string{
		"no frontmatter here\n",
		"---\nonly opening fence\n",
		"---\n---\n\nbody\n", // empty frontmatter
	}
	for _, in := range cases {
		_, err := Decode([]byte(in))
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("input %q: got %v, want ErrMalformed", in, err)
		}
	}
}

func TestDecodeUnsupportedVersion(t *testing.T) {
	in := "---\npad_artifact: playbook\nformat_version: 2\ntitle: x\nprovenance:\n  format_version: 1\n---\n\nbody\n"
	_, err := Decode([]byte(in))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("got %v, want ErrUnsupportedVersion", err)
	}
}

func TestDecodeUnknownKind(t *testing.T) {
	in := "---\npad_artifact: nonsense\nformat_version: 1\ntitle: x\n---\n\nbody\n"
	_, err := Decode([]byte(in))
	if !errors.Is(err, ErrUnknownKind) {
		t.Errorf("got %v, want ErrUnknownKind", err)
	}
}

func TestDecodeBodyPreserved(t *testing.T) {
	body := "Line one\n\nLine three\n  indented\n"
	a := sampleConvention()
	a.Body = body
	data, err := Encode(a)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Body != body {
		t.Errorf("body not preserved:\ngot:  %q\nwant: %q", got.Body, body)
	}
}

func TestDecodeCRLFTolerant(t *testing.T) {
	// An artifact authored/transported on Windows uses CRLF. Decoding it must
	// yield the same result as the LF form: fences detected, body LF-normalized.
	a := sampleConvention()
	a.Body = "Line one\n\nLine three\n"
	lf, err := Encode(a)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	crlf := strings.ReplaceAll(string(lf), "\n", "\r\n")
	got, err := Decode([]byte(crlf))
	if err != nil {
		t.Fatalf("Decode(CRLF): %v", err)
	}
	if got.Kind != a.Kind || got.Title != a.Title {
		t.Errorf("CRLF decode mismatch: got kind=%q title=%q", got.Kind, got.Title)
	}
	if got.Body != a.Body {
		t.Errorf("CRLF body not LF-normalized:\ngot:  %q\nwant: %q", got.Body, a.Body)
	}
}

func TestFieldKeysForKind(t *testing.T) {
	if _, err := FieldKeysForKind("nonsense"); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("got %v, want ErrUnknownKind", err)
	}
	pb, err := FieldKeysForKind(KindPlaybook)
	if err != nil {
		t.Fatal(err)
	}
	if len(pb) != 5 {
		t.Errorf("playbook keys: got %d, want 5", len(pb))
	}
}
