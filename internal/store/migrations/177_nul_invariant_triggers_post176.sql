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

CREATE TRIGGER IF NOT EXISTS pad_nul177_calendar_feeds_name_ins
BEFORE INSERT ON calendar_feeds
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: calendar_feeds.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul177_calendar_feeds_name_upd
BEFORE UPDATE OF name ON calendar_feeds
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: calendar_feeds.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul177_calendar_feeds_workspace_ids_ins
BEFORE INSERT ON calendar_feeds
FOR EACH ROW WHEN NEW.workspace_ids IS NOT NULL AND (
			instr(NEW.workspace_ids, char(0)) > 0
			OR (json_valid(NEW.workspace_ids) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.workspace_ids)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: calendar_feeds.workspace_ids must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul177_calendar_feeds_workspace_ids_upd
BEFORE UPDATE OF workspace_ids ON calendar_feeds
FOR EACH ROW WHEN NEW.workspace_ids IS NOT NULL AND (
			instr(NEW.workspace_ids, char(0)) > 0
			OR (json_valid(NEW.workspace_ids) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.workspace_ids)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: calendar_feeds.workspace_ids must not contain a NUL');
END;

