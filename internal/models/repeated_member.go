package models

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/jsonscan"
)

// RepeatedMemberError refuses a request object that carries one member more
// than once (BUG-3219).
//
// encoding/json decodes a repeated member by decoding every occurrence into the
// same struct field. For a plain map member that MERGED the occurrences; for a
// member decoded from its raw bytes (a json.RawMessage capture, or a type with
// its own UnmarshalJSON that replaces its value) the LAST occurrence wins and
// the earlier ones are discarded without a word. BUG-3202 moved fields_patch
// and field_overrides from the first kind to the second, so a request that
// repeated one of them started answering 200 having dropped part of the write.
// Neither answer is right: a caller who sent two patches meant something the
// server cannot know, so the request is refused, naming the member.
type RepeatedMemberError struct {
	Member string
}

func (e *RepeatedMemberError) Error() string {
	return fmt.Sprintf("%s appears more than once in the request; send it once, carrying every key", e.Member)
}

// RefuseRepeatedMember returns a *RepeatedMemberError when the JSON object
// `data` carries more than one top-level member that encoding/json would
// decode into the struct field tagged `member`, and nil otherwise.
//
// "Would decode into" is encoding/json's own rule: an exact name match, or a
// case-insensitive one under its folding, so `fields_patch` followed by
// `FIELDS_PATCH` is a repeat. Matching only the exact spelling would leave that
// pair taking the last-wins path this exists to close.
//
// Call it AFTER the struct decode has succeeded. It does not validate: input
// that is not a JSON object, or that fails to scan, answers nil, so malformed
// input keeps the error the real decode gives it.
func RefuseRepeatedMember(data []byte, member string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	want := foldJSONName(member)
	seen := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil
		}
		if key == member || foldJSONName(key) == want {
			if seen {
				return &RepeatedMemberError{Member: member}
			}
			seen = true
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil
		}
	}
	return nil
}

// foldJSONName folds a member name the way encoding/json does when it matches
// an object key to a struct field. One implementation, jsonscan's, which the
// body gate's repeated-member check also uses, so the two cannot drift.
func foldJSONName(name string) string { return jsonscan.FoldName([]byte(name)) }
