package models

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// BUG-3546: a collection whose status option is the hyphenated `wont-fix`, with
// no terminal_options declared, never closed those items, because the fallback
// lists spelled it only `wontfix` / `won't fix`.
func TestFallbackTerminalsCoverEveryWontFixSpelling(t *testing.T) {
	schema := CollectionSchema{Fields: []FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "fixed", "wont-fix"}},
	}}
	_, terminals := TerminalValuesForDoneField(schema, CollectionSettings{})
	_, positive := PositiveTerminalValuesForDoneField(schema, CollectionSettings{})
	for _, v := range []string{"wontfix", "wont-fix", "won't fix", "won't-fix"} {
		if !containsString(terminals, v) {
			t.Errorf("%q is not a fallback terminal value", v)
		}
		if !IsNegativeTerminal(v) {
			t.Errorf("%q is not a negative terminal, so it would count as shipped work", v)
		}
		if containsString(positive, v) {
			t.Errorf("%q counts as completed work", v)
		}
	}
	if !containsString(positive, "fixed") {
		t.Error("control: fixed must still count as completed work")
	}
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// The web mirrors both fallback lists (web/src/lib/types/index.ts) and they
// must not drift: a value the server treats as closed and the board treats as
// open is the BUG-3546 shape again. The comment that asked for this was not
// enough on its own.
func TestTerminalListsMatchWeb(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "types", "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	negative := make([]string, 0, len(NegativeTerminals))
	for v := range NegativeTerminals {
		negative = append(negative, v)
	}
	for _, c := range []struct {
		tsName string
		goList []string
	}{
		{"DEFAULT_TERMINAL_STATUSES", DefaultTerminalStatuses},
		{"NEGATIVE_TERMINALS", negative},
	} {
		got := tsStringArray(t, string(src), c.tsName)
		want := append([]string(nil), c.goList...)
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("web %s = %q, Go = %q", c.tsName, got, want)
		}
	}
}

// tsStringArray extracts the string literals of `const <name> = [ ... ];`.
func tsStringArray(t *testing.T, src, name string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)const ` + name + ` = \[(.*?)\];`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("const %s = [...] not found in web/src/lib/types/index.ts", name)
	}
	var out []string
	for _, lit := range regexp.MustCompile(`'([^']*)'|"([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
		if lit[1] != "" {
			out = append(out, lit[1])
		} else {
			out = append(out, lit[2])
		}
	}
	if len(out) == 0 {
		t.Fatalf("const %s has no string literals", name)
	}
	return out
}
