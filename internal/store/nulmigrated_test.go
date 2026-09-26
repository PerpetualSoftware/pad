package store

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestMigratedTablesCoversTheExport pins MigratedTables against the export's
// own SHAPE rather than against a reading of its SQL.
//
// The preflight refuses only on tables in that set, so a section added to
// WorkspaceExport without a matching entry would make the preflight quietly
// stop guarding a table the migration now copies — a miss, in the direction
// that ends in a half-finished migration. A source-regex over ExportWorkspace's
// queries would not catch it either: this codebase has already learned that
// multi-line and Sprintf-composed SQL are invisible to any such instrument
// (TASK-2825).
//
// Reflection over the struct is the maintainable version of the question: every
// slice section, plus the workspace row itself, must map to a listed table.
func TestMigratedTablesCoversTheExport(t *testing.T) {
	t.Parallel()
	// Section field name -> the table it is read from. Adding a section to
	// WorkspaceExport fails the loop below until it is named here AND in
	// MigratedTables, which is the point.
	sectionTables := map[string]string{
		"Collections":  "collections",
		"Items":        "items",
		"Comments":     "comments",
		"ItemLinks":    "item_links",
		"ItemVersions": "item_versions",
		"Reminders":    "item_reminders",
	}

	migrated := MigratedTables()

	// The workspace row is carried by the Workspace field rather than a slice,
	// and ImportWorkspace writes it through CreateWorkspace.
	if !migrated["workspaces"] {
		t.Error("workspaces is not listed, but ImportWorkspace creates the workspace row")
	}

	typ := reflect.TypeOf(models.WorkspaceExport{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Slice {
			continue
		}
		table, known := sectionTables[f.Name]
		if !known {
			t.Errorf("WorkspaceExport has a section %q that this test does not map to a table. The "+
				"migration now copies something new: add it here AND to MigratedTables, or the "+
				"preflight stops guarding it.", f.Name)
			continue
		}
		if !migrated[table] {
			t.Errorf("export section %q reads %q, which MigratedTables does not list — the preflight "+
				"would not refuse on a NUL there", f.Name, table)
		}
	}

	// And the set contains nothing SPURIOUS, which would be an over-refusal
	// rather than a miss but is still wrong.
	for table := range migrated {
		if table == "workspaces" {
			continue
		}
		found := false
		for _, mapped := range sectionTables {
			if mapped == table {
				found = true
			}
		}
		if !found {
			t.Errorf("MigratedTables lists %q, which no export section reads; the preflight would "+
				"refuse a migration over a table it does not copy", table)
		}
	}
}
