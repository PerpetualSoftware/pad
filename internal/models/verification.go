package models

// Verification evidence tied to a revision (TASK-3617). One row is one verdict
// on one acceptance criterion of the item that carries the criteria. Rows are
// append-only; State and PossiblyStale are computed at read time and never
// stored (internal/verification).

// Verdicts a verification may carry: the verify playbook's three.
const (
	VerdictPass     = "pass"
	VerdictFail     = "fail"
	VerdictCantTell = "cant_tell"
)

// ValidVerdict reports whether v is one of the three verdicts.
func ValidVerdict(v string) bool {
	return v == VerdictPass || v == VerdictFail || v == VerdictCantTell
}

// Verification states, in the order they are decided.
const (
	// The criterion id is no longer in the item's acceptance criteria, or is
	// a placeholder again.
	VerificationCriterionRemoved = "criterion_removed"
	// The criterion's text changed since the verdict, so the evidence was
	// about different words.
	VerificationCriterionChanged = "criterion_changed"
	// A newer verdict exists for the same criterion.
	VerificationSuperseded = "superseded"
	VerificationCurrent    = "current"
)

// ItemVerification is one stored verdict.
type ItemVerification struct {
	ID            string `json:"id"`
	ItemID        string `json:"item_id"`
	CriterionID   string `json:"criterion"`
	CriterionHash string `json:"criterion_hash"`
	SpecSeq       int64  `json:"spec_seq"`
	CodeRevision  string `json:"code_revision,omitempty"`
	Verdict       string `json:"verdict"`
	Evidence      string `json:"evidence,omitempty"`
	EvidenceURL   string `json:"evidence_url,omitempty"`
	// ViaItemID is the item that did the work. ViaRef is its ref, filled by
	// the server when the caller may see that item, and left empty otherwise.
	ViaItemID string `json:"-"`
	ViaRef    string `json:"via,omitempty"`
	RunID     string `json:"run_id"`
	CreatedBy string `json:"created_by,omitempty"`
	UserID    string `json:"-"`
	AgentName string `json:"agent_name,omitempty"`
	CreatedAt string `json:"created_at"`

	State         string                 `json:"state"`
	PossiblyStale *VerificationStaleHint `json:"possibly_stale,omitempty"`
}

// VerificationStaleHint says a linked PR merged after the verdict's code
// revision was recorded. It is advisory: it names its evidence and never
// changes State (TASK-3617 ruling (a)).
type VerificationStaleHint struct {
	Via       string `json:"via"`
	MergedSHA string `json:"merged_sha"`
	MergedAt  string `json:"merged_at,omitempty"`
}
