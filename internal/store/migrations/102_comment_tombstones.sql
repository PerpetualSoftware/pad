-- Migration 102: comment tombstones (BUG-3252).
--
-- Deleting a comment that still has replies leaves a tombstone instead of
-- refusing: the row keeps its id (so the replies keep their parent), author and
-- timestamps, and its body is blanked. deleted_at marks it. A comment with no
-- replies is still hard-deleted, and a tombstone is hard-deleted in the same
-- transaction that deletes its last reply.
ALTER TABLE comments ADD COLUMN deleted_at TEXT;
