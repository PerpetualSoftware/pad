-- TASK-4537: an editor dismisses an ended schedule from Outstanding while its
-- item stays open. NULL until dismissed; only an ended schedule can be dismissed.
SET LOCAL lock_timeout = '5s';
ALTER TABLE item_schedules ADD COLUMN ended_dismissed_at TEXT;
ALTER TABLE item_schedules ADD CONSTRAINT item_schedules_dismissed_requires_end
    CHECK (ended_dismissed_at IS NULL OR ended_at IS NOT NULL);
