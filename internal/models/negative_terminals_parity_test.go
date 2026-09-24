package models

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestNegativeTerminalsMatchTheWebCopy pins the web's NEGATIVE_TERMINALS
// (web/src/lib/types/index.ts) to NegativeTerminals. The web copy decides which
// children its per-row progress counts leave out (BUG-3195); if the two lists
// drift, a nested count and the server's /progress disagree about the same
// child.
func TestNegativeTerminalsMatchTheWebCopy(t *testing.T) {
	src, err := os.ReadFile("../../web/src/lib/types/index.ts")
	if err != nil {
		t.Fatalf("read the web types: %v", err)
	}
	block := regexp.MustCompile(`(?s)const NEGATIVE_TERMINALS = \[(.*?)\];`).FindSubmatch(src)
	if block == nil {
		t.Fatal("NEGATIVE_TERMINALS not found in web/src/lib/types/index.ts; if it moved, move this test's reader with it")
	}
	var web []string
	for _, m := range regexp.MustCompile(`'((?:[^'\\]|\\.)*)'|"([^"]*)"`).FindAllSubmatch(block[1], -1) {
		v := string(m[1]) + string(m[2])
		web = append(web, strings.ReplaceAll(v, `\'`, `'`))
	}
	var goSide []string
	for v := range NegativeTerminals {
		goSide = append(goSide, v)
	}
	sort.Strings(web)
	sort.Strings(goSide)
	if strings.Join(web, "|") != strings.Join(goSide, "|") {
		t.Errorf("the web's NEGATIVE_TERMINALS %q differs from NegativeTerminals %q", web, goSide)
	}
}
