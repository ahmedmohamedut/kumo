// Package objectstore is the boundary between the Kumo services and the
// S3-compatible server that holds the object storage data. Services depend on
// the ObjectStore interface only; the RustFS and in-memory fake
// implementations live in sub-packages.
//
// Kumo is the control plane: it creates and lists buckets, browses objects
// and hands out presigned URLs. The object bytes of a console upload or
// download travel between the browser and the server directly, through those
// URLs, and never through kumod. See docs/design/06-object-storage.md.
package objectstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// Sentinel errors. Implementations return them (possibly wrapped) so services
// can map them onto service errors and HTTP statuses in one place.
var (
	// ErrNotFound is returned for an unknown bucket or object.
	ErrNotFound = errors.New("objectstore: not found")
	// ErrAlreadyExists is returned when creating a bucket whose name is taken.
	ErrAlreadyExists = errors.New("objectstore: already exists")
	// ErrBucketNotEmpty is returned when deleting a bucket that holds objects.
	ErrBucketNotEmpty = errors.New("objectstore: bucket not empty")
	// ErrInvalidName is returned for a bucket name or object key the server
	// rejects.
	ErrInvalidName = errors.New("objectstore: invalid name")
	// ErrUnavailable is returned when the server cannot be reached or refuses
	// Kumo's credentials, so a caller can answer 503 rather than 500.
	ErrUnavailable = errors.New("objectstore: unavailable")
)

// MaxListLimit is the most keys one ListObjects call returns, the S3 page size.
const MaxListLimit = 1000

// ObjectStore is the set of operations Kumo needs from an S3-compatible server.
// Implementations must be safe for concurrent use.
type ObjectStore interface {
	// Ping reports whether the server is reachable with Kumo's credentials.
	Ping(ctx context.Context) error

	// Buckets.
	ListBuckets(ctx context.Context) ([]Bucket, error)
	// HeadBucket returns ErrNotFound for a bucket that does not exist.
	HeadBucket(ctx context.Context, name string) (Bucket, error)
	// CreateBucket creates a bucket; a taken name is ErrAlreadyExists, even
	// when Kumo's own credentials own it.
	CreateBucket(ctx context.Context, name string) error
	// DeleteBucket removes an empty bucket; ErrBucketNotEmpty otherwise. A
	// bucket that has ever been versioned is empty only once its old versions
	// and delete markers are gone too, which is what EmptyBucket does.
	DeleteBucket(ctx context.Context, name string) error
	// EmptyBucket deletes every object, every old version and every delete
	// marker, and returns how many entries it removed. It is slow on a large
	// bucket, so a service calls it from a task.
	EmptyBucket(ctx context.Context, name string) (int, error)
	GetVersioning(ctx context.Context, bucket string) (Versioning, error)
	// SetVersioning enables or suspends versioning. A bucket that was never
	// versioned cannot go back to VersioningOff, which is the S3 rule.
	SetVersioning(ctx context.Context, bucket string, enabled bool) error
	// SetCORS sets Kumo's own CORS rule on a bucket, allowing the given
	// origins to GET, PUT and HEAD through a presigned URL. Rules an operator
	// added are kept. An empty list removes Kumo's rule only.
	SetCORS(ctx context.Context, bucket string, origins []string) error

	// Objects.
	ListObjects(ctx context.Context, bucket string, in ListInput) (ObjectPage, error)
	// HeadObject returns ErrNotFound for a key that does not exist.
	HeadObject(ctx context.Context, bucket, key string) (Object, error)
	// PutObject writes a small object through kumod. Console uploads use
	// PresignPut instead; this is for zero-byte folder markers and tests.
	PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error
	// DeleteObjects removes keys; a key that does not exist is not an error.
	DeleteObjects(ctx context.Context, bucket string, keys []string) error

	// Presigned URLs, signed for the address browsers reach the server at.
	PresignGet(ctx context.Context, bucket, key string, in PresignGetInput) (Presigned, error)
	PresignPut(ctx context.Context, bucket, key string, in PresignPutInput) (Presigned, error)
}

// Bucket is one bucket as the server reports it.
type Bucket struct {
	Name      string
	CreatedAt time.Time
}

// Versioning is the versioning state of a bucket.
type Versioning string

// Versioning states, in the vocabulary of the S3 API.
const (
	VersioningOff       Versioning = "off"
	VersioningEnabled   Versioning = "enabled"
	VersioningSuspended Versioning = "suspended"
)

// ListInput pages through one level of a bucket.
type ListInput struct {
	// Prefix limits the listing to keys under it, usually a "folder/" path.
	Prefix string
	// Delimiter groups the keys below the prefix into CommonPrefixes; "/"
	// gives the folder view, "" lists every key under the prefix.
	Delimiter string
	// Cursor is the NextCursor of the previous page, empty for the first.
	Cursor string
	// Limit is the page size, 1..MaxListLimit; 0 means MaxListLimit.
	Limit int
}

// ObjectPage is one page of a listing.
type ObjectPage struct {
	Objects []Object
	// CommonPrefixes are the "folders" one level below the prefix.
	CommonPrefixes []string
	// NextCursor is empty on the last page.
	NextCursor string
}

// Object is one key and its metadata.
type Object struct {
	Key          string
	SizeBytes    int64
	LastModified time.Time
	ETag         string
	ContentType  string
}

// PresignGetInput shapes a download URL.
type PresignGetInput struct {
	TTL time.Duration
	// Filename, when set, makes the browser save the object under this name
	// (Content-Disposition: attachment) instead of displaying it.
	Filename string
}

// PresignPutInput shapes an upload URL.
type PresignPutInput struct {
	TTL time.Duration
	// ContentType is signed into the URL, so the upload must send the same
	// Content-Type header.
	ContentType string
}

// Presigned is a URL a browser can use without Kumo's credentials until it
// expires. Headers are the ones the request must carry for the signature to
// match.
type Presigned struct {
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
}
