-- name: AllocatedCapacity :one
SELECT
  CAST(COALESCE(SUM(vcpus), 0) AS INTEGER) AS vcpus,
  CAST(COALESCE(SUM(memory_mib), 0) * 1048576 AS INTEGER) AS memory_bytes,
  CAST(COALESCE(SUM(root_disk_gb), 0) * 1073741824 AS INTEGER) AS disk_bytes
FROM instances
WHERE state <> 'terminated';
