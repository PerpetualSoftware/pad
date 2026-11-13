SET LOCAL lock_timeout = '5s';
ALTER TABLE schedule_changes DROP CONSTRAINT schedule_changes_change_type_check;
ALTER TABLE schedule_changes ADD CONSTRAINT schedule_changes_change_type_check
    CHECK (change_type IN ('create','update','delete','move','snooze','done','skip','missed_recorded','end','import','carry','ack'));
