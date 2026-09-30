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
}

// ItemVersionDiff is the change one item version row records (PLAN-2348 U2):
// the bodies before and after the write that made the row, so a History card
// can show that edit's own diff rather than one against today's body.
type ItemVersionDiff struct {
	Version Version `json:"version"`
	Before  string  `json:"before"`
	After   string  `json:"after"`
}
