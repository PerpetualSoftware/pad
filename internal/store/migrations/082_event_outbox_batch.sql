-- Migration 082: event_outbox.batch_id (TASK-2714, SPEC-3 v1.5).
--
-- The correlation key for handler-path bulk mutations. A lane-wide bulk action
-- is a HANDLER LOOP over per-item store mutations — there is no bulk
-- transaction — so each member writes its own canonical outbox row from its
-- own transaction. That is what keeps SPEC-3's per-member binding evaluation
-- free. It also means that without a marker, the drain would put 200
-- item.deleted events on the webhook wire for a 200-item lane archive, which
-- is precisely the flood TASK-1668's batch event exists to prevent.
--
-- So the handler stamps one id across every row of one bulk operation, and the
-- drain folds a batch into ONE wire item.bulk_updated while the member rows
-- stay individually addressable for bindings.
--
-- RECORDED, NEVER INFERRED (SPEC-3 v1.5). The alternative was grouping pending
-- rows by workspace and a time window, which needs no schema — and would fold
-- two unrelated single updates into somebody's bulk event whenever they landed
-- in the same tick. A wire event that says "these five items changed together"
-- is only true if something recorded that they did.
--
-- Nullable, and NULL is the overwhelmingly common case: every single-item
-- mutation leaves it unset and delivers individually. No foreign key, matching
-- the rest of this table — a batch is not a row anywhere, it is a name the
-- handler minted for one operation.
ALTER TABLE event_outbox ADD COLUMN batch_id TEXT;

-- The drain's grouping index. Partial on the pending set, since a dispatched
-- batch is never re-grouped, and NULL batch_ids are excluded because the
-- single-item path never looks them up by batch.
CREATE INDEX IF NOT EXISTS idx_event_outbox_batch
    ON event_outbox(batch_id)
    WHERE dispatched_at IS NULL AND batch_id IS NOT NULL;
