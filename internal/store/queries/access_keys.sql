-- Object storage access keys (docs/design/02-data-model.md). The secret is
-- never stored.

-- name: ListAccessKeyRows :many
SELECT k.access_key, k.name, k.level, k.created_by, u.username AS created_by_username, k.created_at, k.updated_at
FROM access_keys k
LEFT JOIN users u ON u.id = k.created_by
ORDER BY k.name;

-- name: GetAccessKeyRow :one
SELECT k.access_key, k.name, k.level, k.created_by, u.username AS created_by_username, k.created_at, k.updated_at
FROM access_keys k
LEFT JOIN users u ON u.id = k.created_by
WHERE k.access_key = ?1;

-- name: ListAccessKeyBuckets :many
SELECT access_key, bucket FROM access_key_buckets ORDER BY access_key, bucket;

-- name: ListBucketsOfAccessKey :many
SELECT bucket FROM access_key_buckets WHERE access_key = ?1 ORDER BY bucket;

-- name: CreateAccessKeyRow :exec
INSERT INTO access_keys (access_key, name, level, created_by, created_at, updated_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?5);

-- name: UpdateAccessKeyRow :exec
UPDATE access_keys SET name = ?2, level = ?3, updated_at = ?4 WHERE access_key = ?1;

-- name: AddAccessKeyBucket :exec
INSERT INTO access_key_buckets (access_key, bucket) VALUES (?1, ?2);

-- name: ClearAccessKeyBuckets :exec
DELETE FROM access_key_buckets WHERE access_key = ?1;

-- name: DeleteAccessKeyRow :execrows
DELETE FROM access_keys WHERE access_key = ?1;
