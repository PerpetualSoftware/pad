package main

import (
	"fmt"
	"io"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// importedWorkspace is the bundle import's 201: the workspace plus the
// additive import_summary (TASK-3612). An older server, and the JSON import
// path, send no summary.
type importedWorkspace struct {
	models.Workspace
	ImportSummary *importSummary `json:"import_summary,omitempty"`
}

type importSummary struct {
	InBundle importCounts `json:"in_bundle"`
	Imported importCounts `json:"imported"`
}

type importCounts struct {
	Collections int `json:"collections"`
	Items       int `json:"items"`
	Comments    int `json:"comments"`
	Attachments int `json:"attachments"`
}

// printImportSummary prints what the bundle carried next to what arrived,
// and marks every kind whose count differs. It reports whether all of them
// matched.
func printImportSummary(w io.Writer, s *importSummary) bool {
	rows := []struct {
		name         string
		bundled, got int
	}{
		{"Collections", s.InBundle.Collections, s.Imported.Collections},
		{"Items", s.InBundle.Items, s.Imported.Items},
		{"Comments", s.InBundle.Comments, s.Imported.Comments},
		{"Attachments", s.InBundle.Attachments, s.Imported.Attachments},
	}
	fmt.Fprintf(w, "  %-12s %10s %10s\n", "", "in bundle", "imported")
	matched := true
	for _, r := range rows {
		mark := ""
		if r.got != r.bundled {
			mark = "  <- does not match"
			matched = false
		}
		fmt.Fprintf(w, "  %-12s %10d %10d%s\n", r.name, r.bundled, r.got, mark)
	}
	if !matched {
		fmt.Fprintln(w, "  The counts differ from the bundle. The workspace you exported from is unchanged,")
		fmt.Fprintln(w, "  so you can delete this copy, export again and re-import.")
	}
	return matched
}
