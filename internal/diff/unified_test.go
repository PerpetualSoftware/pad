package diff

import "testing"

func TestUnified(t *testing.T) {
	for _, c := range []struct{ name, a, b, want string }{
		{"equal", "x\n", "x\n", ""},
		{"one line changed in the middle",
			"1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n",
			"--- before\n+++ after\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+FIVE\n 6\n 7\n 8\n"},
		{"insert at start",
			"a\nb\n", "z\na\nb\n",
			"--- before\n+++ after\n@@ -1,2 +1,3 @@\n+z\n a\n b\n"},
		{"two far changes make two hunks",
			"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", "ONE\n2\n3\n4\n5\n6\n7\n8\n9\nTEN\n",
			"--- before\n+++ after\n@@ -1,4 +1,4 @@\n-1\n+ONE\n 2\n 3\n 4\n@@ -7,4 +7,4 @@\n 7\n 8\n 9\n-10\n+TEN\n"},
		{"no trailing newline",
			"a\nb", "a\nc",
			"--- before\n+++ after\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Unified(c.a, c.b, 3); got != c.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}
