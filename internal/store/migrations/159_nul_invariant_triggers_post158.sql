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

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_members_agent_key_ins
BEFORE INSERT ON conversation_members
FOR EACH ROW WHEN NEW.agent_key IS NOT NULL AND (
			instr(NEW.agent_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_members.agent_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_members_agent_key_upd
BEFORE UPDATE OF agent_key ON conversation_members
FOR EACH ROW WHEN NEW.agent_key IS NOT NULL AND (
			instr(NEW.agent_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_members.agent_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_author_agent_key_ins
BEFORE INSERT ON conversation_messages
FOR EACH ROW WHEN NEW.author_agent_key IS NOT NULL AND (
			instr(NEW.author_agent_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.author_agent_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_author_agent_key_upd
BEFORE UPDATE OF author_agent_key ON conversation_messages
FOR EACH ROW WHEN NEW.author_agent_key IS NOT NULL AND (
			instr(NEW.author_agent_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.author_agent_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_body_ins
BEFORE INSERT ON conversation_messages
FOR EACH ROW WHEN NEW.body IS NOT NULL AND (
			instr(NEW.body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_body_upd
BEFORE UPDATE OF body ON conversation_messages
FOR EACH ROW WHEN NEW.body IS NOT NULL AND (
			instr(NEW.body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_deleted_by_agent_key_ins
BEFORE INSERT ON conversation_messages
FOR EACH ROW WHEN NEW.deleted_by_agent_key IS NOT NULL AND (
			instr(NEW.deleted_by_agent_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.deleted_by_agent_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_deleted_by_agent_key_upd
BEFORE UPDATE OF deleted_by_agent_key ON conversation_messages
FOR EACH ROW WHEN NEW.deleted_by_agent_key IS NOT NULL AND (
			instr(NEW.deleted_by_agent_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.deleted_by_agent_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_op_key_ins
BEFORE INSERT ON conversation_messages
FOR EACH ROW WHEN NEW.op_key IS NOT NULL AND (
			instr(NEW.op_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.op_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_op_key_upd
BEFORE UPDATE OF op_key ON conversation_messages
FOR EACH ROW WHEN NEW.op_key IS NOT NULL AND (
			instr(NEW.op_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.op_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_source_ins
BEFORE INSERT ON conversation_messages
FOR EACH ROW WHEN NEW.source IS NOT NULL AND (
			instr(NEW.source, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.source must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversation_messages_source_upd
BEFORE UPDATE OF source ON conversation_messages
FOR EACH ROW WHEN NEW.source IS NOT NULL AND (
			instr(NEW.source, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversation_messages.source must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversations_dm_key_ins
BEFORE INSERT ON conversations
FOR EACH ROW WHEN NEW.dm_key IS NOT NULL AND (
			instr(NEW.dm_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversations.dm_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversations_dm_key_upd
BEFORE UPDATE OF dm_key ON conversations
FOR EACH ROW WHEN NEW.dm_key IS NOT NULL AND (
			instr(NEW.dm_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversations.dm_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversations_title_ins
BEFORE INSERT ON conversations
FOR EACH ROW WHEN NEW.title IS NOT NULL AND (
			instr(NEW.title, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversations.title must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul159_conversations_title_upd
BEFORE UPDATE OF title ON conversations
FOR EACH ROW WHEN NEW.title IS NOT NULL AND (
			instr(NEW.title, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: conversations.title must not contain a NUL');
END;

