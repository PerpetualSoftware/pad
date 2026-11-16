-- Legacy rows are converted by the Go hook in this migration transaction.
CREATE TABLE item_reminders_new (
 id TEXT PRIMARY KEY,
 workspace_id TEXT NOT NULL,
 item_id TEXT NOT NULL,
 remind_at TEXT NOT NULL,
 fired_at TEXT,
 acked_at TEXT,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 schedule_id TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
 FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE
);
INSERT INTO item_reminders_new (id,workspace_id,item_id,remind_at,fired_at,acked_at,created_at,updated_at,schedule_id)
 SELECT id,workspace_id,item_id,remind_at,fired_at,acked_at,created_at,updated_at,schedule_id FROM item_reminders;
DROP TABLE item_reminders;
ALTER TABLE item_reminders_new RENAME TO item_reminders;
CREATE INDEX idx_item_reminders_armed ON item_reminders(remind_at) WHERE fired_at IS NULL;
CREATE INDEX idx_item_reminders_unacked ON item_reminders(workspace_id,fired_at) WHERE fired_at IS NOT NULL AND acked_at IS NULL;
CREATE INDEX idx_item_reminders_item ON item_reminders(item_id,remind_at);
CREATE UNIQUE INDEX uq_item_reminders_schedule ON item_reminders(schedule_id) WHERE schedule_id IS NOT NULL;
