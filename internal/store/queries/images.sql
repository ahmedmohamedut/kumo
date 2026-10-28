-- name: CreateImage :one
INSERT INTO images (
  id, name, description, os_family, os_version, architecture, default_user,
  format, state, state_reason, progress, source_url, checksum_algo, checksum_value,
  size_bytes, virtual_size_bytes, min_disk_gb, volume_name, created_by, created_at, updated_at,
  kind, engine, engine_version
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6, ?7,
  ?8, ?9, ?10, ?11, ?12, ?13, ?14,
  ?15, ?16, ?17, ?18, ?19, ?20, ?21,
  ?22, ?23, ?24
)
RETURNING *;

-- name: GetImage :one
SELECT * FROM images WHERE id = ?1;

-- name: GetImageByName :one
SELECT * FROM images WHERE name = ?1;

-- PATCH /images/{id}: only the fields a user may change. The import columns
-- (source_url, checksum, format, sizes) are deliberately not reachable here,
-- so a rename cannot rewrite where an image came from.
-- name: UpdateImageMetadata :one
UPDATE images
SET name = ?2,
    description = ?3,
    updated_at = ?4
WHERE id = ?1
RETURNING *;

-- The import workflow reports progress while it downloads. State and progress
-- are always written together, because the download is the only thing that
-- moves an image out of pending.
-- name: UpdateImageProgress :one
UPDATE images
SET state = ?2,
    state_reason = ?3,
    progress = ?4,
    updated_at = ?5
WHERE id = ?1
RETURNING *;

-- The import workflow hands the finished volume to the database.
-- name: FinalizeImage :one
UPDATE images
SET state = 'available',
    state_reason = NULL,
    progress = 100,
    format = ?2,
    size_bytes = ?3,
    virtual_size_bytes = ?4,
    volume_name = ?5,
    updated_at = ?6
WHERE id = ?1
RETURNING *;

-- name: FailImage :one
UPDATE images
SET state = 'failed',
    state_reason = ?2,
    updated_at = ?3
WHERE id = ?1
RETURNING *;

-- name: MarkImageDeleting :one
UPDATE images
SET state = 'deleting',
    updated_at = ?2
WHERE id = ?1
RETURNING *;

-- A failed import may be retried from the console by moving it back to pending;
-- only the state fields are reset, never the size columns.
-- name: ResetImageToPending :one
UPDATE images
SET state = 'pending',
    state_reason = NULL,
    progress = 0,
    updated_at = ?2
WHERE id = ?1
RETURNING *;

-- Keyset pagination over (created_at, id). instr(haystack, '') is 1 in SQLite,
-- so an empty query matches every row, and an empty state_filter matches every
-- state. The argument is not called "state" because that is a column name, and
-- sqlc would type the comparison as untyped.
--
-- kind_filter follows the same idiom, but the service never leaves it empty for
-- a list anyone can reach: an engine image must not appear on the Images pages
-- (docs/design/07-databases.md section 3).
-- name: ListImagesDesc :many
SELECT * FROM images
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(state_filter) AS TEXT) = '' OR state = CAST(sqlc.arg(state_filter) AS TEXT))
  AND (CAST(sqlc.arg(kind_filter) AS TEXT) = '' OR kind = CAST(sqlc.arg(kind_filter) AS TEXT))
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at < CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id < sqlc.arg(cursor_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListImagesAsc :many
SELECT * FROM images
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(state_filter) AS TEXT) = '' OR state = CAST(sqlc.arg(state_filter) AS TEXT))
  AND (CAST(sqlc.arg(kind_filter) AS TEXT) = '' OR kind = CAST(sqlc.arg(kind_filter) AS TEXT))
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at > CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id > sqlc.arg(cursor_id)))
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(page_limit);

-- name: DeleteImage :execrows
DELETE FROM images WHERE id = ?1;

-- DELETE /images/{id} is refused while any instance still references the image.
-- For a running one the volume is its backing file; for a terminated one the
-- foreign key would block the row delete, which would leave an image in
-- 'deleting' with its volume already gone. Refusing both keeps the volume and
-- the metadata in step (docs/design/02-data-model.md, images).
-- name: CountInstancesUsingImage :one
SELECT COUNT(*) FROM instances
WHERE image_id = ?1;

-- name: ListInstancesUsingImage :many
SELECT id, state FROM instances
WHERE image_id = ?1
ORDER BY CASE state WHEN 'terminated' THEN 1 ELSE 0 END, launched_at DESC, id DESC;

-- The engines a node can create a DB instance from: every available engine
-- image, newest first. /system/info reports the set, and a create request
-- resolves its engine and version against it (07-databases.md section 2).
-- Only 'available' images count: an engine image still downloading cannot boot
-- anything, so reporting it would offer an engine that is not there yet.
-- name: ListAvailableEngineImages :many
SELECT id, engine, engine_version, name, min_disk_gb, created_at FROM images
WHERE kind = 'engine'
  AND state = 'available'
  AND engine IS NOT NULL
  AND engine_version IS NOT NULL
ORDER BY engine ASC, engine_version DESC, created_at DESC, id DESC;

-- The newest available image for one engine and version, which is what a
-- create request boots from. Newest, because a patched engine image is
-- imported alongside the old one and only new DB instances get it
-- (07-databases.md section 7).
-- name: GetAvailableEngineImage :one
SELECT * FROM images
WHERE kind = 'engine'
  AND state = 'available'
  AND engine = ?1
  AND engine_version = ?2
ORDER BY created_at DESC, id DESC
LIMIT 1;
