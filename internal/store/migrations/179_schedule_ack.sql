-- Preserve the journal and its guards while adding acknowledgement.
PRAGMA foreign_keys = OFF;

CREATE TABLE schedule_changes_new (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    item_id TEXT NOT NULL,
    schedule_id TEXT NOT NULL,
    occurrence_key TEXT,
    change_type TEXT NOT NULL CHECK (change_type IN ('create','update','delete','move','snooze','done','skip','missed_recorded','end','import','carry','ack')),
    role TEXT CHECK (role IN ('target','earliest','deadline','occupied_until')),
    before_json TEXT,
    after_json TEXT,
    actor TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    reason_source TEXT NOT NULL DEFAULT 'system' CHECK (reason_source IN ('system','user')),
    root_change_id TEXT NOT NULL,
    parent_change_id TEXT,
    recorded_at TEXT NOT NULL
);

INSERT INTO schedule_changes_new (id,workspace_id,item_id,schedule_id,occurrence_key,change_type,role,before_json,after_json,actor,reason,reason_source,root_change_id,parent_change_id,recorded_at)
SELECT id,workspace_id,item_id,schedule_id,occurrence_key,change_type,role,before_json,after_json,actor,reason,reason_source,root_change_id,parent_change_id,recorded_at FROM schedule_changes;
DROP TABLE schedule_changes;
ALTER TABLE schedule_changes_new RENAME TO schedule_changes;
CREATE INDEX idx_schedule_changes_schedule ON schedule_changes(schedule_id, recorded_at);
CREATE INDEX idx_schedule_changes_item ON schedule_changes(workspace_id, item_id, recorded_at);

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_actor_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.actor IS NOT NULL AND (
			instr(NEW.actor, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.actor must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_actor_upd
BEFORE UPDATE OF actor ON schedule_changes
FOR EACH ROW WHEN NEW.actor IS NOT NULL AND (
			instr(NEW.actor, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.actor must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_after_json_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.after_json IS NOT NULL AND (
			instr(NEW.after_json, char(0)) > 0
			OR (json_valid(NEW.after_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.after_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.after_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_after_json_upd
BEFORE UPDATE OF after_json ON schedule_changes
FOR EACH ROW WHEN NEW.after_json IS NOT NULL AND (
			instr(NEW.after_json, char(0)) > 0
			OR (json_valid(NEW.after_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.after_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.after_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_before_json_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.before_json IS NOT NULL AND (
			instr(NEW.before_json, char(0)) > 0
			OR (json_valid(NEW.before_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.before_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.before_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_before_json_upd
BEFORE UPDATE OF before_json ON schedule_changes
FOR EACH ROW WHEN NEW.before_json IS NOT NULL AND (
			instr(NEW.before_json, char(0)) > 0
			OR (json_valid(NEW.before_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.before_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.before_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_occurrence_key_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.occurrence_key IS NOT NULL AND (
			instr(NEW.occurrence_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.occurrence_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_occurrence_key_upd
BEFORE UPDATE OF occurrence_key ON schedule_changes
FOR EACH ROW WHEN NEW.occurrence_key IS NOT NULL AND (
			instr(NEW.occurrence_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.occurrence_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_reason_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.reason IS NOT NULL AND (
			instr(NEW.reason, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.reason must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_reason_upd
BEFORE UPDATE OF reason ON schedule_changes
FOR EACH ROW WHEN NEW.reason IS NOT NULL AND (
			instr(NEW.reason, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.reason must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_role_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.role IS NOT NULL AND (
			instr(NEW.role, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.role must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_role_upd
BEFORE UPDATE OF role ON schedule_changes
FOR EACH ROW WHEN NEW.role IS NOT NULL AND (
			instr(NEW.role, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.role must not contain a NUL');
END;

PRAGMA foreign_keys = ON;
