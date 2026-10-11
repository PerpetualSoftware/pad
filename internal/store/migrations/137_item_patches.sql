-- Migration 137: the edits of every content-anchored patch (PLAN-3578 step 3),
-- so `pad item patch --undo <patch-id>` can invert one.
--
-- One row per applied patch, written in the same transaction as the patch's
-- version row. `edits` is a JSON array of {old, new, ctx_before, ctx_after}:
-- what the agent sent, plus up to ~40 characters of the body on each side of
-- each replacement as it stood right after that edit, so an undo can anchor
-- on text even when the edit deleted its `old` (lead ruling, day 91).
--
-- A side table, not activity metadata: debounced activity entries from one
-- writer merge, and a second patch would overwrite the first one's edits.
-- No read surface (lead ruling): only the undo path reads it; it is not
-- exported. Lifecycle: soft delete leaves it; ON DELETE CASCADE plus the
-- workspace purge list remove it with its item.
CREATE TABLE IF NOT EXISTS item_patches (
    id           TEXT PRIMARY KEY,
    item_id      TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    edits        TEXT NOT NULL,
    version_id   TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_item_patches_item ON item_patches(item_id);
