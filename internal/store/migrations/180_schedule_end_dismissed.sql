-- TASK-4537: an editor dismisses an ended schedule from Outstanding while its
-- item stays open. NULL until dismissed; only an ended schedule can be dismissed.
ALTER TABLE item_schedules ADD COLUMN ended_dismissed_at TEXT CHECK (ended_dismissed_at IS NULL OR ended_at IS NOT NULL);

-- Keep historical endings out of Outstanding after upgrade.
UPDATE item_schedules SET ended_dismissed_at = ended_at WHERE ended_at IS NOT NULL;
