package cli

import "testing"

func TestChatTarget(t *testing.T) {
	for _, tc := range []struct {
		input  string
		number int
		agent  string
		bad    bool
	}{
		{"C12", 12, "", false}, {"12", 12, "", false}, {"@rook", 0, "rook", false},
		{"C", 0, "", true}, {"C0", 0, "", true}, {"@", 0, "", true},
		{"rook", 0, "", true}, {"C-1", 0, "", true}, {"C1x", 0, "", true},
		{"0", 0, "", true}, {"+1", 0, "", true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			n, a, err := ParseChatTarget(tc.input)
			if (err != nil) != tc.bad || n != tc.number || a != tc.agent {
				t.Fatalf("got (%d, %q, %v), want (%d, %q), bad=%v", n, a, err, tc.number, tc.agent, tc.bad)
			}
		})
	}
}
