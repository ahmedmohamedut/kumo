-- Subnets (docs/design/02-data-model.md, subnets table): allocation partitions
-- of their parent VPC's bridge.

-- name: CreateSubnet :one
INSERT INTO subnets (id, vpc_id, name, cidr, is_default, created_by, created_at, updated_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
RETURNING *;

-- name: GetSubnet :one
SELECT * FROM subnets WHERE id = ?1;

-- Detail adds the number of allocated addresses. The available count is
-- usable_ips - allocated_ips, which the service computes from the two.
-- name: GetSubnetDetail :one
SELECT s.*,
  (SELECT COUNT(*) FROM network_interfaces WHERE subnet_id = s.id) AS allocated_ips
FROM subnets s
WHERE s.id = ?1;

-- Keyset pagination with an optional vpc_id filter (docs/design/01-api.md,
-- Subnets). instr(haystack, '') is 1 in SQLite, so an empty query matches
-- every row, and an empty vpc_id disables the filter, the way images.sql treats
-- an empty state filter.
-- name: ListSubnetsDesc :many
SELECT * FROM subnets
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(vpc_id) AS TEXT) = '' OR vpc_id = CAST(sqlc.arg(vpc_id) AS TEXT))
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at < CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id < sqlc.arg(cursor_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListSubnetsAsc :many
SELECT * FROM subnets
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(vpc_id) AS TEXT) = '' OR vpc_id = CAST(sqlc.arg(vpc_id) AS TEXT))
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at > CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id > sqlc.arg(cursor_id)))
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(page_limit);

-- The sibling-overlap check of Create needs every subnet of a VPC.
-- name: ListSubnetsForVPC :many
SELECT * FROM subnets WHERE vpc_id = ?1;

-- A tags-only PATCH bumps updated_at; the immutable attributes never move.
-- name: TouchSubnet :one
UPDATE subnets SET updated_at = ?2 WHERE id = ?1 RETURNING *;

-- name: DeleteSubnet :execrows
DELETE FROM subnets WHERE id = ?1;