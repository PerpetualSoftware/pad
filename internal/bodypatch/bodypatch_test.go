package bodypatch

import (
	"errors"
	"reflect"
	"testing"
)

func TestApply(t *testing.T) {
	body := "alpha\nbeta\ngamma beta\n"
	for _, c := range []struct {
		name  string
		edits []Edit
		want  string
		err   any
	}{
		{"single unique", []Edit{{Old: "alpha", New: "ALPHA"}}, "ALPHA\nbeta\ngamma beta\n", nil},
		{"edits apply in order to the evolving body", []Edit{{Old: "alpha", New: "x"}, {Old: "x\nbeta", New: "y"}}, "y\ngamma beta\n", nil},
		{"missing", []Edit{{Old: "delta", New: "d"}}, "", &AnchorMissingError{}},
		{"ambiguous without hint", []Edit{{Old: "beta", New: "b"}}, "", &AnchorAmbiguousError{}},
		{"hint picks a match", []Edit{{Old: "beta", New: "B", LineHint: 3}}, "alpha\nbeta\ngamma B\n", nil},
		{"hint on no match is still ambiguous", []Edit{{Old: "beta", New: "B", LineHint: 1}}, "", &AnchorAmbiguousError{}},
		{"hint ignored when unique", []Edit{{Old: "gamma", New: "G", LineHint: 1}}, "alpha\nbeta\nG beta\n", nil},
		{"empty old refused", []Edit{{Old: "alpha", New: "a"}, {Old: "", New: "z"}}, "", &EmptyAnchorError{}},
		{"overlapping occurrences are ambiguous", []Edit{{Old: "aa", New: "b"}}, "", &AnchorAmbiguousError{}},
		{"a later failure applies nothing", []Edit{{Old: "alpha", New: "a"}, {Old: "nope", New: "z"}}, "", &AnchorMissingError{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := body
			if c.name == "overlapping occurrences are ambiguous" {
				in = "aaa"
			}
			got, err := Apply(in, c.edits)
			if c.err != nil {
				target := reflect.New(reflect.TypeOf(c.err).Elem()).Interface()
				if err == nil || !errors.As(err, &target) {
					t.Fatalf("err = %v, want %T", err, c.err)
				}
				if got != "" {
					t.Fatalf("a refused patch returned a body: %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestAmbiguousNamesEveryLine(t *testing.T) {
	_, err := Apply("x\ny x\nx", []Edit{{Old: "x", New: "z"}})
	var amb *AnchorAmbiguousError
	if !errors.As(err, &amb) || !reflect.DeepEqual(amb.Lines, []int{1, 2, 3}) {
		t.Fatalf("err = %v", err)
	}
}
