-- Add is_diff column to versions table.
-- false (0) = content is full text (legacy), true (1) = content is a diff/patch.
ALTER TABLE versions ADD COLUMN is_diff INTEGER NOT NULL DEFAULT 0;
