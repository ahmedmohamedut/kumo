-- +goose Up
-- Engine images (docs/design/07-databases.md §3). An engine image is an
-- ordinary image that the user does not launch: it carries an installed
-- database engine, and a DB instance boots from it.
--
-- 'user' is an image the Images pages offer; 'engine' is a golden image the
-- databases module boots. Every image list and search filters on kind, so an
-- engine image never appears on the Images pages, while the import task, the
-- checksum, the pool and both size columns go on working unchanged.
ALTER TABLE images ADD COLUMN kind TEXT NOT NULL DEFAULT 'user'
  CHECK (kind IN ('user','engine'));

-- Set on an engine image and NULL otherwise. Together they are how a create
-- request resolves "Postgres 18" to a volume, which is why the engine and its
-- version are columns rather than something inferred from a name.
ALTER TABLE images ADD COLUMN engine TEXT;
ALTER TABLE images ADD COLUMN engine_version TEXT;

-- The lookup POST /rds/db-instances makes: the newest available image for an
-- engine and version.
CREATE INDEX idx_images_engine ON images(kind, engine, engine_version);

-- +goose Down
DROP INDEX idx_images_engine;
ALTER TABLE images DROP COLUMN engine_version;
ALTER TABLE images DROP COLUMN engine;
ALTER TABLE images DROP COLUMN kind;
