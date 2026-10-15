package models

import "time"

type Version struct {
	ID            string    `json:"id"`
	DocumentID    string    `json:"document_id"`
	Content       string    `json:"content"`
	ChangeSummary string    `json:"change_summary,omitempty"`
	CreatedBy     string    `json:"created_by"`
	Source        string    `json:"source"`
	IsDiff        bool      `json:"is_diff"`
	CreatedAt     time.Time `json:"created_at"`

	// PLAN-2348 U2 (item versions only). UserID is who wrote the row;
	// ActorName is that user's name, joined on read. Both are empty for rows
	// written before U2 (the column existed, unwritten, since migration 012)
	// and for system rows (recovery).
	UserID    string `json:"user_id,omitempty"`
	ActorName string `json:"actor_name,omitempty"`
	// LinesAdded / LinesRemoved count the change this row's WRITE recorded:
	// empty → body for a create row, body → the replacing body for an update
	// row. nil means unknown (every row written before migration 103).
	LinesAdded   *int `json:"lines_added,omitempty"`
	LinesRemoved *int `json:"lines_removed,omitempty"`
	// IsCreate marks a row written by item create, which holds the body AS
	// CREATED; an update row holds the body BEFORE its edit.
	IsCreate bool `json:"is_create,omitempty"`
	// Imported marks a row a workspace import wrote (BUG-3379): its
	// created_by and source came from the export, unverified here.
	Imported bool `json:"imported,omitempty"`

	// ViaApp and ViaAppName name the app whose write made this version,
	// absent for a person's or agent's (SPEC-6 U9c, TASK-3413). The name
	// is the app's bot display name, else its origin, and survives
	// uninstall. The server fills both on read; no scan sets them.
	ViaApp     string `json:"via_app,omitempty"`
	ViaAppName string `json:"via_app_name,omitempty"`

	// Unreconstructable marks a resolved version whose body the reverse-patch
	// walk could not rebuild, or could not verify: never a body to restore or
	// diff (BUG-3815, BUG-3820). Set only by the resolving reads, and served
	// so a client can tell. UnreconstructableReason says which:
	//   - "patch_failed": the patch did not apply; Content holds a
	//     "[patch error: ...]" placeholder.
	//   - "hash_mismatch": the patch applied, but not to the body its
	//     body_sha256 records; Content holds the placeholder.
	//   - "unverified": the row has no body_sha256 and lies below a failed or
	//     mismatched row, so it was resolved from a body that may not be its
	//     base. Content holds that best-effort body, which may be wrong.
	// A real body that happens to start with the placeholder text is not one.
	Unreconstructable       bool   `json:"unreconstructable,omitempty"`
	UnreconstructableReason string `json:"unreconstructable_reason,omitempty"`

	// BodySHA256 is the hex sha256 of the body this row stands for, or ""
	// for a row written before BUG-3820 (or imported without one). Internal:
	// the walk checks it; it is not served.
	BodySHA256 string `json:"-"`
}

// ItemVersionDiff is the change one item version row records (PLAN-2348 U2):
// the bodies before and after the write that made the row, so a History card
// can show that edit's own diff rather than one against today's body.
type ItemVersionDiff struct {
	Version Version `json:"version"`
	Before  string  `json:"before"`
	After   string  `json:"after"`
}
