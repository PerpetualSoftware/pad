-- Migration 113: which app install made a write (SPEC-6 U2a, TASK-3390).
--
-- via_app names the install a row was written through, on every table an app
-- write touches or that consolidates writers: items, comments, activities,
-- item_versions and item_links. It is part of the WRITER IDENTITY where writes
-- are merged (the item-version throttle and the activity debounce), so two
-- installs, or an install and a human, never fold into one version or one
-- activity. items.created_via_app is the install that CREATED the item; app
-- updates are allowed only on items whose created_via_app is that install. It
-- is set once and never changes.
--
-- Both point at app_installs, whose rows are never deleted (an uninstalled
-- install stays as a tombstone), so attribution survives uninstall. ON DELETE
-- SET NULL covers only the workspace purge that removes everything anyway.
-- The values come from the server's resolved install, never from a request.
ALTER TABLE items ADD COLUMN via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE items ADD COLUMN created_via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE comments ADD COLUMN via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE activities ADD COLUMN via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE item_versions ADD COLUMN via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE item_links ADD COLUMN via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_items_created_via_app ON items (created_via_app) WHERE created_via_app IS NOT NULL;
