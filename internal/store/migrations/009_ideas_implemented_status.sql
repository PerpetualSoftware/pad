UPDATE collections
SET schema = json_replace(
    schema,
    '$.fields[0].options',
    json('["new","exploring","planned","implemented","rejected"]')
)
WHERE slug = 'ideas'
  AND json_extract(schema, '$.fields[0].key') = 'status'
  AND json_extract(schema, '$.fields[0].options') NOT LIKE '%implemented%';
