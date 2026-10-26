-- The presets are seeded once by migration 0003, not here: an administrator
-- manages the set, so a boot-time seed would bring back a deleted type.

-- Ordered smallest first, which is the order a picker wants. A plain ORDER BY
-- id would sort kumo.2xlarge before kumo.large.
-- name: ListInstanceTypes :many
SELECT id, vcpus, memory_mib, description
FROM instance_types
ORDER BY memory_mib ASC, vcpus ASC, id ASC;

-- name: GetInstanceType :one
SELECT id, vcpus, memory_mib, description FROM instance_types WHERE id = ?1;

-- name: CreateInstanceType :one
INSERT INTO instance_types (id, vcpus, memory_mib, description)
VALUES (?1, ?2, ?3, ?4)
RETURNING id, vcpus, memory_mib, description;

-- name: UpdateInstanceType :one
UPDATE instance_types
SET vcpus = ?2, memory_mib = ?3, description = ?4
WHERE id = ?1
RETURNING id, vcpus, memory_mib, description;

-- name: DeleteInstanceType :execrows
DELETE FROM instance_types WHERE id = ?1;

-- Every row counts, terminated ones included: instances.instance_type is a
-- foreign key with no ON DELETE clause, so any reference blocks the delete.
-- name: ListInstancesUsingInstanceType :many
SELECT id FROM instances
WHERE instance_type = ?1
ORDER BY launched_at DESC, id DESC;
