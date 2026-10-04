-- The instance type presets of docs/design/01-api.md. They are seeded on every
-- boot with INSERT OR IGNORE, so an operator who edits a preset keeps the edit.
-- The list lives here rather than in Go because it is data the database owns.
-- name: SeedInstanceTypes :exec
INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib, description) VALUES
  ('kumo.nano',    1, 512,   '1 vCPU, 512 MiB'),
  ('kumo.micro',   1, 1024,  '1 vCPU, 1 GiB'),
  ('kumo.small',   1, 2048,  '1 vCPU, 2 GiB'),
  ('kumo.medium',  2, 4096,  '2 vCPU, 4 GiB'),
  ('kumo.large',   2, 8192,  '2 vCPU, 8 GiB'),
  ('kumo.xlarge',  4, 16384, '4 vCPU, 16 GiB'),
  ('kumo.2xlarge', 8, 32768, '8 vCPU, 32 GiB');

-- Ordered smallest first, which is the order a picker wants. A plain ORDER BY
-- id would sort kumo.2xlarge before kumo.large.
-- name: ListInstanceTypes :many
SELECT id, vcpus, memory_mib, description
FROM instance_types
ORDER BY memory_mib ASC, vcpus ASC, id ASC;

-- name: GetInstanceType :one
SELECT id, vcpus, memory_mib, description FROM instance_types WHERE id = ?1;
