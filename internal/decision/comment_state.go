package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// TASK-3119 U2: a comment is asked about on its own (the "comment subject"),
// with its item's title and collection for context, instead of riding in
// every item state as part of a trail that was clipped at 2,000 runes per
// comment. This is that state's ONE serialisation, shared by the eval
// harness (which measured it before it shipped) and the runner.

// maxCommentSubjectRunes bounds the comment. It is the body's bound, not the
// trail's: a comment asked about alone has the request to itself. Census at
// the time (the :7777 DB): 20 of 23,618 comments exceed it.
const maxCommentSubjectRunes = maxStateBodyRunes

// CommentState is the state a comment subject is asked about. Like
// ItemState, part of the hash: a change here re-asks every comment.
type CommentState struct {
	Title      string `json:"title"`
	Collection string `json:"collection"`
	Comment    string `json:"comment"`
}

// BuildCommentState builds and serialises one comment's state.
func BuildCommentState(itemTitle, collectionSlug, body string) (BuiltState, error) {
	clipped, truncated := clipRunes(body, maxCommentSubjectRunes)
	b, err := json.Marshal(CommentState{Title: itemTitle, Collection: collectionSlug, Comment: clipped})
	if err != nil {
		return BuiltState{}, fmt.Errorf("decision comment state: marshal: %w", err)
	}
	sum := sha256.Sum256(b)
	return BuiltState{Bytes: b, Hash: hex.EncodeToString(sum[:]), Truncated: truncated}, nil
}
