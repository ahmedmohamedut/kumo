-- Queries of the managed Postgres module (docs/design/07-databases.md).
--
-- There is no password column and no password hash column to write: the admin
-- credential reaches the guest as a SCRAM verifier in the seed and nothing
-- persists it (section 3). Nothing in this file selects one, because there is
-- nothing to select.

-- The DB instance a managed VM belongs to. There is at most one, by the unique
-- index on instance_id.
-- name: GetDBInstanceByInstance :one
SELECT id, identifier FROM db_instances WHERE instance_id = ?1;

-- name: CreateDBInstance :one
INSERT INTO db_instances (
  id, identifier, state, state_reason, engine, engine_version,
  image_id, instance_id, instance_type, storage_gb, port,
  admin_username, database_name, endpoint, created_by, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, NULL, ?4, ?5,
  ?6, ?7, ?8, ?9, ?10,
  ?11, ?12, NULL, ?13, ?14, ?14
)
RETURNING *;

-- name: GetDBInstance :one
SELECT * FROM db_instances WHERE id = ?1;

-- The state and the reason are written together, so a DB instance cannot end
-- up 'failed' with nothing to say about it.
-- name: SetDBInstanceState :one
UPDATE db_instances
SET state = ?2,
    state_reason = ?3,
    updated_at = ?4
WHERE id = ?1
RETURNING *;

-- The end of a successful create: the endpoint only exists once the guest has
-- an address, so it is written with the state that publishes it
-- (docs/design/07-databases.md section 4 step 7).
-- name: MarkDBInstanceAvailable :one
UPDATE db_instances
SET state = 'available',
    state_reason = NULL,
    endpoint = ?2,
    updated_at = ?3
WHERE id = ?1
RETURNING *;

-- Keyset pagination over (created_at, id), the same shape every other
-- collection uses; instr(haystack, '') is 1 in SQLite, so an empty query
-- matches every row and an empty state_filter matches every state.
-- name: ListDBInstancesDesc :many
SELECT * FROM db_instances
WHERE instr(lower(identifier), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(state_filter) AS TEXT) = '' OR state = CAST(sqlc.arg(state_filter) AS TEXT))
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at < CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id < sqlc.arg(cursor_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListDBInstancesAsc :many
SELECT * FROM db_instances
WHERE instr(lower(identifier), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(state_filter) AS TEXT) = '' OR state = CAST(sqlc.arg(state_filter) AS TEXT))
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at > CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id > sqlc.arg(cursor_id)))
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(page_limit);

-- The vCPUs, the memory and the backing VM's state are read with the DB
-- instance, because they live on the instances row the module owns rather than
-- being snapshotted here (docs/design/07-databases.md section 3). One query
-- per page beats one per row.
-- name: ListDBInstanceServers :many
SELECT d.id AS db_instance_id, i.id AS instance_id, i.state, i.vcpus, i.memory_mib,
       i.private_ip, i.root_disk_gb
FROM db_instances d
JOIN instances i ON i.id = d.instance_id
WHERE d.id IN (sqlc.slice('db_instance_ids'));

-- The reconciler's input: every DB instance with the backing VM's observed
-- state and address, so one pass can decide the transitions of section 6
-- without a read per row. Deliberately unpaginated, like the instance pass.
-- The VPC name comes with it because the pass may be the one that publishes a
-- DB instance, and the endpoint is the identifier under that name (R9). The
-- joins to it are outer: a server whose interface is gone is a DB instance the
-- pass still has to judge.
-- name: ListDBInstancesForReconcile :many
SELECT d.*, i.state AS instance_state, i.private_ip, v.name AS vpc_name
FROM db_instances d
JOIN instances i ON i.id = d.instance_id
LEFT JOIN network_interfaces ni ON ni.instance_id = i.id AND ni.device_index = 0
LEFT JOIN subnets sn ON sn.id = ni.subnet_id
LEFT JOIN vpcs v ON v.id = sn.vpc_id
ORDER BY d.id;

-- The last write of a delete. The db_instances row goes before the instances
-- row it references, which is the order the foreign key demands
-- (docs/design/07-databases.md section 4).
-- name: DeleteDBInstance :execrows
DELETE FROM db_instances WHERE id = ?1;
