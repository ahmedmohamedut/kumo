-- name: CreateInstance :one
INSERT INTO instances (
  id, name, state, state_reason, image_id, instance_type,
  vcpus, memory_mib, root_disk_gb,
  key_pair_id, key_pair_name, user_data, libvirt_uuid, mac_address, private_ip,
  created_by, launched_at, updated_at, managed_by
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6,
  ?7, ?8, ?9,
  ?10, ?11, ?12, NULL, ?13, ?14,
  ?15, ?16, ?17, ?18
)
RETURNING *;

-- name: GetInstance :one
SELECT * FROM instances WHERE id = ?1;

-- name: GetInstanceByName :one
SELECT * FROM instances WHERE name = ?1;

-- The launch workflow moves an instance between states, so state and reason are
-- always written together: a state without a reason is how an instance ends up
-- 'error' with nothing to say about it.
-- name: SetInstanceState :one
UPDATE instances
SET state = ?2,
    state_reason = ?3,
    updated_at = ?4
WHERE id = ?1
RETURNING *;

-- The successful end of a launch. The runtime columns are written with the state
-- because an instance is not running until the hypervisor says it is, and the
-- MAC and IP come from the same call.
-- name: MarkInstanceRunning :one
UPDATE instances
SET state = 'running',
    state_reason = NULL,
    libvirt_uuid = ?2,
    mac_address = ?3,
    private_ip = ?4,
    updated_at = ?5
WHERE id = ?1
RETURNING *;

-- The IP arrives after the domain is running, when the DHCP lease appears. A
-- separate query keeps the reconciler able to refresh it without touching any
-- other column.
-- name: SetInstanceAddress :one
UPDATE instances
SET private_ip = ?2,
    updated_at = ?3
WHERE id = ?1
RETURNING *;

-- PATCH /instances/{id}. The name is the only column an operator may change:
-- everything else on the row is either a launch input (a vCPU count, a disk
-- size) or runtime data libvirt owns. The partial unique index
-- idx_instances_name_live refuses a duplicate name and the caller maps the
-- violation onto 409.
-- name: RenameInstance :one
UPDATE instances
SET name = ?2,
    updated_at = ?3
WHERE id = ?1
RETURNING *;

-- Keyset pagination over (launched_at, id); instr(haystack, '') is 1 in
-- SQLite, so an empty query matches every row, and an empty state_filter
-- matches every state.
-- include_managed is a flag rather than a value to match, because the value a
-- user-facing list wants to match is the empty string, which is also what the
-- "no filter" idiom of state_filter means. Zero hides the VMs the databases
-- module owns, which is what every list a console user can reach passes; the
-- module itself reads db_instances and never this (07-databases.md section 3).
-- name: ListInstancesDesc :many
SELECT * FROM instances
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(include_managed) AS INTEGER) <> 0 OR managed_by = '')
  AND (CAST(sqlc.arg(state_filter) AS TEXT) = '' OR state = CAST(sqlc.arg(state_filter) AS TEXT))
  AND (CAST(sqlc.arg(cursor_launched) AS TEXT) = ''
       OR launched_at < CAST(sqlc.arg(cursor_launched) AS TEXT)
       OR (launched_at = CAST(sqlc.arg(cursor_launched) AS TEXT) AND id < sqlc.arg(cursor_id)))
ORDER BY launched_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListInstancesAsc :many
SELECT * FROM instances
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(include_managed) AS INTEGER) <> 0 OR managed_by = '')
  AND (CAST(sqlc.arg(state_filter) AS TEXT) = '' OR state = CAST(sqlc.arg(state_filter) AS TEXT))
  AND (CAST(sqlc.arg(cursor_launched) AS TEXT) = ''
       OR launched_at > CAST(sqlc.arg(cursor_launched) AS TEXT)
       OR (launched_at = CAST(sqlc.arg(cursor_launched) AS TEXT) AND id > sqlc.arg(cursor_id)))
ORDER BY launched_at ASC, id ASC
LIMIT sqlc.arg(page_limit);

-- The image name is denormalised into the instance projection for list views, so
-- a page of instances needs the names of the images behind it. One query per page
-- beats one per instance.
-- name: ListImageNames :many
SELECT id, name FROM images WHERE id IN (sqlc.slice('ids'));

-- task_id in the instance projection is the task of the most recent state change,
-- which is how the console finds out why an instance is still pending. It is
-- keyed on resource_id rather than on an instance, so the databases module
-- reads a DB instance's own tasks through it too (07-databases.md section 8).
-- name: GetLatestInstanceTask :one
SELECT * FROM tasks
WHERE resource_id = ?1
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- The end of a terminate: the row goes once the domain, its volumes and its dhcp
-- host are gone, taking its network_interfaces rows with it through the cascade;
-- the tags go in the same transaction. That releases the name and the capacity
-- the row held (docs/design/02-data-model.md). The launch workflow never calls
-- it: a failed instance keeps its row in 'error' so the console can show what
-- happened and a retry has something to work on.
-- name: DeleteInstance :execrows
DELETE FROM instances WHERE id = ?1;

-- The reconciler's input: every instance the control plane knows, so one pass
-- can be matched against one ListDomains call (docs/design/00-architecture.md
-- section 4). It is deliberately unpaginated and unfiltered: which rows a pass
-- may touch is a rule of the reconciler, not of SQL, and a node holds few
-- enough instances that a page would only let one drift unobserved.
-- Deliberately unfiltered: libvirt is the truth for runtime state whoever
-- created the domain, and a managed VM that drifted is the one nobody is
-- watching by hand (07-databases.md section 6).
-- name: ListInstancesForReconcile :many
SELECT * FROM instances ORDER BY id;
