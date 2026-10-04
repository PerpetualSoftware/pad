-- Migration 122: app-owned webhooks (SPEC-6 U10a, TASK-3408).
--
-- One webhook row per app install (DOC-3371 §5 "App-owned webhooks"), beside
-- the owners' hooks in the same table. app_install_id marks it; NULL is an
-- owner hook. An app hook is the install's, not the owner's: the owner
-- webhook routes, the owner dispatcher and the plan's webhook count all
-- exclude it.
--
-- secret_delivered_at is the HOLD (lead ruling, day 86): an app hook delivers
-- nothing until a redeem has handed its signing secret to the app. Events in
-- the meantime are skipped, not queued, since the app could not verify them.
--
-- For an app hook, events holds the declared subscriptions resolved to
-- companion collection IDs at write time: [{"name": ..., "collection_ids": [...]}].
ALTER TABLE webhooks ADD COLUMN app_install_id TEXT REFERENCES app_installs(id) ON DELETE CASCADE;
ALTER TABLE webhooks ADD COLUMN secret_delivered_at TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhooks_app_install ON webhooks (app_install_id) WHERE app_install_id IS NOT NULL;
