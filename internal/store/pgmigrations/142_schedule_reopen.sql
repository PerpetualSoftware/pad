SET LOCAL lock_timeout = '5s';
ALTER TABLE schedule_changes DROP CONSTRAINT schedule_changes_change_type_check;
ALTER TABLE schedule_changes ADD CONSTRAINT schedule_changes_change_type_check
    CHECK (change_type IN ('create','update','delete','move','snooze','done','skip','missed_recorded','end','import','carry','ack','reopen'));

CREATE TABLE schedule_reopen_checkpoints (
    schedule_id TEXT PRIMARY KEY REFERENCES item_schedules(id) ON DELETE CASCADE,
    resolution_id TEXT NOT NULL,
    occurrence_key TEXT NOT NULL,
    operation TEXT NOT NULL,
    snapshot_version INTEGER NOT NULL,
    item_seq BIGINT NOT NULL,
    before_json TEXT,
    after_json TEXT,
    blocked_code TEXT,
    reopened_change_id TEXT
);
