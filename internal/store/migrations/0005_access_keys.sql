-- +goose Up
-- Object storage access keys (docs/design/06-object-storage.md §5). Each is a
-- RustFS user with a policy of its own; this row is what Kumo knows about it.
-- The secret key is never stored: it is returned once, in the create answer.
CREATE TABLE access_keys (
  access_key  TEXT PRIMARY KEY,                  -- KUMO + 16 base32 characters, the S3 access key id
  name        TEXT NOT NULL UNIQUE,              -- a label for the application that holds it
  level       TEXT NOT NULL CHECK (level IN ('read-only','read-write')),
  created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

-- The buckets a key reaches. A bucket is named, not a row of its own, so
-- there is no foreign key: deleting a bucket leaves the grant, which stops
-- mattering until a bucket of that name exists again.
CREATE TABLE access_key_buckets (
  access_key TEXT NOT NULL REFERENCES access_keys(access_key) ON DELETE CASCADE,
  bucket     TEXT NOT NULL,
  PRIMARY KEY (access_key, bucket)
);

-- +goose Down
DROP TABLE access_key_buckets;
DROP TABLE access_keys;
