package mcp

import (
	"reflect"
	"strings"
	"testing"
)

// TASK-3536: tool descriptions are reflowed for the wire. Whitespace only:
// every word survives in order, every action keeps its own header line, and
// Required:/Optional: keep their own lines.
func TestCompactToolDescriptionKeepsEveryWord(t *testing.T) {
	if len(Catalog) == 0 {
		t.Fatal("premise: empty catalog")
	}
	for _, def := range Catalog {
		got := compactToolDescription(def.Description)
		if !reflect.DeepEqual(strings.Fields(got), strings.Fields(def.Description)) {
			t.Errorf("%s: the reflow changed the words", def.Name)
		}
		for action := range def.Actions {
			if !strings.Contains("\n"+got, "\n  "+action+" ") {
				t.Errorf("%s: action %q lost its header line", def.Name, action)
			}
		}
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "     ") && !(strings.HasPrefix(line, "      ") && isListItemLine(strings.TrimSpace(line))) {
				t.Errorf("%s: a line keeps a deep indent: %q", def.Name, line)
			}
		}
		if len(got) > len(def.Description) {
			t.Errorf("%s: the reflow grew the description", def.Name)
		}
	}
}

func TestCompactToolDescriptionShape(t *testing.T) {
	in := "Things.\n\nActions:\n  make   — Make one.\n             Required: name.\n             Optional: size, which\n             may be large.\n  drop   — Drop one,\n    carefully.\n\nUse it well."
	want := "Things.\n\nActions:\n  make   — Make one.\n    Required: name.\n    Optional: size, which may be large.\n  drop   — Drop one, carefully.\n\nUse it well."
	if got := compactToolDescription(in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// Codex round 1: a list must stay a list. Every bullet, numbered item and
// one-word label in the source starts its own line on the wire, and a bullet's
// wrapped continuation joins the bullet, not the line before it.
func TestCompactToolDescriptionKeepsListStructure(t *testing.T) {
	in := "Things.\n  drop   — Drop one.\n             Required: slug.\n             Constraints:\n               - Cannot drop a default\n                 collection.\n               - Items stay.\n               1. First step\n                  of two.\n               2) Second."
	want := "Things.\n  drop   — Drop one.\n    Required: slug.\n    Constraints:\n      - Cannot drop a default collection.\n      - Items stay.\n      1. First step of two.\n      2) Second."
	if got := compactToolDescription(in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for _, def := range Catalog {
		got := "\n" + compactToolDescription(def.Description)
		for _, line := range strings.Split(def.Description, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(line, " ") || !(isListItemLine(trimmed) || isLabelLine(trimmed)) {
				continue
			}
			if !strings.Contains(got, "\n    "+trimmed) && !strings.Contains(got, "\n      "+trimmed) {
				t.Errorf("%s: %q no longer starts its own line", def.Name, trimmed)
			}
		}
	}
}
