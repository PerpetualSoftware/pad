-- Resolved occurrences keep the role offsets in force when they were resolved
-- (TASK-4714). The roles_ prefix keeps the unqualified item_schedules
-- references in correlated subqueries over schedule_occurrences unambiguous.
SET LOCAL lock_timeout = '5s';
ALTER TABLE schedule_occurrences
    ADD COLUMN IF NOT EXISTS roles_target_days INTEGER,
    ADD COLUMN IF NOT EXISTS roles_target_secs INTEGER,
    ADD COLUMN IF NOT EXISTS roles_earliest_days INTEGER,
    ADD COLUMN IF NOT EXISTS roles_earliest_secs INTEGER,
    ADD COLUMN IF NOT EXISTS roles_deadline_days INTEGER,
    ADD COLUMN IF NOT EXISTS roles_deadline_secs INTEGER,
    ADD COLUMN IF NOT EXISTS roles_deadline_behavior TEXT,
    ADD COLUMN IF NOT EXISTS roles_occupied_days INTEGER,
    ADD COLUMN IF NOT EXISTS roles_occupied_secs INTEGER;
-- Backfill in one set-based statement: every resolved row that is not moved
-- takes the offsets of its schedule now, which is what it displays today.
UPDATE schedule_occurrences SET
    (roles_target_days, roles_target_secs, roles_earliest_days, roles_earliest_secs,
     roles_deadline_days, roles_deadline_secs, roles_deadline_behavior,
     roles_occupied_days, roles_occupied_secs) =
    (SELECT sc.target_days, sc.target_secs, sc.earliest_days, sc.earliest_secs,
            sc.deadline_days, sc.deadline_secs, sc.deadline_behavior,
            sc.occupied_days, sc.occupied_secs
     FROM item_schedules sc WHERE sc.id = schedule_occurrences.schedule_id)
WHERE state <> 'moved';
ALTER TABLE schedule_occurrences
    ADD CONSTRAINT schedule_occurrences_roles_target_check CHECK ((roles_target_days IS NULL) = (roles_target_secs IS NULL)),
    ADD CONSTRAINT schedule_occurrences_roles_earliest_check CHECK ((roles_earliest_days IS NULL) = (roles_earliest_secs IS NULL)),
    ADD CONSTRAINT schedule_occurrences_roles_deadline_check CHECK ((roles_deadline_days IS NULL) = (roles_deadline_secs IS NULL)),
    ADD CONSTRAINT schedule_occurrences_roles_deadline_behavior_check CHECK (
        (roles_deadline_behavior IS NULL OR roles_deadline_behavior IN ('lapses','stays_owed'))
        AND ((roles_deadline_days IS NULL) = (roles_deadline_behavior IS NULL))),
    ADD CONSTRAINT schedule_occurrences_roles_occupied_check CHECK ((roles_occupied_days IS NULL) = (roles_occupied_secs IS NULL)),
    ADD CONSTRAINT schedule_occurrences_roles_state_check CHECK (state <> 'moved' OR roles_target_days IS NULL);
