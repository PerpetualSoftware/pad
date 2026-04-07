-- Migration 003: Rename link_type 'phase' → 'parent'
--
-- Mirrors SQLite migration 023_parent_link_type.sql.
-- Generalizes the phase→task relationship so any item can be a parent of
-- child items with progress tracking, burndown charts, and dependency graphs.
-- The 'parent' link_type means: source_id is a child of target_id.

UPDATE item_links SET link_type = 'parent' WHERE link_type = 'phase';
