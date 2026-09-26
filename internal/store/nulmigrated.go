package store

// MigratedTables names the tables `pad db migrate-to-pg` actually copies.
//
// The migration is application-level: it walks workspaces and runs
// ExportWorkspace / ImportWorkspace on each. That reads seven tables and no
// others — the command's own help says users, platform settings and auth data
// are NOT migrated — so a NUL in users.name, platform_settings.value,
// sessions.user_agent or any oauth table cannot break it.
//
// The preflight uses this to decide what to REFUSE on, not what to REPORT
// (codex round 9). Scanning everything is right: `pad db scan-nul` is about the
// database, and an operator should hear about every affected row. Blocking a
// migration over a row it will never touch is not — it demands the operator
// rewrite content that has nothing to do with the copy they asked for.
//
// TestMigratedTablesCoversTheExport pins this against models.WorkspaceExport's
// own shape, so a new export section fails here rather than silently making the
// preflight miss a table.
//
// KNOWN RESIDUAL, stated because it is the same class of over-refusal one size
// smaller: the export also skips SOFT-DELETED collections and items
// (`deleted_at IS NULL`), and this filter is per-table, not per-row. A NUL in a
// soft-deleted item still blocks a migration that would not have carried it.
// Narrowing that needs a per-row deleted_at check at every candidate, which is
// more machinery than the remaining over-refusal costs — the operator's way out
// is the same single repair command either way.
func MigratedTables() map[string]bool {
	return map[string]bool{
		"workspaces":    true,
		"collections":   true,
		"items":         true,
		"comments":      true,
		"item_links":    true,
		"item_versions": true,
		// item_reminders joined the export in IDEA-2641. Its columns are all
		// machine-produced (ids, a re-parsed RFC3339 instant, server clocks),
		// so a NUL here is not reachable through any writer — it is listed for
		// COVERAGE, not because the preflight expects to find anything. The
		// alternative is a table the migration copies and the preflight does
		// not know about, which is the exact gap this list exists to close.
		"item_reminders": true,
	}
}
