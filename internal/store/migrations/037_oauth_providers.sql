-- Track which OAuth providers a user has explicitly linked.
-- JSON array, e.g. ["github"] or ["github","google"]. Empty string = no providers.
ALTER TABLE users ADD COLUMN oauth_providers TEXT DEFAULT '';
