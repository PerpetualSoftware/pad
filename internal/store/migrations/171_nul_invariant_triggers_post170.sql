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

CREATE TRIGGER IF NOT EXISTS pad_nul171_milestone_children_reason_ins
BEFORE INSERT ON milestone_children
FOR EACH ROW WHEN NEW.reason IS NOT NULL AND (
			instr(NEW.reason, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: milestone_children.reason must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul171_milestone_children_reason_upd
BEFORE UPDATE OF reason ON milestone_children
FOR EACH ROW WHEN NEW.reason IS NOT NULL AND (
			instr(NEW.reason, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: milestone_children.reason must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul171_milestone_completion_evidence_done_value_ins
BEFORE INSERT ON milestone_completion_evidence
FOR EACH ROW WHEN NEW.done_value IS NOT NULL AND (
			instr(NEW.done_value, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: milestone_completion_evidence.done_value must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul171_milestone_completion_evidence_done_value_upd
BEFORE UPDATE OF done_value ON milestone_completion_evidence
FOR EACH ROW WHEN NEW.done_value IS NOT NULL AND (
			instr(NEW.done_value, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: milestone_completion_evidence.done_value must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul171_schedule_forecast_events_source_ins
BEFORE INSERT ON schedule_forecast_events
FOR EACH ROW WHEN NEW.source IS NOT NULL AND (
			instr(NEW.source, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_forecast_events.source must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul171_schedule_forecast_events_source_upd
BEFORE UPDATE OF source ON schedule_forecast_events
FOR EACH ROW WHEN NEW.source IS NOT NULL AND (
			instr(NEW.source, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_forecast_events.source must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul171_schedule_forecasts_source_ins
BEFORE INSERT ON schedule_forecasts
FOR EACH ROW WHEN NEW.source IS NOT NULL AND (
			instr(NEW.source, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_forecasts.source must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul171_schedule_forecasts_source_upd
BEFORE UPDATE OF source ON schedule_forecasts
FOR EACH ROW WHEN NEW.source IS NOT NULL AND (
			instr(NEW.source, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_forecasts.source must not contain a NUL');
END;

