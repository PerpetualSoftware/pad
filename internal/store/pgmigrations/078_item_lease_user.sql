-- Migration 078: bind an item execution lease to the user who claimed it
-- (BUG-3341). Mirrors SQLite migration 104; see it for the rationale.
-- NULL for pre-existing leases, which keep refreshing and releasing on the
-- label alone until they expire.
ALTER TABLE items ADD COLUMN IF NOT EXISTS lease_user_id TEXT;
