-- Legacy rows are converted by the Go hook in this migration transaction.
ALTER TABLE item_reminders ALTER COLUMN schedule_id SET NOT NULL;
