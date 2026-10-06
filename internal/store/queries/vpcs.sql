-- VPCs (docs/design/02-data-model.md, vpcs table). One VPC is one libvirt
-- network and one Linux bridge.

-- name: CreateVPC :one
INSERT INTO vpcs (
  id, name, cidr, mode, gateway_ip, netmask, bridge_name, libvirt_network,
  is_default, created_by, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12
)
RETURNING *;

-- name: GetVPC :one
SELECT * FROM vpcs WHERE id = ?1;

-- name: GetVPCByName :one
SELECT * FROM vpcs WHERE name = ?1;

-- name: GetDefaultVPC :one
SELECT * FROM vpcs WHERE is_default = 1 LIMIT 1;

-- Detail adds the counts the console shows next to a VPC: the subnets it
-- contains and the network interfaces (instances) inside those subnets.
-- name: GetVPCDetail :one
SELECT v.*,
  (SELECT COUNT(*) FROM subnets WHERE vpc_id = v.id) AS subnet_count,
  (SELECT COUNT(*) FROM network_interfaces ni
   JOIN subnets s ON s.id = ni.subnet_id
   WHERE s.vpc_id = v.id) AS instance_count
FROM vpcs v
WHERE v.id = ?1;

-- Keyset pagination over (created_at, id), the same shape as every other list.
-- instr(haystack, '') is 1 in SQLite, so an empty query matches every row.
-- name: ListVPCsDesc :many
SELECT * FROM vpcs
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at < CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id < sqlc.arg(cursor_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListVPCsAsc :many
SELECT * FROM vpcs
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at > CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id > sqlc.arg(cursor_id)))
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(page_limit);

-- The subnet listing groups its parents' gateways in one query.
-- name: ListVPCsByIDs :many
SELECT * FROM vpcs WHERE id IN (sqlc.slice('ids'));

-- The CIDR of every VPC, for the no-overlap rule of create and bootstrap.
-- name: ListVPCCIDRs :many
SELECT id, cidr FROM vpcs ORDER BY created_at, id;

-- A tags-only PATCH bumps updated_at; the immutable attributes never move.
-- name: TouchVPC :one
UPDATE vpcs SET updated_at = ?2 WHERE id = ?1 RETURNING *;

-- name: DeleteVPC :execrows
DELETE FROM vpcs WHERE id = ?1;