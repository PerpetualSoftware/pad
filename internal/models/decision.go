package models

import "encoding/json"

// ItemDecision is one stored answer to one typed question about an item
// (PLAN-3114 unit 2, TASK-3117).
//
// Answer is the provider's answer as the decision package marshalled it; its
// shape depends on Kind (choice / score / noul) and is documented on
// decision.Answer. Confidence is nil where the primitive returns none — a Noul
// answer carries no confidence, and a zero would read as "certainly not".
//
// Current is computed at READ time, never stored: it is true when StateHash
// equals the hash of the item's state as it stands now. An answer computed
// before a later edit or comment is kept for the audit but is not current.
type ItemDecision struct {
	ID             string          `json:"id"`
	ItemID         string          `json:"item_id"`
	QuestionSet    string          `json:"question_set"`
	QuestionKey    string          `json:"question_key"`
	Kind           string          `json:"kind"`
	Answer         json.RawMessage `json:"answer"`
	Confidence     *float64        `json:"confidence"`
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	StateHash      string          `json:"state_hash"`
	ItemSeq        int64           `json:"item_seq"`
	StateTruncated bool            `json:"state_truncated,omitempty"`
	EvaluatedAt    string          `json:"evaluated_at"`
	Current        bool            `json:"current"`
}
