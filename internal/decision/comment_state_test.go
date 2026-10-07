package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The comment subject's state is exactly title, collection and the comment,
// hashed over the bytes that are sent.
func TestBuildCommentState_ShapeAndHash(t *testing.T) {
	st, err := BuildCommentState("Set up staging", "tasks", "the password is hunter2")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"title":"Set up staging","collection":"tasks","comment":"the password is hunter2"}`
	if string(st.Bytes) != want {
		t.Fatalf("state = %s\nwant    %s", st.Bytes, want)
	}
	sum := sha256.Sum256(st.Bytes)
	if st.Hash != hex.EncodeToString(sum[:]) || st.Truncated {
		t.Fatalf("hash/truncated = %s/%v", st.Hash, st.Truncated)
	}
}

// A comment is clipped at the BODY bound, not the trail's 2,000 runes: a
// credential at rune 2,786 of a long status update must reach the provider.
func TestBuildCommentState_ClipsAtTheBodyBound(t *testing.T) {
	long := strings.Repeat("é", maxStateCommentRunes+800) + " SECRET"
	st, err := BuildCommentState("t", "tasks", long)
	if err != nil {
		t.Fatal(err)
	}
	if st.Truncated || !strings.Contains(string(st.Bytes), "SECRET") {
		t.Fatalf("a %d-rune comment was clipped (truncated=%v)", maxStateCommentRunes+807, st.Truncated)
	}
	over := strings.Repeat("a", maxCommentSubjectRunes+1)
	st, err = BuildCommentState("t", "tasks", over)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Truncated {
		t.Fatal("a comment over the bound was not reported truncated")
	}
}
