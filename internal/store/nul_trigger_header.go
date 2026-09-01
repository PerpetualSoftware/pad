package store

// nulTriggerMigrationHeader is the generated migration's preamble.
//
// It lives here rather than only in the .sql file so the generator and the file
// share one definition — the pin test compares the whole rendered text, header
// included, which is what makes a hand edit to the file detectable.
const nulTriggerMigrationHeader = `-- Layer B of the NUL invariant (DOC-2823 S2): the database owns the rule.
--
-- GENERATED from internal/store/nulcolumns.go. Do not edit by hand — run
--   GEN_NUL_TRIGGERS=1 go test ./internal/store/ -run TestGenerateNULTriggerMigration
-- and commit the result. TestNULTriggersMatchTheList fails if this file and the
-- Go list disagree.
--
-- WHY TRIGGERS AND NOT ONLY THE GO GUARD. S1's Layer A makes the invariant a
-- property of the BINARY. BUG-2813 is the window where that is not enough: an
-- older binary serving the same SQLite file has no such guard, and a rollback,
-- a staged rollout or a second instance writes rows the invariant forbids.
-- A trigger is enforced by the FILE, so it holds for every writer.
--
-- SQLITE ONLY. Postgres refuses a NUL in text natively (SQLSTATE 22021) and an
-- escape decoding to one in jsonb (22P05), so the database already owns the
-- rule there; adding triggers would duplicate an existing guarantee.
--
-- PREDICATE, measured on modernc.org/sqlite v1.57.0 / SQLite 3.53.3 in
-- TASK-2824 rather than assumed:
--   * instr(col, char(0)) finds a real NUL. length() does NOT — it C-truncates
--     at the NUL — so instr is the only builtin to trust here.
--   * json_tree DECODES the six-character escape, exposing a real NUL in both
--     the value and key columns, and a DOUBLED backslash stays literal text
--     with no false positive.
--   * a Go-bound real-NUL parameter reaches the trigger intact, which is what
--     makes BEFORE INSERT the right shape: an old binary's write is seen.
--
-- No DB-side repair is attempted, deliberately: the same measurement shows
-- SQLite's string functions disagree about NUL-bearing text, so transforming
-- such a value in SQL cannot be trusted. Repair is S3, in Go.

`
