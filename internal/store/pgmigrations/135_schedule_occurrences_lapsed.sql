SET LOCAL lock_timeout = '5s';
ALTER TABLE schedule_occurrences DROP CONSTRAINT schedule_occurrences_state_check;
ALTER TABLE schedule_occurrences ADD CONSTRAINT schedule_occurrences_state_check
    CHECK (state IN ('done','skipped','moved','missed','lapsed'));
