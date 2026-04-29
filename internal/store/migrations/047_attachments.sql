-- Migration 047: Attachments table for inline images + file uploads (TASK-869).
--
-- See [[Attachments — architecture & migration design]] (DOC-865) for the full
-- design. This migration is schema groundwork only — no Go consumers yet.
--
-- Key columns:
--   storage_key   "<backend>:<hash>" so the driver registry can route by
--                 prefix. Phase 1 ships fs:; Phase 2 introduces s3:.
--   content_hash  Raw sha256 of the original bytes. Same hash across rows
--                 means the same physical blob is referenced — content-
--                 addressed dedupe is a free side effect.
--   item_id       Nullable. NULL = orphan, eligible for GC after the grace
--                 period expires.
--   parent_id     Non-null on derived blobs (thumbnails) so the GC can
--                 reclaim them when the original goes away.
--   variant       'original' | 'thumb-sm' | 'thumb-md' | NULL.
--   deleted_at    Soft delete; orphan GC reclaims later.

CREATE TABLE IF NOT EXISTS attachments (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    item_id       TEXT,
    uploaded_by   TEXT NOT NULL,
    storage_key   TEXT NOT NULL,
    content_hash  TEXT NOT NULL,
    mime_type     TEXT NOT NULL,
    size_bytes    INTEGER NOT NULL,
    filename      TEXT NOT NULL,
    width         INTEGER,
    height        INTEGER,
    parent_id     TEXT,
    variant       TEXT,
    created_at    TEXT NOT NULL,
    deleted_at    TEXT
);

-- Hot paths:
--   workspace lookup for storage usage SUM and listing
--   item lookup for "what's attached to this item" + GC orphan scan
--   hash lookup for dedupe (full index, not partial — dedupe must see
--     soft-deleted rows so a re-upload of the same bytes can resurrect
--     the existing blob without writing a duplicate)
--   parent lookup so the GC can find a derived blob's siblings
CREATE INDEX IF NOT EXISTS idx_attach_workspace ON attachments(workspace_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_attach_item      ON attachments(item_id)      WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_attach_hash      ON attachments(content_hash);
CREATE INDEX IF NOT EXISTS idx_attach_parent    ON attachments(parent_id)    WHERE parent_id IS NOT NULL;
