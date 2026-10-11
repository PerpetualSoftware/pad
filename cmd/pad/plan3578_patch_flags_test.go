package main

import (
	"strings"
	"testing"
)

// PLAN-3578: the CLI builds a patch's edits from --old/--new pairs or --edits
// and refuses a bad combination before any request.
func TestPLAN3578_PatchEditsFromFlags(t *testing.T) {
	got, err := patchEditsFromFlags([]string{"a", "b"}, []string{"A", ""}, "", nil)
	if err != nil || len(got) != 2 || got[1].Old != "b" || got[1].New != "" {
		t.Fatalf("pairs: %+v %v", got, err)
	}
	got, err = patchEditsFromFlags(nil, nil, `[{"old":"x","new":"y","line_hint":3}]`, nil)
	if err != nil || len(got) != 1 || got[0].LineHint != 3 {
		t.Fatalf("json: %+v %v", got, err)
	}
	got, err = patchEditsFromFlags(nil, nil, "-", strings.NewReader(`[{"old":"x","new":"y"}]`))
	if err != nil || len(got) != 1 {
		t.Fatalf("stdin: %+v %v", got, err)
	}
	for name, c := range map[string]struct {
		olds, news []string
		edits      string
		want       string
	}{
		"unpaired":       {[]string{"a"}, nil, "", "each --old needs its --new"},
		"none":           {nil, nil, "", "at least one"},
		"mixed":          {[]string{"a"}, []string{"b"}, `[]`, "conflicts"},
		"empty anchor":   {[]string{""}, []string{"b"}, "", "old is empty"},
		"bad json":       {nil, nil, `{`, "JSON array"},
		"empty json old": {nil, nil, `[{"old":"","new":"z"}]`, "old is empty"},
	} {
		if _, err := patchEditsFromFlags(c.olds, c.news, c.edits, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}
