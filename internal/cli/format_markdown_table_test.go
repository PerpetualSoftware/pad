package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderMarkdownTable_Shape(t *testing.T) {
	var buf bytes.Buffer
	RenderMarkdownTable(&buf, []string{"A", "B"}, [][]string{{"1", "2"}, {"3", "4"}})

	want := strings.Join([]string{
		"| A | B |",
		"| --- | --- |",
		"| 1 | 2 |",
		"| 3 | 4 |",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Errorf("table =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderMarkdownTable_EscapesCells(t *testing.T) {
	var buf bytes.Buffer
	RenderMarkdownTable(&buf, []string{"A"}, [][]string{
		{"pipe | here"},
		{"line\nbreak"},
		{"ansi \x1b[2K here"},
	})
	out := buf.String()

	if !strings.Contains(out, `pipe \| here`) {
		t.Errorf("pipe not escaped:\n%s", out)
	}
	if strings.Contains(out, "line\nbreak") {
		t.Errorf("newline not collapsed:\n%q", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("escape byte survived:\n%q", out)
	}
	// Header + separator + three rows, and nothing extra from the newline cell.
	if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")); n != 5 {
		t.Errorf("got %d lines, want 5:\n%s", n, out)
	}
}

// A row with the wrong number of cells must not change the column count, or the
// whole table stops parsing as a table.
func TestRenderMarkdownTable_NormalizesRaggedRows(t *testing.T) {
	var buf bytes.Buffer
	RenderMarkdownTable(&buf, []string{"A", "B", "C"}, [][]string{
		{"only-one"},
		{"one", "two", "three", "four-is-too-many"},
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for i, line := range lines {
		if n := strings.Count(line, "|"); n != 4 {
			t.Errorf("line %d has %d pipes, want 4 (3 cells): %q", i, n, line)
		}
	}
	if strings.Contains(buf.String(), "four-is-too-many") {
		t.Errorf("overflow cell was not dropped:\n%s", buf.String())
	}
}

func TestRenderMarkdownTable_NoHeadersWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	RenderMarkdownTable(&buf, nil, [][]string{{"x"}})
	if buf.Len() != 0 {
		t.Errorf("expected no output without headers, got %q", buf.String())
	}
}
