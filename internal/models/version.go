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
}
