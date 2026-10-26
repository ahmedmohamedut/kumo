-- +goose Up
-- The instance type presets of docs/design/01-api.md. Until this migration
-- they were seeded on every boot; now that an administrator can create, edit
-- and delete types, a boot-time seed would resurrect a preset somebody
-- deleted, so they are seeded exactly once, here. INSERT OR IGNORE keeps the
-- rows a node already has, edits included.
INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib, description) VALUES
  ('kumo.nano',    1, 512,   '1 vCPU, 512 MiB'),
  ('kumo.micro',   1, 1024,  '1 vCPU, 1 GiB'),
  ('kumo.small',   1, 2048,  '1 vCPU, 2 GiB'),
  ('kumo.medium',  2, 4096,  '2 vCPU, 4 GiB'),
  ('kumo.large',   2, 8192,  '2 vCPU, 8 GiB'),
  ('kumo.xlarge',  4, 16384, '4 vCPU, 16 GiB'),
  ('kumo.2xlarge', 8, 32768, '8 vCPU, 32 GiB');

-- +goose Down
-- Nothing to undo: the presets may be referenced by instances, and a row an
-- administrator edited is no longer the one this migration wrote.
SELECT 1;
