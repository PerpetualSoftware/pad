-- Migration 142: an opaque worker id on the item execution lease (TASK-3613).
-- lease_holder is the agent's display name, so two agents with one name on
-- one account shared a lease. A lease claimed with a worker id ("wk_" + 32
-- hex, minted by the client per session) is matched on that id plus
-- lease_user_id; the label becomes display only. NULL for name-only leases
-- and for every lease claimed before this migration, which keep today's
-- label + user rule until they expire (at most 24h).
ALTER TABLE items ADD COLUMN lease_worker_id TEXT;
