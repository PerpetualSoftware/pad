-- Migration 120: app upgrades stage like installs (SPEC-6 U8b2, TASK-3397).
--
-- An upgrade reuses the pending-install reservation (the same owner and
-- instance caps, fetch deadline and staged blobs). upgrade_of names the
-- install it would upgrade; NULL is a fresh install. The pending record goes
-- with the install row only if that row is deleted, which never happens (it
-- is the tombstone), so the cascade is for completeness.
ALTER TABLE app_install_pending ADD COLUMN upgrade_of TEXT REFERENCES app_installs(id) ON DELETE CASCADE;
