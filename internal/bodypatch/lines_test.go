package bodypatch

import "testing"

func TestParseLineRange(t *testing.T) {
	for in, want := range map[string][2]int{"3": {3, 3}, "2-5": {2, 5}, "4-": {4, 0}, " 1-1 ": {1, 1}} {
		s, e, err := ParseLineRange(in)
		if err != nil || s != want[0] || e != want[1] {
			t.Errorf("%q: %d %d %v", in, s, e, err)
		}
	}
	for _, bad := range []string{"", "0", "-3", "5-2", "a-b", "2-x"} {
		if _, _, err := ParseLineRange(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSliceBodyLines(t *testing.T) {
	lines, from, to, total := SliceBodyLines("a\nb\nc\n", 2, 0)
	if total != 3 || from != 2 || to != 3 || len(lines) != 2 || lines[1].N != 3 || lines[1].Text != "c" {
		t.Fatalf("got %+v %d %d %d", lines, from, to, total)
	}
	lines, _, to, total = SliceBodyLines("a\nb", 1, 9)
	if total != 2 || to != 2 || len(lines) != 2 {
		t.Fatalf("clip: %+v %d %d", lines, to, total)
	}
	lines, _, _, total = SliceBodyLines("", 1, 0)
	if total != 0 || len(lines) != 0 {
		t.Fatalf("empty body: %+v %d", lines, total)
	}
}
