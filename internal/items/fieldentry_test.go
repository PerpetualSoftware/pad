package items

import (
	"errors"
	"strings"
	"testing"
)

// The asymmetry IS the contract: a padded key is refused, a padded value is
// carried through untouched. These cases are written as a table of the exact
// strings the two doors disagreed about before BUG-2870, so a future edit that
// "tidies up" by trimming values fails here rather than in someone's data.
func TestSplitFieldEntry(t *testing.T) {
	cases := []struct {
		name      string
		entry     string
		wantKey   string
		wantValue string
		wantErr   string // substring; "" means no error
		malformed bool   // expect ErrFieldEntryMalformed specifically
	}{
		{name: "plain", entry: "effort=l", wantKey: "effort", wantValue: "l"},

		// The reported case. The CLI stored a field literally named " effort";
		// the remote door wrote `effort`. Now neither does.
		{name: "padded key refused", entry: " effort=l", wantErr: "whitespace around its key"},
		{name: "trailing padded key refused", entry: "effort =l", wantErr: "whitespace around its key"},
		{name: "tab padded key refused", entry: "\teffort=l", wantErr: "whitespace around its key"},

		// A whitespace-only key names no field at all, so it gets its own
		// message rather than advice to write the trimmed form (which is "").
		{name: "whitespace-only key", entry: " =l", wantErr: "only whitespace"},

		// Values are content. All four of these used to reach the two doors
		// differently; now both doors get exactly what was typed.
		{name: "padded value kept", entry: "cost= 3", wantKey: "cost", wantValue: " 3"},
		{name: "trailing padded value kept", entry: "note=x ", wantKey: "note", wantValue: "x "},
		{name: "value that is only a space", entry: "note= ", wantKey: "note", wantValue: " "},
		{name: "empty value", entry: "note=", wantKey: "note", wantValue: ""},

		// The value half owns every remaining `=`, so a value can contain one.
		{name: "value containing equals", entry: "expr=a=b", wantKey: "expr", wantValue: "a=b"},

		// Malformed: the callers disagree about what to DO here, so the helper
		// only classifies it.
		{name: "no separator", entry: "effort", malformed: true},
		{name: "leading separator", entry: "=l", malformed: true},
		{name: "empty entry", entry: "", malformed: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, value, err := SplitFieldEntry(tc.entry)

			switch {
			case tc.malformed:
				if !errors.Is(err, ErrFieldEntryMalformed) {
					t.Fatalf("SplitFieldEntry(%q) error = %v, want ErrFieldEntryMalformed", tc.entry, err)
				}
				return
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("SplitFieldEntry(%q) = (%q, %q), want an error containing %q",
						tc.entry, key, value, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("SplitFieldEntry(%q) error = %q, want it to contain %q", tc.entry, err, tc.wantErr)
				}
				if errors.Is(err, ErrFieldEntryMalformed) {
					t.Errorf("a padded key must not classify as malformed — callers skip malformed entries silently")
				}
				return
			}

			if err != nil {
				t.Fatalf("SplitFieldEntry(%q) unexpected error: %v", tc.entry, err)
			}
			if key != tc.wantKey || value != tc.wantValue {
				t.Errorf("SplitFieldEntry(%q) = (%q, %q), want (%q, %q)",
					tc.entry, key, value, tc.wantKey, tc.wantValue)
			}
		})
	}
}

// The refusal has to be actionable: it must show the caller the form to write.
// A message that only says "bad key" leaves them guessing which half is wrong.
func TestSplitFieldEntry_RefusalNamesTheCorrectedForm(t *testing.T) {
	_, _, err := SplitFieldEntry(" effort=l")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), `"effort=l"`) {
		t.Errorf("refusal does not show the corrected entry: %v", err)
	}
	if !strings.Contains(err.Error(), `" effort=l"`) {
		t.Errorf("refusal does not show what the caller wrote: %v", err)
	}
}

// A padded VALUE must never be reported as a key problem — that message would
// send the caller to fix the half that is fine.
func TestSplitFieldEntry_PaddedValueIsNotAKeyRefusal(t *testing.T) {
	key, value, err := SplitFieldEntry("cost= 3")
	if err != nil {
		t.Fatalf("padded value must be accepted, got: %v", err)
	}
	if key != "cost" || value != " 3" {
		t.Fatalf("got (%q, %q), want (\"cost\", \" 3\")", key, value)
	}
}
