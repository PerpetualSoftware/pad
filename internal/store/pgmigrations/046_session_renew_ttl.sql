-- Sliding-session renewal: remember each session's renewal window so the auth
-- middleware can extend expires_at on activity without re-deriving the TTL from
-- device_info (which doesn't reliably encode web vs CLI lifetimes).
-- 0 means "no sliding renewal" — legacy rows created before this migration keep
-- their fixed expiry and naturally age out; new sessions store their TTL here.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS renew_ttl_seconds INTEGER NOT NULL DEFAULT 0;
