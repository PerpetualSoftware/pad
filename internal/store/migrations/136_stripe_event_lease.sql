-- BUG-3561: a processed-event marker written BEFORE the sidecar's handler
-- ran was final, so a sidecar crash or redeploy mid-handler (or a failed
-- unmark) lost the event for good. The marker now has two states:
-- 'processing' with a lease while a handler runs, 'done' once the sidecar
-- reports success. A retry finding an expired lease takes the event over.
-- Existing rows were written by sidecars that treated the mark as final, so
-- they are 'done'.
ALTER TABLE stripe_processed_events ADD COLUMN state TEXT NOT NULL DEFAULT 'done';
ALTER TABLE stripe_processed_events ADD COLUMN lease_until TEXT;
