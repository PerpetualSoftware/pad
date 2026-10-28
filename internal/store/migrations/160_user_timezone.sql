-- users.timezone: the IANA zone the user wants days computed in (TASK-4101).
-- NULL means never set; readers compute in UTC. Validated by internal/usertz
-- before any write, so it never holds a NUL or an unloadable name.
ALTER TABLE users ADD COLUMN timezone TEXT;
