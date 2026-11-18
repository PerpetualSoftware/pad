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

CREATE TRIGGER IF NOT EXISTS pad_nul187_workspace_slug_claims_slug_ins
BEFORE INSERT ON workspace_slug_claims
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspace_slug_claims.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul187_workspace_slug_claims_slug_upd
BEFORE UPDATE OF slug ON workspace_slug_claims
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspace_slug_claims.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul187_workspace_slug_claims_workspace_id_ins
BEFORE INSERT ON workspace_slug_claims
FOR EACH ROW WHEN NEW.workspace_id IS NOT NULL AND (
			instr(NEW.workspace_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspace_slug_claims.workspace_id must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul187_workspace_slug_claims_workspace_id_upd
BEFORE UPDATE OF workspace_id ON workspace_slug_claims
FOR EACH ROW WHEN NEW.workspace_id IS NOT NULL AND (
			instr(NEW.workspace_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspace_slug_claims.workspace_id must not contain a NUL');
END;

