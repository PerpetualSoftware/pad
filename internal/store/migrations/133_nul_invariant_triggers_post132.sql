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

CREATE TRIGGER IF NOT EXISTS pad_nul133_cli_auth_sessions_requester_ip_ins
BEFORE INSERT ON cli_auth_sessions
FOR EACH ROW WHEN NEW.requester_ip IS NOT NULL AND (
			instr(NEW.requester_ip, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: cli_auth_sessions.requester_ip must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul133_cli_auth_sessions_requester_ip_upd
BEFORE UPDATE OF requester_ip ON cli_auth_sessions
FOR EACH ROW WHEN NEW.requester_ip IS NOT NULL AND (
			instr(NEW.requester_ip, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: cli_auth_sessions.requester_ip must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul133_cli_auth_sessions_requester_user_agent_ins
BEFORE INSERT ON cli_auth_sessions
FOR EACH ROW WHEN NEW.requester_user_agent IS NOT NULL AND (
			instr(NEW.requester_user_agent, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: cli_auth_sessions.requester_user_agent must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul133_cli_auth_sessions_requester_user_agent_upd
BEFORE UPDATE OF requester_user_agent ON cli_auth_sessions
FOR EACH ROW WHEN NEW.requester_user_agent IS NOT NULL AND (
			instr(NEW.requester_user_agent, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: cli_auth_sessions.requester_user_agent must not contain a NUL');
END;

