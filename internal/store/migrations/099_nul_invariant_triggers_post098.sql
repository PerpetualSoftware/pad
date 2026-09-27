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

CREATE TRIGGER IF NOT EXISTS pad_nul099_user_workspace_tabs_last_route_ins
BEFORE INSERT ON user_workspace_tabs
FOR EACH ROW WHEN NEW.last_route IS NOT NULL AND (
			instr(NEW.last_route, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: user_workspace_tabs.last_route must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul099_user_workspace_tabs_last_route_upd
BEFORE UPDATE OF last_route ON user_workspace_tabs
FOR EACH ROW WHEN NEW.last_route IS NOT NULL AND (
			instr(NEW.last_route, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: user_workspace_tabs.last_route must not contain a NUL');
END;

