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

CREATE TRIGGER IF NOT EXISTS pad_nul129_item_builtin_origin_seed_content_ins
BEFORE INSERT ON item_builtin_origin
FOR EACH ROW WHEN NEW.seed_content IS NOT NULL AND (
			instr(NEW.seed_content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_builtin_origin.seed_content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul129_item_builtin_origin_seed_content_upd
BEFORE UPDATE OF seed_content ON item_builtin_origin
FOR EACH ROW WHEN NEW.seed_content IS NOT NULL AND (
			instr(NEW.seed_content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_builtin_origin.seed_content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul129_item_builtin_origin_seed_fields_ins
BEFORE INSERT ON item_builtin_origin
FOR EACH ROW WHEN NEW.seed_fields IS NOT NULL AND (
			instr(NEW.seed_fields, char(0)) > 0
			OR (json_valid(NEW.seed_fields) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.seed_fields)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_builtin_origin.seed_fields must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul129_item_builtin_origin_seed_fields_upd
BEFORE UPDATE OF seed_fields ON item_builtin_origin
FOR EACH ROW WHEN NEW.seed_fields IS NOT NULL AND (
			instr(NEW.seed_fields, char(0)) > 0
			OR (json_valid(NEW.seed_fields) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.seed_fields)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_builtin_origin.seed_fields must not contain a NUL');
END;

