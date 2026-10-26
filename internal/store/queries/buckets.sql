-- Kumo's metadata of the object storage buckets (docs/design/02-data-model.md).
-- The list of buckets itself is read from RustFS; these rows are joined to it.

-- name: ListBucketRows :many
SELECT b.name, b.state, b.created_by, u.username AS created_by_username, b.created_at, b.updated_at
FROM buckets b
LEFT JOIN users u ON u.id = b.created_by
ORDER BY b.name;

-- name: GetBucketRow :one
SELECT b.name, b.state, b.created_by, u.username AS created_by_username, b.created_at, b.updated_at
FROM buckets b
LEFT JOIN users u ON u.id = b.created_by
WHERE b.name = ?1;

-- name: CreateBucketRow :exec
INSERT INTO buckets (name, state, created_by, created_at, updated_at)
VALUES (?1, 'available', ?2, ?3, ?3);

-- Adopts a bucket made outside Kumo the first time it is changed through Kumo,
-- so its tags have a row to belong to. An existing row keeps its creator.
-- name: EnsureBucketRow :exec
INSERT INTO buckets (name, state, created_by, created_at, updated_at)
VALUES (?1, 'available', NULL, ?2, ?2)
ON CONFLICT (name) DO UPDATE SET updated_at = excluded.updated_at;

-- name: MarkBucketDeleting :exec
UPDATE buckets SET state = 'deleting', updated_at = ?2 WHERE name = ?1;

-- name: DeleteBucketRow :exec
DELETE FROM buckets WHERE name = ?1;
