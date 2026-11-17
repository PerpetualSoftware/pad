-- Layer B of the NUL invariant (DOC-2823 S2), for columns added after 084.
--
-- GENERATED from internal/store/nulcolumns.go. Do not edit by hand — run
--   GEN_NUL_TRIGGERS=1 go test ./internal/store/ -run TestGenerateNULTriggerMigration
-- and commit the result. TestNULTriggersMatchTheList fails if this file and the
-- Go list disagree.
--
-- WHY A SEPARATE FILE (BUG-3108). A trigger can only follow its column. 084
-- runs before these columns exist, and SQLite re-parses every trigger during
-- an ALTER TABLE ... RENAME, so a trigger there naming a later column breaks
-- the next table rebuild. The predicate, the marker and the SQLite-only scope
-- are those of 084; its header carries the measurements.

CREATE TRIGGER IF NOT EXISTS pad_nul185_item_entries_body_ins
BEFORE INSERT ON item_entries
FOR EACH ROW WHEN NEW.body IS NOT NULL AND (
			instr(NEW.body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_entries.body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul185_item_entries_body_upd
BEFORE UPDATE OF body ON item_entries
FOR EACH ROW WHEN NEW.body IS NOT NULL AND (
			instr(NEW.body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_entries.body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul185_item_entries_entry_id_ins
BEFORE INSERT ON item_entries
FOR EACH ROW WHEN NEW.entry_id IS NOT NULL AND (
			instr(NEW.entry_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_entries.entry_id must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul185_item_entries_entry_id_upd
BEFORE UPDATE OF entry_id ON item_entries
FOR EACH ROW WHEN NEW.entry_id IS NOT NULL AND (
			instr(NEW.entry_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_entries.entry_id must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul185_item_entries_head_ins
BEFORE INSERT ON item_entries
FOR EACH ROW WHEN NEW.head IS NOT NULL AND (
			instr(NEW.head, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_entries.head must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul185_item_entries_head_upd
BEFORE UPDATE OF head ON item_entries
FOR EACH ROW WHEN NEW.head IS NOT NULL AND (
			instr(NEW.head, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_entries.head must not contain a NUL');
END;

