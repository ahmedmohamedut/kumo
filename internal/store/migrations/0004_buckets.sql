-- +goose Up
-- What Kumo adds to an object storage bucket (docs/design/06-object-storage.md).
-- RustFS is the source of truth for which buckets exist; a bucket created
-- outside Kumo has no row until an administrator tags it. Tags live in the
-- generic tags table under the resource id 'bucket/<name>', because a bucket
-- is named, not numbered, and a bare name could equal another resource's id.
CREATE TABLE buckets (
  name        TEXT PRIMARY KEY,                  -- the S3 bucket name, immutable
  state       TEXT NOT NULL DEFAULT 'available' CHECK (state IN ('available','deleting')),
  created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

-- +goose Down
DELETE FROM tags WHERE resource_id LIKE 'bucket/%';
DROP TABLE buckets;
