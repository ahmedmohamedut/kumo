-- Queries of the managed Postgres module (docs/design/07-databases.md).
-- The service that owns them arrives in M7.5; this one exists because the
-- instance service has to name the DB instance when it refuses a lifecycle
-- action on the VM behind it.

-- The DB instance a managed VM belongs to. There is at most one, by the unique
-- index on instance_id.
-- name: GetDBInstanceByInstance :one
SELECT id, identifier FROM db_instances WHERE instance_id = ?1;
