-- Migration 128: which built-in convention or playbook an item was made from
-- (TASK-3462), so a later fix to Pad's text can be OFFERED to it.
--
-- builtin_key is collections.BuiltinEntry.Key (e.g. `playbook/ship`).
-- seed_hash is that entry's BuiltinEntry.Hash when the item was given its
-- text, and seed_content / seed_fields are that text (the body, and the
-- fields blob the item was created with). All three NULL means the version
-- is unknown (an item adopted after the fact). The text is kept, not just
-- its hash, so an item edited since can be shown what the LIBRARY changed
-- (seed -> library) apart from what its user changed (seed -> current).
--
-- A side table rather than columns on items (lead ruling, day 89): only the
-- built-in endpoints read it, and the items row is selected and scanned at a
-- dozen sites that would otherwise all have to carry it.
--
-- Lifecycle: soft delete and restore leave the row alone; a collection move
-- and a workspace transfer keep the item id, so the row stays; a copy (within
-- or across workspaces) is a new, user-made item and gets none; workspace
-- export/import round-trips it; ON DELETE CASCADE plus the purge list remove it
-- with its item.
CREATE TABLE IF NOT EXISTS item_builtin_origin (
    item_id     TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
    builtin_key TEXT NOT NULL,
    seed_hash   TEXT,
    seed_content TEXT,
    seed_fields  TEXT,
    created_at  TEXT NOT NULL
);
