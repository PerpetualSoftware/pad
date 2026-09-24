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

CREATE TRIGGER IF NOT EXISTS pad_nul094_item_decisions_model_ins
BEFORE INSERT ON item_decisions
FOR EACH ROW WHEN NEW.model IS NOT NULL AND (
			instr(NEW.model, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_decisions.model must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul094_item_decisions_model_upd
BEFORE UPDATE OF model ON item_decisions
FOR EACH ROW WHEN NEW.model IS NOT NULL AND (
			instr(NEW.model, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_decisions.model must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul094_items_lease_holder_ins
BEFORE INSERT ON items
FOR EACH ROW WHEN NEW.lease_holder IS NOT NULL AND (
			instr(NEW.lease_holder, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.lease_holder must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul094_items_lease_holder_upd
BEFORE UPDATE OF lease_holder ON items
FOR EACH ROW WHEN NEW.lease_holder IS NOT NULL AND (
			instr(NEW.lease_holder, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.lease_holder must not contain a NUL');
END;

