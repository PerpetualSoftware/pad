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
-- are 084's; its header carries the measurements.

CREATE TRIGGER IF NOT EXISTS pad_nul138_item_patches_edits_ins
BEFORE INSERT ON item_patches
FOR EACH ROW WHEN NEW.edits IS NOT NULL AND (
			instr(NEW.edits, char(0)) > 0
			OR (json_valid(NEW.edits) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.edits)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_patches.edits must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul138_item_patches_edits_upd
BEFORE UPDATE OF edits ON item_patches
FOR EACH ROW WHEN NEW.edits IS NOT NULL AND (
			instr(NEW.edits, char(0)) > 0
			OR (json_valid(NEW.edits) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.edits)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_patches.edits must not contain a NUL');
END;

