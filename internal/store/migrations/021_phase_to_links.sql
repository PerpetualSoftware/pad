-- Migration 021: Move phase relations from fields JSON to item_links table
--
-- Previously, Task→Phase membership was stored as a UUID in
-- json_extract(items.fields, '$.phase'). This migration moves that data into
-- the item_links table with link_type='phase', unifying all item relationships
-- under one system.

-- 1. Migrate existing phase field values to item_links (including archived tasks/phases)
INSERT OR IGNORE INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
SELECT
  lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
  i.workspace_id,
  i.id,
  json_extract(i.fields, '$.phase'),
  'phase',
  COALESCE(i.created_by, 'system'),
  COALESCE(i.updated_at, i.created_at)
FROM items i
JOIN collections c ON c.id = i.collection_id AND c.slug = 'tasks'
WHERE json_extract(i.fields, '$.phase') IS NOT NULL
  AND json_extract(i.fields, '$.phase') != ''
  AND json_extract(i.fields, '$.phase') IN (SELECT id FROM items);

-- 2. Strip the phase key from task fields JSON
UPDATE items SET fields = json_remove(fields, '$.phase')
WHERE collection_id IN (SELECT id FROM collections WHERE slug = 'tasks')
  AND json_extract(fields, '$.phase') IS NOT NULL;

-- 3. Remove the phase field definition from tasks collection schema
UPDATE collections SET schema = json_set(schema, '$.fields',
  (SELECT json_group_array(json(value))
   FROM json_each(json_extract(collections.schema, '$.fields'))
   WHERE json_extract(value, '$.key') != 'phase'))
WHERE slug = 'tasks'
  AND json_extract(schema, '$.fields') LIKE '%"key":"phase"%';
