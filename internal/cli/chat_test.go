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

func TestChatReadSeq(t *testing.T) {
	for _, tc := range []struct {
		name          string
		seqs          []int64
		readSeq, want int64
	}{
		{"empty", nil, 10, 0},
		{"distance two leaves one unseen", []int64{12, 13}, 10, 0},
		{"distance one is contiguous", []int64{11, 12}, 10, 12},
		{"next ascending", []int64{11, 12, 13}, 10, 13},
		{"next descending", []int64{13, 12, 11}, 10, 13},
		{"overlap ascending", []int64{8, 9, 10, 11}, 10, 11},
		{"overlap descending", []int64{11, 10, 9, 8}, 10, 11},
		{"gap ascending", []int64{31, 32, 33}, 0, 0},
		{"gap descending", []int64{33, 32, 31}, 0, 0},
		{"since skips unread", []int64{41, 42}, 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := make([]struct {
				Seq int64 `json:"seq"`
			}, len(tc.seqs))
			for i, seq := range tc.seqs {
				messages[i].Seq = seq
			}
			if got := ChatReadSeq(messages, tc.readSeq); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
