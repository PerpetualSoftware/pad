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

CREATE TRIGGER IF NOT EXISTS pad_nul140_idempotency_keys_base_body_ins
BEFORE INSERT ON idempotency_keys
FOR EACH ROW WHEN NEW.base_body IS NOT NULL AND (
			instr(NEW.base_body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: idempotency_keys.base_body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul140_idempotency_keys_base_body_upd
BEFORE UPDATE OF base_body ON idempotency_keys
FOR EACH ROW WHEN NEW.base_body IS NOT NULL AND (
			instr(NEW.base_body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: idempotency_keys.base_body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul140_idempotency_keys_key_ins
BEFORE INSERT ON idempotency_keys
FOR EACH ROW WHEN NEW.key IS NOT NULL AND (
			instr(NEW.key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: idempotency_keys.key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul140_idempotency_keys_key_upd
BEFORE UPDATE OF key ON idempotency_keys
FOR EACH ROW WHEN NEW.key IS NOT NULL AND (
			instr(NEW.key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: idempotency_keys.key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul140_idempotency_keys_response_ins
BEFORE INSERT ON idempotency_keys
FOR EACH ROW WHEN NEW.response IS NOT NULL AND (
			instr(NEW.response, char(0)) > 0
			OR (json_valid(NEW.response) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.response)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: idempotency_keys.response must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul140_idempotency_keys_response_upd
BEFORE UPDATE OF response ON idempotency_keys
FOR EACH ROW WHEN NEW.response IS NOT NULL AND (
			instr(NEW.response, char(0)) > 0
			OR (json_valid(NEW.response) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.response)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: idempotency_keys.response must not contain a NUL');
END;

