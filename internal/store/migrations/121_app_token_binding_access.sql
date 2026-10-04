-- Migration 121: the consented access of a delegated app grant (SPEC-6 U5b,
-- TASK-3399).
--
-- A delegated token's access is the lesser of the manifest's
-- delegated.access and what the person chose on the consent page (DOC-3371
-- §4 step 4); the consent page offers read-only even when the manifest asks
-- for write, defaulting to it (lead ruling Q2). The choice is recorded on the
-- binding the issuance barrier writes, under the install-row lock, so every
-- token of the grant carries the same value. NULL on a service binding,
-- whose access is the install's service_access.
--
-- delegated_user_id is the person a delegated grant acts for, written by the
-- barrier at the grant's FIRST persistence, which is its authorization code:
-- the code table has no subject column, so without it a person's unexchanged
-- delegated code could be found by nothing but its session data. Disabling
-- or claiming the account deactivates those codes through it, and erasing
-- the account cascades the binding away (introspection then refuses every
-- token of the grant as unbound). NULL on a service binding.
ALTER TABLE app_token_bindings ADD COLUMN delegated_access TEXT;
ALTER TABLE app_token_bindings ADD COLUMN delegated_user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
--
-- The barrier also remembers, for a delegated grant, what it was issued
-- against, so a change between two of its persistences is never forgotten
-- (codex U5b-1 r1):
--   - delegated_credential_epoch: the person's users.credential_epoch, which a
--     disable or an account claim bumps, so a disable and re-enable between
--     two persistences refuses the next one;
--   - delegated_member_since: the person's workspace_members.created_at, so a
--     removal and re-add between two persistences refuses the next one;
--   - revoked_at: the person's console revoke, a tombstone the barrier refuses
--     instead of a deleted binding it would take for a first issuance and
--     recreate. The sweep removes it with the grant's last row.
ALTER TABLE app_token_bindings ADD COLUMN delegated_credential_epoch INTEGER;
ALTER TABLE app_token_bindings ADD COLUMN delegated_member_since TEXT;
ALTER TABLE app_token_bindings ADD COLUMN revoked_at TEXT;
CREATE INDEX IF NOT EXISTS idx_app_token_bindings_delegated_user ON app_token_bindings (delegated_user_id);
