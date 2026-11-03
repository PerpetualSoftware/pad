package main

import (
	"strings"
	"testing"
)

// BUG-4265: `pad item comment` takes --stdin like create and update do, and
// refuses an empty or whitespace-only body before anything is written.
// What a refusal prevents is a write, so every refusal leg counts the stub's
// writes.

const multiLineCommentBody = "first line\n\n`code` and \"double\" and 'single' quotes\n$HOME and \\n stay literal\n\nlast line\n"

func TestCommentStdin_PostsTheBodyExactly(t *testing.T) {
	stub := &commentStub{caps: "new"}
	restore := feedStdin(t, multiLineCommentBody)
	defer restore()

	if err := runCommentCmd(t, stub, commentCmd(), "TASK-5", "--stdin"); err != nil {
		t.Fatal(err)
	}
	if len(stub.writes) != 1 || !strings.HasPrefix(stub.writes[0], "POST ") || !strings.HasSuffix(stub.writes[0], "/items/TASK-5/comments") {
		t.Fatalf("writes = %v, want one POST to the item's comments", stub.writes)
	}
	if got := stub.bodies[0]["body"]; got != multiLineCommentBody {
		t.Errorf("posted body = %q, want the piped text exactly: %q", got, multiLineCommentBody)
	}
}

func TestCommentArgument_StillPosts(t *testing.T) {
	stub := &commentStub{caps: "new"}
	if err := runCommentCmd(t, stub, commentCmd(), "TASK-5", "Reproduced on main"); err != nil {
		t.Fatal(err)
	}
	if len(stub.bodies) != 1 || stub.bodies[0]["body"] != "Reproduced on main" {
		t.Errorf("posted = %v, want the argument as the body", stub.bodies)
	}
}

func TestComment_RefusesAnEmptyBody_NothingPosted(t *testing.T) {
	for name, tc := range map[string]struct {
		stdin string // "" and not useStdin means the argument form
		args  []string
	}{
		"empty argument":          {args: []string{"TASK-5", ""}},
		"whitespace argument":     {args: []string{"TASK-5", " \t\n "}},
		"empty stdin":             {stdin: "", args: []string{"TASK-5", "--stdin"}},
		"whitespace-only stdin":   {stdin: "  \n\t\n", args: []string{"TASK-5", "--stdin"}},
		"stdin and an argument":   {stdin: "a real body\n", args: []string{"TASK-5", "also a body", "--stdin"}},
		"stdin and an empty arg":  {stdin: "a real body\n", args: []string{"TASK-5", "", "--stdin"}},
		"stdin with a blank body": {stdin: "\n", args: []string{"TASK-5", "--stdin"}},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &commentStub{caps: "new"}
			if tc.stdin != "" || containsFlag(tc.args, "--stdin") {
				restore := feedStdin(t, tc.stdin)
				defer restore()
			}
			err := runCommentCmd(t, stub, commentCmd(), tc.args...)
			if err == nil {
				t.Fatalf("exit was zero, want a refusal; writes = %v", stub.writes)
			}
			if len(stub.writes) != 0 {
				t.Errorf("writes = %v after a refusal, want none", stub.writes)
			}
		})
	}
}

func containsFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
