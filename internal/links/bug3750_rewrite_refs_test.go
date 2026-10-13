package links

import "testing"

func TestBUG3750_RewriteRefs(t *testing.T) {
	renamed := map[string]string{"IDEA-1": "IDEA-2", "TASK-2": "TASK-3", "TASK-3": "TASK-4"}
	for _, tc := range []struct{ in, want string }{
		{"a [[IDEA-1]] b", "a [[IDEA-2]] b"},
		{"a [[IDEA-1|the idea]] b", "a [[IDEA-2|the idea]] b"},
		{"a [[idea-1]] b", "a [[IDEA-2]] b"},
		{"chain [[TASK-2]] [[TASK-3]]", "chain [[TASK-3]] [[TASK-4]]"},
		{"title [[IDEA one]] and ws [[w::IDEA-1]]", "title [[IDEA one]] and ws [[w::IDEA-1]]"},
		{"code `[[IDEA-1]]` stays", "code `[[IDEA-1]]` stays"},
		{"unmapped [[TASK-9]]", "unmapped [[TASK-9]]"},
	} {
		if got, _ := RewriteRefs(tc.in, renamed); got != tc.want {
			t.Errorf("RewriteRefs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
