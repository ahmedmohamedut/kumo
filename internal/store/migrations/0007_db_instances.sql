-- +goose Up
-- Managed Postgres (docs/design/07-databases.md section 3).

-- The VM behind a DB instance is an ordinary instance that the user does not
-- own. '' is a user's own instance; 'rds' is one the databases module created.
--
-- Every instance list and search filters on this, so a managed VM cannot
-- appear on the Instances pages, while capacity accounting, the reconciler and
-- the orphan report go on counting it exactly as before: that is the whole
-- reason the row lives here instead of in a table of its own (R7).
ALTER TABLE instances ADD COLUMN managed_by TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_instances_managed ON instances(managed_by);

CREATE TABLE db_instances (
  id             TEXT PRIMARY KEY,               -- db-<17 hex>
  identifier     TEXT NOT NULL,                  -- user-chosen, immutable; seeds the endpoint name
  state          TEXT NOT NULL CHECK (state IN
                  ('creating','available','starting','stopping','stopped',
                   'rebooting','deleting','failed')),
  state_reason   TEXT,
  engine         TEXT NOT NULL CHECK (engine IN ('postgres')),
  engine_version TEXT NOT NULL,                  -- '18'
  image_id       TEXT NOT NULL REFERENCES images(id),
  -- The hidden VM. NOT NULL with no cascade on purpose: a DB instance without
  -- its instance is not a state worth representing, and the delete flow
  -- removes the two rows in one transaction, in that order.
  instance_id    TEXT NOT NULL REFERENCES instances(id),
  instance_type  TEXT NOT NULL REFERENCES instance_types(id),
  storage_gb     INTEGER NOT NULL,               -- the data disk, not the root disk
  port           INTEGER NOT NULL DEFAULT 5432,
  admin_username TEXT NOT NULL,                  -- the role the create request named
  database_name  TEXT NOT NULL,                  -- the database created and owned by it
  endpoint       TEXT,                           -- <identifier>.<vpc name>, written once it has an address
  created_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
-- No password column and no password hash column. Kumo computes a
-- SCRAM-SHA-256 verifier from the request and discards the password (R8), so a
-- db_instances row cannot be used to authenticate to its own database. That is
-- the point, and it is why there is no password reset in this version.
CREATE UNIQUE INDEX idx_db_instances_identifier ON db_instances(identifier);
CREATE UNIQUE INDEX idx_db_instances_instance ON db_instances(instance_id);

-- +goose Down
DROP TABLE db_instances;
DROP INDEX idx_instances_managed;
ALTER TABLE instances DROP COLUMN managed_by;
