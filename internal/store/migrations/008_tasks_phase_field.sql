UPDATE collections
SET schema = json_set(
    schema,
    '$.fields[#]',
    json('{"key":"phase","label":"Phase","type":"relation","collection":"phases"}')
)
WHERE slug = 'tasks'
  AND json_extract(schema, '$.fields') NOT LIKE '%"key":"phase"%';
