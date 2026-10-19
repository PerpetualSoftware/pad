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

CREATE TRIGGER IF NOT EXISTS pad_nul152_item_verifications_agent_name_ins
BEFORE INSERT ON item_verifications
FOR EACH ROW WHEN NEW.agent_name IS NOT NULL AND (
			instr(NEW.agent_name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_verifications.agent_name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul152_item_verifications_agent_name_upd
BEFORE UPDATE OF agent_name ON item_verifications
FOR EACH ROW WHEN NEW.agent_name IS NOT NULL AND (
			instr(NEW.agent_name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_verifications.agent_name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul152_item_verifications_created_by_ins
BEFORE INSERT ON item_verifications
FOR EACH ROW WHEN NEW.created_by IS NOT NULL AND (
			instr(NEW.created_by, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_verifications.created_by must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul152_item_verifications_created_by_upd
BEFORE UPDATE OF created_by ON item_verifications
FOR EACH ROW WHEN NEW.created_by IS NOT NULL AND (
			instr(NEW.created_by, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_verifications.created_by must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul152_item_verifications_evidence_ins
BEFORE INSERT ON item_verifications
FOR EACH ROW WHEN NEW.evidence IS NOT NULL AND (
			instr(NEW.evidence, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_verifications.evidence must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul152_item_verifications_evidence_upd
BEFORE UPDATE OF evidence ON item_verifications
FOR EACH ROW WHEN NEW.evidence IS NOT NULL AND (
			instr(NEW.evidence, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_verifications.evidence must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul152_item_verifications_evidence_url_ins
BEFORE INSERT ON item_verifications
FOR EACH ROW WHEN NEW.evidence_url IS NOT NULL AND (
			instr(NEW.evidence_url, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_verifications.evidence_url must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul152_item_verifications_evidence_url_upd
BEFORE UPDATE OF evidence_url ON item_verifications
FOR EACH ROW WHEN NEW.evidence_url IS NOT NULL AND (
			instr(NEW.evidence_url, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_verifications.evidence_url must not contain a NUL');
END;

