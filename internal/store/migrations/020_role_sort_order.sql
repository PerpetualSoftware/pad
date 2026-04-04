-- Add role_sort_order to items for independent ordering in the role board view.
-- Defaults to 0; items without explicit ordering sort by priority then created_at.
ALTER TABLE items ADD COLUMN role_sort_order INTEGER NOT NULL DEFAULT 0;
