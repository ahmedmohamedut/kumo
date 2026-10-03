-- Tags of one resource (docs/design/02-data-model.md, tags table).
-- name: ListResourceTags :many
SELECT resource_id, key, value
FROM tags
WHERE resource_id = ?1
ORDER BY key;

-- name: ListTagsForResources :many
SELECT resource_id, key, value
FROM tags
WHERE resource_id IN (sqlc.slice('resource_ids'))
ORDER BY resource_id, key;

-- name: DeleteResourceTags :exec
DELETE FROM tags WHERE resource_id = ?1;

-- name: SetTag :exec
INSERT INTO tags (resource_id, key, value)
VALUES (?1, ?2, ?3)
ON CONFLICT (resource_id, key) DO UPDATE SET value = excluded.value;
