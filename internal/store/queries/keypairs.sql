-- name: CreateKeyPair :one
INSERT INTO key_pairs (id, name, type, public_key, fingerprint, imported, created_by, created_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
RETURNING id, name, type, public_key, fingerprint, imported, created_by, created_at;

-- name: GetKeyPair :one
SELECT id, name, type, public_key, fingerprint, imported, created_by, created_at
FROM key_pairs
WHERE id = ?1;

-- Keyset pagination over (created_at, id); instr(haystack, '') is 1 in
-- SQLite, so an empty query matches every row.
-- name: ListKeyPairsDesc :many
SELECT id, name, type, public_key, fingerprint, imported, created_by, created_at
FROM key_pairs
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (sqlc.arg(cursor_created) = ''
       OR created_at < sqlc.arg(cursor_created)
       OR (created_at = sqlc.arg(cursor_created) AND id < sqlc.arg(cursor_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListKeyPairsAsc :many
SELECT id, name, type, public_key, fingerprint, imported, created_by, created_at
FROM key_pairs
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (sqlc.arg(cursor_created) = ''
       OR created_at > sqlc.arg(cursor_created)
       OR (created_at = sqlc.arg(cursor_created) AND id > sqlc.arg(cursor_id)))
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(page_limit);

-- name: DeleteKeyPair :execrows
DELETE FROM key_pairs WHERE id = ?1;

-- name: CountLiveInstancesWithKeyPair :one
SELECT COUNT(*) FROM instances
WHERE key_pair_id = ?1 AND state <> 'terminated';

-- name: ListLiveInstancesWithKeyPair :many
SELECT id FROM instances
WHERE key_pair_id = ?1 AND state <> 'terminated'
ORDER BY launched_at DESC, id DESC;
