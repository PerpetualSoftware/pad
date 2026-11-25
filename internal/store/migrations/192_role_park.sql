-- Per-role park line on agent_roles.
ALTER TABLE agent_roles ADD COLUMN park_mode TEXT NOT NULL DEFAULT 'none' CHECK (park_mode IN ('none','top','line'));
ALTER TABLE agent_roles ADD COLUMN park_sort_order INTEGER CHECK ((park_mode = 'line') = (park_sort_order IS NOT NULL));
ALTER TABLE agent_roles ADD COLUMN park_set_at TEXT;
ALTER TABLE agent_roles ADD COLUMN park_set_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE agent_roles ADD COLUMN park_seq INTEGER NOT NULL DEFAULT 0;
